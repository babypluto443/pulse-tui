package attack

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/babypluto/pulse-tui/internal/rate"
)

// 必须的调用侧约定：signals 由测试（模拟 main 的角色）创建，
// 容量即 burst；测试结束时它由 rate（此处手动模拟）close。

// TestRateAccuracy —— M2 验收的短跑版（验收原文 10s 由 T2.6 人肉跑）：
// rate=100、2s、瞬时 200 端点 → 完成数 200±5%。
// 服务器零延迟，实测 RPS 只取决于控速器精度 + 工人池是否跟得上。
func TestRateAccuracy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	signals := make(chan struct{}, 50) // burst = c = 50
	go func() {                        // 用真的 rate.Run 发号（集成测试）——别手写 time.Sleep 相对间隔循环，
		// 那是 P1 批判过的误差累积做法，200 次迭代能漂移几十毫秒
		_ = rate.Run(context.Background(), rate.Config{
			Rate:     100,
			Deadline: time.Now().Add(2 * time.Second),
		}, signals)
	}()

	results, err := Run(context.Background(), Config{
		URL: srv.URL, Timeout: time.Second, Workers: 50,
	}, signals)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	total := 0
	for range results {
		total++
	}
	// 2s × 100rps = 200；±5% 容差。低于下限说明工人池没跟上控速器
	if total < 190 || total > 210 {
		t.Errorf("完成数 = %d, 期望 200±5%%（190~210）", total)
	}
	t.Logf("2s 完成 %d 个请求，实测 RPS ≈ %.1f", total, float64(total)/2.0)
}

// TestDrainOnCancel —— 排水语义（grill-me 决议 #3）：
// 300ms 慢端点，中途 cancel → ① 在途请求自然完成且 Error 为空（不被
// 中断成错误尖峰）② 发射停止 ③ results 正常 close，程序不悬挂。
func TestDrainOnCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	signals := make(chan struct{}, 10)

	results, err := Run(ctx, Config{
		URL: srv.URL, Timeout: 2 * time.Second, Workers: 10,
	}, signals)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	go func() { // 模拟 rate.Run：发送与 close 必须同 goroutine 串行，
		// 绝不允许“别的 goroutine 在发、我在 close”（-race 抓过：那是替身自己的 bug）
		defer close(signals)
		for i := 0; i < 100; i++ {
			select {
			case <-ctx.Done():
				return
			case signals <- struct{}{}:
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	time.Sleep(500 * time.Millisecond)
	cancel()

	total, drained := 0, 0
	start := time.Now()
	for r := range results {
		total++
		if r.Error == "" {
			drained++
		}
	}
	elapsed := time.Since(start)

	// 关键断言：所有结果都是传输成功——在途请求没有被中断成错误
	if total != drained {
		t.Errorf("total=%d 但传输成功=%d，存在被取消中断的在途请求", total, drained)
	}
	if total == 0 {
		t.Fatal("一个结果都没有，排水语义失效")
	}
	// 不悬挂：cancel 后最多再等一个在途请求（300ms）就该收工
	if elapsed > 2*time.Second {
		t.Errorf("排水耗时 %v，疑似悬挂", elapsed)
	}
	t.Logf("取消后完成 %d 个在途请求，全部传输成功，排水耗时 %v", total, elapsed)
}

// TestWorkerBounded —— 工人池上界：慢端点下峰值并发永远 ≤ c。
func TestWorkerBounded(t *testing.T) {
	var cur, peak atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		c := cur.Add(1)
		for {
			p := peak.Load()
			if c <= p || peak.CompareAndSwap(p, c) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		cur.Add(-1)
	}))
	defer srv.Close()

	const workers = 4
	signals := make(chan struct{}, workers)
	go func() {
		deadline := time.Now().Add(1 * time.Second)
		for {
			if !time.Now().Add(5 * time.Millisecond).Before(deadline) {
				break
			}
			signals <- struct{}{}
			time.Sleep(5 * time.Millisecond)
		}
		close(signals)
	}()

	results, err := Run(context.Background(), Config{
		URL: srv.URL, Timeout: time.Second, Workers: workers,
	}, signals)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for range results {
	}
	if p := peak.Load(); p > workers {
		t.Errorf("峰值并发 = %d, 超过工人池 %d", p, workers)
	} else {
		t.Logf("峰值并发 = %d（上界 %d）", p, workers)
	}
}

