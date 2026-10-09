// pulse-tui —— 一个并发架构为核心的 HTTP 压测 CLI（P0 阶段：仅 CLI 骨架）。
//
// 本文件目前只做一件事：用 stdlib flag 解析 SPEC §3 的全部参数并回显，
// 不发任何请求。rate / attack / metrics / report 在后续阶段逐步接上。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/babypluto/pulse-tui/internal/attack"
	"github.com/babypluto/pulse-tui/internal/metrics"
	"github.com/babypluto/pulse-tui/internal/rate"
	"github.com/babypluto/pulse-tui/internal/report"
	"github.com/babypluto/pulse-tui/internal/tui"
	tea "github.com/charmbracelet/bubbletea"
)

// headerList 实现 flag.Value，让 -H 可以重复出现：
//
//	-H "Authorization: Bearer x" -H "X-Trace-Id: abc"
//
// stdlib flag 不直接支持可重复参数，必须自定义类型（PLAN P0 坑点提示）。
type headerList []string

func (h *headerList) String() string {
	return strings.Join(*h, ", ")
}

func (h *headerList) Set(v string) error {
	*h = append(*h, v)
	return nil
}

// config 汇总全部 CLI 参数，字段名与 flag 名一一对应。
// P1/P2 接管时，这坨值将原样传给 rate.Run 与 attack 的工人池。
type config struct {
	rate     float64       // -r：每秒发射请求数（float64，支持 0.5 这类亚赫兹速率）
	duration time.Duration // -d：压测持续时间
	conns    int           // -c：工人池大小（同时在途请求数上限）
	method   string        // -X：HTTP method
	headers  headerList    // -H：自定义 header，可重复
	body     string        // -b：请求 body
	timeout  time.Duration // -timeout：单请求超时
	jsonOut  bool          // -json：JSON 输出报告
	url      string        // 位置参数：目标 URL
}

func main() {
	cfg := parseFlags()

	// 参数回显走 stderr：stdout 是报告专用通道（-json 要进管道被 jq 消费），
	// 诊断信息混进去等于污染 P3 的输出契约
	fmt.Fprintln(os.Stderr, "参数确认")
	fmt.Fprintf(os.Stderr, "  目标 URL     : %s\n", cfg.url)
	fmt.Fprintf(os.Stderr, "  -r  发射速率 : %.2f req/s\n", cfg.rate)
	fmt.Fprintf(os.Stderr, "  -d  持续时间 : %s\n", cfg.duration)
	fmt.Fprintf(os.Stderr, "  -c  工人池   : %d\n", cfg.conns)
	fmt.Fprintf(os.Stderr, "  -X  method   : %s\n", cfg.method)
	fmt.Fprintf(os.Stderr, "  -H  headers  : %s\n", cfg.headers.String())
	fmt.Fprintf(os.Stderr, "  -b  body     : %s\n", cfg.body)
	fmt.Fprintf(os.Stderr, "  -timeout     : %s\n", cfg.timeout)
	fmt.Fprintf(os.Stderr, "  -json        : %v\n", cfg.jsonOut)

	// ---- P2 接线：flag → rate → attack → Ctrl-C 排水 → P3 报告 ----

	// SIGINT → ctx 取消：rate 停发射，attack 停取号，在途请求自然跑完
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// signals channel 由 main 创建，容量即 burst = -c（grill-me 决议 #4）
	signals := make(chan struct{}, cfg.conns)
	rateErr := make(chan error, 1)
	go func() {
		rateErr <- rate.Run(ctx, rate.Config{
			Rate:     cfg.rate,
			Deadline: time.Now().Add(cfg.duration),
			// OnEmit 不挂：P2 阶段统计走 attack.Result，P4 TUI 再启用打点
		}, signals)
	}()

	results, err := attack.Run(ctx, attack.Config{
		URL:     cfg.url,
		Method:  cfg.method,
		Header:  headerMap(cfg.headers),
		Body:    []byte(cfg.body),
		Timeout: cfg.timeout,
		Workers: cfg.conns,
	}, signals)
	if err != nil {
		fmt.Fprintln(os.Stderr, "attack:", err)
		os.Exit(1)
	}

	// ---- P4 接线：引擎后台化 + 三路输出分派 ----
	// 聚合循环移入引擎 goroutine，main 腾出来跑 TUI（决议 #1）

	agg := metrics.New()
	done := make(chan metrics.Snapshot, 1)
	go func() {
		for r := range results {
			agg.Add(r)
		}
		done <- agg.Snapshot()
	}()

	switch {
	case cfg.jsonOut: // 管道场景：跳过 TUI，stdout 只有一份 JSON
		snap := <-done
		if err := report.JSON(snap, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "render json:", err)
			os.Exit(1)
		}
	case isTTY(os.Stdout): // 交互场景：实时 TUI
		p := tea.NewProgram(tui.New(agg, stop), tea.WithAltScreen())
		go func() { // 引擎收工 → 通知 TUI 切报告视图
			p.Send(tui.DoneMsg{Snap: <-done})
		}()
		if _, err := p.Run(); err != nil {
			fmt.Fprintln(os.Stderr, "tui:", err)
			os.Exit(1)
		}
		// 报告已在 TUI 内展示；p.Run 返回时终端状态已由 Bubble Tea 恢复
	default: // 无 TTY 无 -json：管道友好降级，行为与 P3 一致
		snap := <-done
		if err := report.Text(snap, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "render text:", err)
			os.Exit(1)
		}
	}

	// rate 的错误此刻必然已产生（它比 attack 先结束）
	if err := <-rateErr; err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "rate:", err)
	}
}

