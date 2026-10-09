package attack

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// Result 单次请求的原始记录——P3 metrics 的输入契约，字段一次定好不再改
// （grill-me 决议 #2：Result 只当记录员，2xx/4xx/5xx 的分类裁判权在 P3）。
type Result struct {
	// Timestamp 请求完成时刻。开始时刻 = Timestamp - Latency，不冗余存。
	Timestamp time.Time `json:"timestamp"`
	// Latency 从请求写出 → 读完整个响应 body（SPEC §5 口径，对齐 hey/vegeta）。
	Latency time.Duration `json:"latency_ns"`
	// StatusCode HTTP 状态码；网络错误/超时拿不到响应时为 0
	// （0 与任何真实状态码不冲突，这是选 0 而不是 -1 的原因）。
	StatusCode int `json:"status_code"`
	// Error 传输层失败描述（网络错误/超时/请求构建失败），空串 = 传输成功。
	// 注意：4xx/5xx 不写这里——它们有合法 StatusCode，分类由 P3 做。
	Error string `json:"error"`
}

// Config 汇总攻击器参数（与 P1 的 rate.Config 同风格：字段只增不改签名）。
type Config struct {
	URL     string            // 目标 URL，必填
	Method  string            // HTTP method，空 → GET（main 已规整大写，这里兜底）
	Header  map[string]string // 自定义 header
	Body    []byte            // 请求 body（POST 等使用）
	Timeout time.Duration     // 单请求超时，挂到 http.Client.Timeout，必填
	Workers int               // 工人池大小（-c），必填
}

// Run 启动工人池，从 signals 取号、发请求，把 Result 送入返回的只读流。
//
// 生命周期：本函数立即返回；全部工人收工后 close results（唯一 close
// 责任方），调用方用 range 消费天然安全。
//
// 取消语义：ctx 取消（Ctrl-C / Deadline 由 main 组织）→ 停止取号 →
// 在途请求自然跑完（挂 drain ctx，不挂 ctx）→ close results。
func Run(ctx context.Context, cfg Config, signals <-chan struct{}) (<-chan Result, error) {
	// fail-fast：脏配置绝不能进入工人池
	switch {
	case cfg.URL == "":
		return nil, fmt.Errorf("attack: URL 不能为空")
	case cfg.Workers <= 0:
		return nil, fmt.Errorf("attack: Workers 必须大于 0，当前 %d", cfg.Workers)
	case cfg.Timeout <= 0:
		return nil, fmt.Errorf("attack: Timeout 必须大于 0，当前 %v", cfg.Timeout)
	default:
	}
	method := cfg.Method
	if method == "" {
		method = http.MethodGet
	}

	// 全局唯一的 Client（P2 核心思想：绝不一请求一 Client）。
	// 自定义 Transport 不 ForceAttemptHTTP2 → 统一 HTTP/1.1；
	// 连接层上限与工人池对齐：worker 池已限在途 ≤ c，这里是防御性同界，
	// 同时把空闲连接池喂饱（默认 MaxIdleConnsPerHost=2，高 RPS 下必抖）。
	client := &http.Client{
		Timeout: cfg.Timeout,
		Transport: &http.Transport{
			MaxIdleConnsPerHost: cfg.Workers,
			MaxConnsPerHost:     cfg.Workers,
		},
	}

	// drain ctx：在途请求的生命线。只等全部工人完工后才取消——
	// Ctrl-C 时在途请求自然跑完、latency 照常入账（grill-me 决议 #3）。
	// cancel 在 wg.Wait() 之后调用，兼作资源兜底。
	drainCtx, cancel := context.WithCancel(context.Background())

	results := make(chan Result)
	var wg sync.WaitGroup
	wg.Add(cfg.Workers)
	for i := 0; i < cfg.Workers; i++ {
		go func() {
			defer wg.Done()
			work(ctx, drainCtx, client, cfg, method, signals, results)
		}()
	}
	go func() {
		wg.Wait()
		cancel()       // 所有工人收工后解除 drain 资源（兜底防泄漏）
		close(results) // 唯一 close 责任方
	}()
	return results, nil
}

// work 是单个工人的主循环：取号 → 发一个请求 → 送 Result。
func work(ctx, drainCtx context.Context, client *http.Client, cfg Config, method string, signals <-chan struct{}, results chan<- Result) {
	for {
		// 第一级：非阻塞优先检查取消（对 P1 契约的履约点）。
		// 没有这一步，取消时下面的 select 会在“ctx 已就绪”和
		// “缓冲里还有遗留号”之间随机选择，可能把遗留号又执行掉，
		// 破坏“立即停止发射”。
		select {
		case <-ctx.Done():
			return
		default:
		}

		// 第二级：等号或取消。signals 被 rate.Run close 后 ok=false，
		// 正常收工；缓冲遗留号随之丢弃——与取消路径语义一致
		// （“发射”指变成真实请求，没被工人接走的号不算数）。
		select {
		case <-ctx.Done():
			return
		case _, ok := <-signals:
			if !ok {
				return
			}
		}

		results <- doOne(drainCtx, client, cfg, method)
	}
}

// doOne 执行单次请求并打点，全口径对齐 SPEC §5。
func doOne(ctx context.Context, client *http.Client, cfg Config, method string) Result {
	// body 每次新建 Reader：bytes.Reader 有读取游标，复用会从
	// 上次读到的位置继续，第二个请求就成空 body 了
	var body io.Reader
	if len(cfg.Body) > 0 {
		body = bytes.NewReader(cfg.Body)
	}
	req, err := http.NewRequestWithContext(ctx, method, cfg.URL, body)
	if err != nil {
		return Result{Timestamp: time.Now(), Error: "构建请求失败: " + err.Error()}
	}
	for k, v := range cfg.Header {
		req.Header.Set(k, v)
	}

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		// 传输层失败（含超时——Client.Timeout 到点在这里浮出）：
		// StatusCode 保持 0，Error 带原始错误。
		// latency 照记：失败请求的耗时也是真实数据（SPEC §5 口径）
		return Result{Timestamp: time.Now(), Latency: time.Since(start), Error: err.Error()}
	}
	// 必须读完 body（哪怕丢弃）连接才能复用 keep-alive，
	// 否则每个请求都新建 TCP 连接，高 RPS 下 TIME_WAIT 爆炸
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	return Result{
		Timestamp:  time.Now(),
		Latency:    time.Since(start),
		StatusCode: resp.StatusCode,
	}
}
