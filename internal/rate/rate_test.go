package rate

import (
	"context"
	"testing"
	"time"
)

// collectEmit 返回一个挂好 OnEmit 的 Config 副本，把发射时刻收进切片。
// 只在 Run 返回之后读取切片——Run 的返回与最后一次 OnEmit 调用之间
// 有 happens-before（同一 goroutine 顺序执行），无需加锁。
func collectEmit(cfg Config, sink *[]time.Time) Config {
	cfg.OnEmit = func(_ int64, at time.Time) {
		*sink = append(*sink, at)
	}
	return cfg
}

// TestIntervalDistribution —— M1 验收主体：
// rate=100 连发 100 个号，≥90% 相邻间隔 ∈ [8ms, 12ms]（±20%），
// 且总耗时（首号到末号）= 990ms ± 5%。
// 断言走整体分布而非单次硬断言：macOS 调度抖动会让个别间隔越界，
// 测试偶发红比测试松更伤（grill-me 决议 #5）。
func TestIntervalDistribution(t *testing.T) {
	const (
		targetRate = 100.0
		count      = 100
		low        = 8 * time.Millisecond
		high       = 12 * time.Millisecond
	)

	var ts []time.Time
	cfg := Config{
		Rate: targetRate,
		// Deadline = 1005ms：第 100 个号预定时刻 start+1000ms 可发，
		// 第 101 个（1010ms）越线——恰好 100 个。
		// 不能放宽到 2s：那会预定 200 个号，缓冲（100）装不下而
		// 测试端又在等 Run 返回才消费 → 死锁（第一次跑就踩了，exit 137）。
		// 两侧各留 5ms 余量抗调度抖动。
		Deadline: time.Now().Add(1005 * time.Millisecond),
	}
	cfg = collectEmit(cfg, &ts)
	ch := make(chan struct{}, count) // 缓冲装得下全部信号：排除背压干扰，测纯控速精度

	if err := Run(context.Background(), cfg, ch); err != nil {
		t.Fatalf("Run 返回错误: %v", err)
	}
	if len(ts) != count {
		t.Fatalf("发射数量 = %d, 期望 %d", len(ts), count)
	}
	for i := 0; i < count; i++ {
		if _, ok := <-ch; !ok {
			t.Fatalf("第 %d 个信号读取时 channel 已关闭", i)
		}
	}
	if _, ok := <-ch; ok {
		t.Fatal("正常结束后 channel 应已关闭")
	}

	// 相邻间隔分布
	inRange := 0
	var intervals []time.Duration
	for i := 1; i < len(ts); i++ {
		d := ts[i].Sub(ts[i-1])
		intervals = append(intervals, d)
		if d >= low && d <= high {
			inRange++
		}
	}
	ratio := float64(inRange) / float64(len(intervals))
	if ratio < 0.90 {
		t.Errorf("间隔落在 [%v, %v] 的比例 = %.1f%%（%d/%d），低于 90%%\n间隔明细: %v",
			low, high, ratio*100, inRange, len(intervals), intervals)
	}

	// 总耗时：99 个间隔 × 10ms = 990ms，±5%
	total := ts[len(ts)-1].Sub(ts[0])
	want := time.Duration((count - 1) / targetRate * float64(time.Second)) // 990ms
	tolerance := want * 5 / 100
	if total < want-tolerance || total > want+tolerance {
		t.Errorf("总耗时 = %v, 期望 %v ± %v", total, want, tolerance)
	}
	t.Logf("间隔分布: %d/%d 在 [%v,%v]（%.1f%%），总耗时 %v（期望 %v）",
		inRange, len(intervals), low, high, ratio*100, total, want)
}

// TestDeadline 验证截断语义：rate=10、deadline=start+350ms，
// 预定时刻 100/200/300ms 可发（3 个），400ms 越线不发。
// 350ms 离边界 50ms，远超调度抖动，不会偶发红。
func TestDeadline(t *testing.T) {
	var ts []time.Time
	cfg := Config{
		Rate:     10,
		Deadline: time.Now().Add(350 * time.Millisecond),
	}
	cfg = collectEmit(cfg, &ts)
	ch := make(chan struct{}, 10)

	if err := Run(context.Background(), cfg, ch); err != nil {
		t.Fatalf("Run 返回错误: %v", err)
	}
	for range ch { // 排干缓冲，确认 close
	}
	if len(ts) != 3 {
		t.Errorf("发射数量 = %d, 期望 3（预定时刻 100/200/300ms ≤ 350ms，400ms 越线）", len(ts))
	}
}

