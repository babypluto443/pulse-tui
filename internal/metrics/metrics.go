package metrics

import (
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/babypluto/pulse-tui/internal/attack"
)

// Snapshot 不可变汇总——报告层（文本/JSON/TUI）唯一的数据源。
// 所有字段都是 Add 收敛后的拷贝，调用方随意持有、修改，不影响聚合器内部。
type Snapshot struct {
	Total               int64         // 全部完成数（含失败）
	Success             int64         // 2xx
	Failed              int64         // 非 2xx + 传输错误
	Duration            time.Duration // 请求窗口：最后响应完成 − 首请求开始
	RPS                 float64       // Total / Duration.Seconds()（窗口口径）
	LatencyMean         time.Duration
	P50, P90, P99, P999 time.Duration
	StatusCodes         map[int]int64    // 状态码全量分布（含 0=传输失败桶）
	Errors              map[string]int64 // 传输错误按消息分组计数
}

// 状态码数组的特殊桶：0 是真实取值（传输失败无响应），999 之外越界的
// 挤进 overflowBucket——正常 HTTP 状态码 ≤ 999，此桶只是防御性兜底。
const (
	overflowBucket = 999
	codeSpace      = 1000
)

// Aggregator 热路径聚合器。零值不可用，必须 New()。
type Aggregator struct {
	total   atomic.Int64
	success atomic.Int64
	failed  atomic.Int64
	codes   [codeSpace]atomic.Int64

	// latency 与 errors 失败路径才写 errors，共用一把锁足够；
	// 锁竞争分析见包注释（grill-me 决议 #2）
	mu        sync.Mutex
	latencies []time.Duration
	errs      map[string]int64

	// 请求窗口两端：UnixNano 存储，0 表示尚未设置。
	// min/max 更新用 CAS 循环，无锁。
	minStart atomic.Int64
	maxEnd   atomic.Int64
}

// New 创建聚合器。
func New() *Aggregator {
	return &Aggregator{errs: make(map[string]int64)}
}

// Add 记录一个请求结果。热路径：每个请求一次，只在传输失败时才碰 mutex。
func (a *Aggregator) Add(r attack.Result) {
	a.total.Add(1)
	if r.StatusCode >= 200 && r.StatusCode <= 299 {
		a.success.Add(1)
	} else {
		a.failed.Add(1) // 含 3xx/1xx/0——grill-me 决议 #4：非 2xx 全失败
	}
	a.bucket(r.StatusCode).Add(1)

	a.mu.Lock()
	a.latencies = append(a.latencies, r.Latency)
	if r.Error != "" {
		a.errs[r.Error]++
	}
	a.mu.Unlock()

	// 请求窗口两端（CAS 循环：新值更小/更大才换，失败说明被并发抢先，重读再试）
	startNS := r.Timestamp.Add(-r.Latency).UnixNano()
	for {
		old := a.minStart.Load()
		if old != 0 && old <= startNS {
			break
		}
		if a.minStart.CompareAndSwap(old, startNS) {
			break
		}
	}
	endNS := r.Timestamp.UnixNano()
	for {
		old := a.maxEnd.Load()
		if old >= endNS {
			break
		}
		if a.maxEnd.CompareAndSwap(old, endNS) {
			break
		}
	}
}

// bucket 返回状态码对应的原子桶。0 = 传输失败（合法取值），
// 越界（<0 或 ≥1000）挤进溢出桶。
func (a *Aggregator) bucket(code int) *atomic.Int64 {
	if code < 0 || code >= codeSpace {
		return &a.codes[overflowBucket]
	}
	return &a.codes[code]
}

// Snapshot 收敛为不可变汇总。latency 拷贝后排序，内部切片保持
// 纯 append 语义（Snapshot 之后再 Add 也不乱）。
func (a *Aggregator) Snapshot() Snapshot {
	s := Snapshot{
		Total:       a.total.Load(),
		Success:     a.success.Load(),
		Failed:      a.failed.Load(),
		StatusCodes: make(map[int]int64, 8),
		Errors:      make(map[string]int64),
	}
	for c := 0; c < codeSpace; c++ {
		if v := a.codes[c].Load(); v > 0 {
			s.StatusCodes[c] = v
		}
	}

	a.mu.Lock()
	lat := make([]time.Duration, len(a.latencies))
	copy(lat, a.latencies)
	for k, v := range a.errs {
		s.Errors[k] = v
	}
	a.mu.Unlock()

	if len(lat) > 0 {
		sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
		var sum time.Duration
		for _, d := range lat {
			sum += d
		}
		s.LatencyMean = sum / time.Duration(len(lat))
		s.P50 = percentile(lat, 0.50)
		s.P90 = percentile(lat, 0.90)
		s.P99 = percentile(lat, 0.99)
		s.P999 = percentile(lat, 0.999)
	}

	startNS, endNS := a.minStart.Load(), a.maxEnd.Load()
	if startNS != 0 && endNS > startNS {
		s.Duration = time.Duration(endNS - startNS)
		if s.Total > 0 {
			s.RPS = float64(s.Total) / s.Duration.Seconds()
		}
	}
	return s
}

// percentile 升序切片取分位数。下标口径全项目唯一：
// idx = ceil(q*n) - 1（与 cmd/ratetest 对齐），q∈(0,1]。
// 固定这一种并在测试里用已知样本钉死，防止未来“差一位”悄悄漂移。
func percentile(sorted []time.Duration, q float64) time.Duration {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	idx := int(math.Ceil(q*float64(n))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= n {
		idx = n - 1
	}
	return sorted[idx]
}
