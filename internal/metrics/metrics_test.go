package metrics

import (
	"sync"
	"testing"
	"time"

	"github.com/babypluto/pulse-tui/internal/attack"
)

// res 造一个 Result 的小工具（时间戳按 latency 顺推，保证窗口一致）。
func res(latency time.Duration, code int, errMsg string) attack.Result {
	end := time.Unix(1_000_000, 0)
	return attack.Result{
		Timestamp:  end,
		Latency:    latency,
		StatusCode: code,
		Error:      errMsg,
	}
}

// TestConcurrentAdd —— 并发正确性（跑测试时必须带 -race）：
// 8 goroutine × 1250 个 Result，计数/分布必须分毫不差。
func TestConcurrentAdd(t *testing.T) {
	const (
		goroutines = 8
		perG       = 1250
	)
	agg := New()
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(id int) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				seq := id*perG + i
				switch {
				case seq%100 == 0: // 1% 传输失败
					agg.Add(res(time.Millisecond, 0, "dial tcp: connection refused"))
				case seq%10 == 0: // 10% 服务端 500
					agg.Add(res(2*time.Millisecond, 500, ""))
				default:
					agg.Add(res(time.Millisecond, 200, ""))
				}
			}
		}(g)
	}
	wg.Wait()

	s := agg.Snapshot()
	wantTotal := int64(goroutines * perG)
	// seq%100==0 先命中传输失败分支（switch 顺序），它同时也是 %10 的倍数，
	// 所以 500 桶 = %10 总数 − %100 总数；failed = 传输失败 + 500
	wantTransport := wantTotal / 100
	want500 := wantTotal/10 - wantTransport
	wantFail := wantTransport + want500

	if s.Total != wantTotal {
		t.Errorf("Total = %d, 期望 %d", s.Total, wantTotal)
	}
	if s.Failed != wantFail || s.Success != wantTotal-wantFail {
		t.Errorf("Failed = %d / Success = %d, 期望 %d / %d", s.Failed, s.Success, wantFail, wantTotal-wantFail)
	}
	if s.StatusCodes[0] != wantTransport || s.StatusCodes[500] != want500 || s.StatusCodes[200] != wantTotal-wantFail {
		t.Errorf("状态码分布不对: %v", s.StatusCodes)
	}
	if s.Errors["dial tcp: connection refused"] != wantTotal/100 {
		t.Errorf("errors 分组不对: %v", s.Errors)
	}
	if s.Total != s.Success+s.Failed {
		t.Errorf("不变量破坏: Total != Success + Failed")
	}
}

// TestPercentileFixed —— 用已知样本把分位数口径钉死：
// latencies = 1ms..10ms（10 个，天然升序）。
// idx = ceil(q*n)-1：P50→ceil(5)-1=4→5ms；P90→ceil(9)-1=8→9ms；
// P99→ceil(9.9)-1=9→10ms；P999→ceil(9.99)-1=9→10ms。mean=5.5ms。
func TestPercentileFixed(t *testing.T) {
	agg := New()
	for i := 1; i <= 10; i++ {
		agg.Add(res(time.Duration(i)*time.Millisecond, 200, ""))
	}
	s := agg.Snapshot()

	checks := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"mean", s.LatencyMean, 5500 * time.Microsecond},
		{"p50", s.P50, 5 * time.Millisecond},
		{"p90", s.P90, 9 * time.Millisecond},
		{"p99", s.P99, 10 * time.Millisecond},
		{"p999", s.P999, 10 * time.Millisecond},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, 期望 %v（口径 ceil(q*n)-1 被改动？）", c.name, c.got, c.want)
		}
	}
}

// TestFailureClassification —— 3xx/1xx/0 全归 failed（grill-me 决议 #4）。
func TestFailureClassification(t *testing.T) {
	agg := New()
	agg.Add(res(time.Millisecond, 301, "")) // 裸重定向
	agg.Add(res(time.Millisecond, 100, "")) // 1xx
	agg.Add(res(time.Millisecond, 0, "timeout"))
	agg.Add(res(time.Millisecond, 200, ""))
	agg.Add(res(time.Millisecond, 299, ""))
	s := agg.Snapshot()

	if s.Success != 2 || s.Failed != 3 {
		t.Errorf("Success=%d Failed=%d, 期望 2/3（非 2xx 全失败）", s.Success, s.Failed)
	}
	if s.StatusCodes[301] != 1 || s.StatusCodes[100] != 1 || s.StatusCodes[0] != 1 {
		t.Errorf("状态码分布应保留全量信息: %v", s.StatusCodes)
	}
}

// TestWindow —— 请求窗口口径：最后响应完成 − 首请求开始。
// 三个请求：开始时刻 0ms/10ms/20ms，latency 5ms → 窗口 = 25ms−0ms = 25ms。
func TestWindow(t *testing.T) {
	agg := New()
	base := time.Unix(1_000_000, 0)
	mk := func(startOffset, latency time.Duration) attack.Result {
		return attack.Result{
			Timestamp:  base.Add(startOffset + latency),
			Latency:    latency,
			StatusCode: 200,
		}
	}
	agg.Add(mk(0, 5*time.Millisecond))
	agg.Add(mk(10*time.Millisecond, 5*time.Millisecond))
	agg.Add(mk(20*time.Millisecond, 5*time.Millisecond))

	s := agg.Snapshot()
	if s.Duration != 25*time.Millisecond {
		t.Errorf("窗口 = %v, 期望 25ms（最后响应完成 25ms − 首请求开始 0ms）", s.Duration)
	}
	wantRPS := 3.0 / 0.025
	if diff := s.RPS - wantRPS; diff > 0.01 || diff < -0.01 {
		t.Errorf("RPS = %v, 期望 %v", s.RPS, wantRPS)
	}
}

// TestSnapshotImmutable —— Snapshot 必须是深拷贝：改返回值不影响内部。
func TestSnapshotImmutable(t *testing.T) {
	agg := New()
	agg.Add(res(time.Millisecond, 200, ""))
	s1 := agg.Snapshot()
	s1.StatusCodes[200] = 99999 // 蓄意污染
	s1.Errors["x"] = 1

	s2 := agg.Snapshot()
	if s2.StatusCodes[200] != 1 {
		t.Errorf("内部被 Snapshot 污染: %v", s2.StatusCodes)
	}
	if _, ok := s2.Errors["x"]; ok {
		t.Errorf("errors 被污染: %v", s2.Errors)
	}
}