// isTTY 判断 stdout 是否连着终端：TUI 只在有真终端时启用。
func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// headerMap 把 "-H K: V" 列表转成 map；格式不对的跳过并提示。
func headerMap(headers headerList) map[string]string {
	m := make(map[string]string, len(headers))
	for _, h := range headers {
		k, v, ok := strings.Cut(h, ":")
		if !ok {
			fmt.Fprintf(os.Stderr, "忽略格式错误的 header（应为 \"K: V\"）：%q\n", h)
			continue
		}
		m[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return m
}

func parseFlags() config {
	cfg := config{}

	flag.Float64Var(&cfg.rate, "r", 50, "每秒发射请求数（恒定 RPS 语义，支持小数）")
	flag.DurationVar(&cfg.duration, "d", 10*time.Second, "压测持续时间")
	flag.IntVar(&cfg.conns, "c", 10, "工人池大小（同时在途请求数上限）")
	flag.StringVar(&cfg.method, "X", "GET", "HTTP method")
	flag.Var(&cfg.headers, "H", "自定义 header，可重复（-H \"Authorization: Bearer x\"）")
	flag.StringVar(&cfg.body, "b", "", "请求 body（配合 -X POST 等使用）")
	flag.DurationVar(&cfg.timeout, "timeout", 30*time.Second, "单请求超时")
	flag.BoolVar(&cfg.jsonOut, "json", false, "以 JSON 输出报告")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "用法: pulse-tui [flags] <URL>\n参数:\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	cfg.url = flag.Arg(0)
	cfg.validate() // 参数不合法直接退出，后续阶段拿到的都是干净值
	return cfg
}

// validate 校验参数合法性。P0 阶段就把边界守住，
// 免得 P1/P2 排查时被脏参数干扰（比如 -r 0 会让 n/rate 除零）。
// 用指针接收者：内部还要规整 method 的大小写，必须改到原值上。
func (c *config) validate() {
	fail := func(msg string) {
		fmt.Fprintln(os.Stderr, "参数错误: "+msg)
		os.Exit(2)
	}
	switch {
	case c.url == "":
		fail("缺少目标 URL，用法: pulse-tui [flags] <URL>")
	case c.rate <= 0:
		fail("-r 必须大于 0")
	case c.duration <= 0:
		fail("-d 必须大于 0")
	case c.conns <= 0:
		fail("-c 必须大于 0")
	case c.timeout <= 0:
		fail("-timeout 必须大于 0")
	}
	// method 统一大写，后面拼请求时省一步心智负担
	c.method = strings.ToUpper(c.method)
}
