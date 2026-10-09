package rate

import (
	"context"
	"fmt"
	"time"
)

// Config 汇总控速器的全部参数。字段只增不改签名（grill-me 决议 #1）。
type Config struct {
	// Rate 每秒发射信号数，必须 > 0。支持亚赫兹速率（如 0.5 = 每 2 秒发 1 个）。
	Rate float64

	// Deadline 最后一个可发射号的预定时刻上限（绝对时间）。
	// 循环判定：next = start + n/Rate，next.After(Deadline) 即停止。
	// 零值视为配置错误，直接返回 error。
	Deadline time.Time

	// OnEmit 可空回调：每个信号成功送入 ch 的瞬间，以 (序号, 当前时刻)
	// 调用。M1 验收与 cmd/ratetest 靠它拿权威发射时刻；nil 时零开销。
	// 注意：回调在调度 goroutine 内同步执行，写慢会拖累发射节奏，
	// 只做轻量记录（append 到切片）。
	OnEmit func(n int64, at time.Time)
}

// Run 按恒定 RPS 向 ch 发射信号，直到 Deadline 越线或 ctx 取消。
//
// 调用方约定：
//   - ch 必须非 nil，缓冲容量即 burst（容量决定背压时机，由调用方定）；
//   - 不要 close ch，Run 是唯一 close 责任方（任何退出路径都 close）；
//   - 返回值：参数错误返回 error；Deadline 到点自然结束返回 nil；
//     ctx 取消返回 ctx.Err()。
func Run(ctx context.Context, cfg Config, ch chan<- struct{}) error {
	// ch 的 nil 检查必须最先做：nil channel 无从 close（调用方手里
	// 根本没有 channel），且下面的 defer close 对 nil 会 panic。
	if ch == nil {
		return fmt.Errorf("rate: ch 不能为 nil")
	}
	// 唯一 close 责任方：无论 return 从哪条路径出去（含参数错误），
	// close 都保证执行——所以 close 的 defer 必须注册在所有可能
	// return 的校验之前。调用方用 range 消费时因此天然安全。
	defer close(ch)

	// 参数校验，fail-fast：脏参数绝不能进入调度循环
	switch {
	case cfg.Rate <= 0:
		return fmt.Errorf("rate: Rate 必须大于 0，当前 %v", cfg.Rate)
	case cfg.Deadline.IsZero():
		return fmt.Errorf("rate: Deadline 未设置（零值时间）")
	}

	// 绝对时间表的基准点。之后每个号的预定时刻只由 start 和 n 决定，
	// 与“上一次实际何时发出”无关——这就是误差不累积的关键。
	start := time.Now()

	// 复用单个 Timer 而不是每次 time.After：time.After 每轮在堆上
	// 分配新 Timer 且到期前无法 GC，高 RPS 长压测下是纯浪费。
	timer := time.NewTimer(0)
	if !timer.Stop() {
		<-timer.C // 排空 NewTimer(0) 立即触发的那一发，Reset 前必须清干净
	}
	defer timer.Stop()

	for n := int64(1); ; n++ {
		// 第 n 个号的预定时刻。float64 换算后再转 Duration：
		// 支持 0.5 这类亚赫兹速率，整数除法在这里是错的。
		next := start.Add(time.Duration(float64(n) / cfg.Rate * float64(time.Second)))
		if next.After(cfg.Deadline) {
			return nil // Deadline 驱动截断：预定时刻越线，一个都不多发
		}

		// 睡到 next。wait 每轮重算（坑点：别在循环外算一次）。
		// 若因背压落后于时刻表，wait <= 0，跳过睡眠立即补发——
		// 追赶语义保证平均 RPS 仍对齐目标，漂移不累积。
		if wait := time.Until(next); wait > 0 {
			timer.Reset(wait)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
			}
		}

		// 发射。select 同时监听 ctx：即使阻塞在慢消费（背压）上，
		// 取消也能立即生效——这就是“立即停止发射”的落地。
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ch <- struct{}{}:
			if cfg.OnEmit != nil {
				cfg.OnEmit(n, time.Now())
			}
		}
	}
}
