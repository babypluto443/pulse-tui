// ratetest —— rate 控速器的人肉验收工具（SPEC M1：控速器 + cmd/ratetest）。
//
// 用法：
//
//	ratetest -r 100 -n 100
//
// 逐行打印每个信号的发射时刻与相邻间隔，结尾输出统计
// （min/max/avg/p95），肉眼即可判断间隔是否贴着 1/r。
// 它是独立调试命令，自有 flag，不占用 pulse-tui 主 CLI 的参数契约。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/babypluto/pulse-tui/internal/rate"
)

func main() {
	rps := flag.Float64("r", 100, "每秒发射信号数")
	total := flag.Int("n", 100, "发射信号总数")
	flag.Parse()

	// deadline = n/r + margin。margin 必须小于一个发射间隔 1/r，
	// 否则第 n+1..个号的预定时刻也落在 deadline 内，会多发信号
	// （第一版用固定 1s 余量，rate=100 时等于多放 100 个名额，
	// 缓冲 100 装不下又无人消费 → 死锁，实测 exit 137）。
	// 取半个间隔并夹在 [启动开销, 500ms]：上限防低速时白等，
	// 下限要盖过 main 到 Run 内部 start 之间的启动开销（µs 级）。
	margin := time.Duration(float64(time.Second) / (2 * *rps))
	if margin > 500*time.Millisecond {
		margin = 500 * time.Millisecond
	}
	deadline := time.Now().Add(time.Duration(float64(*total) / *rps * float64(time.Second)).Round(time.Microsecond) + margin)

	// 缓冲装得下全部信号 + 后台排水兜底：即使 margin 意外放进额外
	// 名额，Run 也不会阻塞在发送上——结构性排除死锁。
	ch := make(chan struct{}, *total)
	drained := make(chan struct{})
	go func() {
		for range ch {
		}
		close(drained)
	}()
	var stamps []time.Time

	cfg := rate.Config{
		Rate:     *rps,
		Deadline: deadline,
		OnEmit: func(_ int64, at time.Time) {
			stamps = append(stamps, at)
		},
	}

	// OnEmit 在调度 goroutine 里同步调用，Run 返回后读取 stamps 安全
	err := rate.Run(context.Background(), cfg, ch)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ratetest:", err)
		os.Exit(1)
	}
	<-drained // 等后台排水 goroutine 确认 channel 已排干并关闭

	if len(stamps) < 2 {
		fmt.Fprintln(os.Stderr, "ratetest: 信号数不足 2，无法统计间隔")
		os.Exit(1)
	}

	fmt.Printf("rate=%.4g  发射 %d 个信号\n\n", *rps, len(stamps))
	fmt.Println("  n   时刻(相对 ms)   与上一号间隔")

	var intervals []time.Duration
	for i, at := range stamps {
		rel := at.Sub(stamps[0])
		if i == 0 {
			fmt.Printf("%3d  %10.3f        —\n", i+1, float64(rel)/1e6)
			continue
		}
		d := stamps[i].Sub(stamps[i-1])
		intervals = append(intervals, d)
		fmt.Printf("%3d  %10.3f  %10.3f\n", i+1, float64(rel)/1e6, float64(d)/1e6)
	}

	// 分位数口径：升序取 ceil(q*n)-1（与 P3 metrics 将采用的口径一致，
	// 提前在这里用同一规则，保持项目内统一）
	sort.Slice(intervals, func(i, j int) bool { return intervals[i] < intervals[j] })
	pct := func(q float64) time.Duration {
		idx := int(q*float64(len(intervals))+0.999999) - 1 // ceil(q*n)-1
		if idx < 0 {
			idx = 0
		}
		return intervals[idx]
	}
	var sum time.Duration
	for _, d := range intervals {
		sum += d
	}

	fmt.Printf("\n间隔统计：min=%.3fms  avg=%.3fms  p50=%.3fms  p95=%.3fms  max=%.3fms\n",
		float64(intervals[0])/1e6,
		float64(sum)/float64(len(intervals))/1e6,
		float64(pct(0.50))/1e6,
		float64(pct(0.95))/1e6,
		float64(intervals[len(intervals)-1])/1e6,
	)
	fmt.Printf("目标间隔=%.3fms（1/rate）\n", 1e3 / *rps)
}