// TestCancel 分两个子场景验证取消语义（grill-me 决议 #4）：
// 1. 睡眠中取消：ctx.Done 立即打断 timer 等待
// 2. 背压阻塞中取消：ch 满且无人消费，发送阻塞时 ctx.Done 仍能生效
// 两者都应返回 ctx.Err()，且 channel 已被 Run 关闭。
func TestCancel(t *testing.T) {
	t.Run("睡眠中取消", func(t *testing.T) {
		cfg := Config{
			Rate:     1, // 首号预定时刻 start+1s，取消必然发生在睡眠期间
			Deadline: time.Now().Add(10 * time.Second),
		}
		ch := make(chan struct{}) // 无缓冲
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(50 * time.Millisecond)
			cancel()
		}()
		err := Run(ctx, cfg, ch)
		if err != context.Canceled {
			t.Errorf("Run 返回 = %v, 期望 context.Canceled", err)
		}
		if _, ok := <-ch; ok {
			t.Error("取消后 channel 应已关闭")
		}
	})

	t.Run("背压阻塞中取消", func(t *testing.T) {
		cfg := Config{
			Rate:     1,
			Deadline: time.Now().Add(10 * time.Second),
		}
		ch := make(chan struct{}, 1)
		ch <- struct{}{} // 预先填满：首号发送必然阻塞（无人消费）
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			// 首号预定时刻 start+1s：此刻 Run 已睡醒、阻塞在发送上
			// （缓冲被预填占满且无人消费）。1.5s 取消命中的才是
			// 发送阻塞路径；若太早取消，命中的是睡眠路径，测了个寂寞。
			time.Sleep(1500 * time.Millisecond)
			cancel()
		}()
		err := Run(ctx, cfg, ch)
		if err != context.Canceled {
			t.Errorf("Run 返回 = %v, 期望 context.Canceled", err)
		}
		<-ch // 排掉预填信号
		if _, ok := <-ch; ok {
			t.Error("取消后 channel 应已关闭")
		}
	})
}

// TestCloseOnNormalEnd 验证正常到点：返回 nil、range 能自然退出、
// 5 个信号一个不少（deadline=start+110ms，预定时刻 20/40/60/80/100ms
// 可发，120ms 越线；110ms 离两边边界 10ms+，不会偶发红）。
func TestCloseOnNormalEnd(t *testing.T) {
	var ts []time.Time
	cfg := Config{
		Rate:     50,
		Deadline: time.Now().Add(110 * time.Millisecond),
	}
	cfg = collectEmit(cfg, &ts)
	ch := make(chan struct{}, 10)

	if err := Run(context.Background(), cfg, ch); err != nil {
		t.Fatalf("Run 返回错误: %v（期望 nil）", err)
	}
	got := 0
	for range ch {
		got++
	}
	if got != 5 || len(ts) != 5 {
		t.Errorf("range 收到 %d 个信号 / OnEmit 记录 %d 个, 期望 5/5", got, len(ts))
	}
}

// TestInvalidConfig 验证参数校验 fail-fast：不进入循环、channel 仍被 close。
func TestInvalidConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"Rate 为 0", Config{Deadline: time.Now().Add(time.Second)}},
		{"Rate 为负", Config{Rate: -1, Deadline: time.Now().Add(time.Second)}},
		{"Deadline 零值", Config{Rate: 100}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch := make(chan struct{}, 1)
			err := Run(context.Background(), tc.cfg, ch)
			if err == nil {
				t.Fatal("期望返回参数错误，实际 nil")
			}
			if _, ok := <-ch; ok {
				t.Error("错误路径 channel 也应被 close（唯一 close 责任方）")
			}
		})
	}
}