// TestResultFields —— Result 契约口径：POST/header/body 生效；
// 4xx 是合法 StatusCode 而非传输错误；网络错误 StatusCode=0。
func TestResultFields(t *testing.T) {
	t.Run("POST + header + body 真实送达", func(t *testing.T) {
		var mu sync.Mutex
		var gotHeader, gotBody string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			gotHeader, gotBody = r.Header.Get("X-Test"), string(body)
			mu.Unlock()
		}))
		defer srv.Close()

		res := doOne(context.Background(), &http.Client{Timeout: time.Second},
			Config{
				URL:    srv.URL,
				Method: http.MethodPost,
				Header: map[string]string{"X-Test": "hello"},
				Body:   []byte(`{"a":1}`),
			}, http.MethodPost)
		if res.Error != "" || res.StatusCode != http.StatusOK {
			t.Fatalf("请求失败: %d/%q", res.StatusCode, res.Error)
		}
		mu.Lock()
		defer mu.Unlock()
		if gotHeader != "hello" || gotBody != `{"a":1}` {
			t.Errorf("服务端收到 header=%q body=%q, 期望 hello / {\"a\":1}", gotHeader, gotBody)
		}
	})

	t.Run("404 是合法 StatusCode 不是 Error", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		defer srv.Close()
		res := doOne(context.Background(), &http.Client{Timeout: time.Second},
			Config{URL: srv.URL}, http.MethodGet)
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("StatusCode = %d, 期望 404", res.StatusCode)
		}
		if res.Error != "" {
			t.Errorf("4xx 不该写 Error，实际 %q", res.Error)
		}
		if res.Latency <= 0 {
			t.Error("Latency 应为正数")
		}
	})

	t.Run("连接拒绝 StatusCode=0 且 Error 非空", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
		srv.Close() // 立即关闭：端口已无人监听
		res := doOne(context.Background(), &http.Client{Timeout: time.Second},
			Config{URL: srv.URL}, http.MethodGet)
		if res.StatusCode != 0 {
			t.Errorf("StatusCode = %d, 期望 0", res.StatusCode)
		}
		if res.Error == "" {
			t.Error("传输层失败必须写 Error")
		}
	})

	t.Run("超时计入失败不悬空", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(500 * time.Millisecond)
		}))
		defer srv.Close()
		res := doOne(context.Background(), &http.Client{Timeout: 50 * time.Millisecond},
			Config{URL: srv.URL}, http.MethodGet)
		if res.StatusCode != 0 || res.Error == "" {
			t.Errorf("超时应 StatusCode=0 且 Error 非空，实际 %d/%q", res.StatusCode, res.Error)
		}
		if res.Latency < 40*time.Millisecond {
			t.Errorf("超时 Latency = %v，明显不对", res.Latency)
		}
	})
}

// TestInvalidConfig —— 参数校验 fail-fast。
func TestInvalidConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"URL 为空", Config{Timeout: time.Second, Workers: 1}},
		{"Workers 为 0", Config{URL: "http://x", Timeout: time.Second}},
		{"Timeout 为 0", Config{URL: "http://x", Workers: 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Run(context.Background(), tc.cfg, make(chan struct{})); err == nil {
				t.Fatal("期望参数错误，实际 nil")
			}
		})
	}
}

// closedSignals 返回一个已关闭的信号通道（模拟 rate 正常收工后的状态）。
func closedSignals() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}
