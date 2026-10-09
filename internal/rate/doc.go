// Package rate 实现恒定 RPS 控速器（P1 / M1）。
//
// 核心思想：绝对时间表调度，而非等间隔 sleep。
// 记下 start 时刻后，第 n 个发射信号的预定时刻 = start + n/rate，
// 每次睡到“下一个绝对时刻”再发号——单次误差不累积，无漂移。
//
// 对外形态（P1 grill-me 定稿）：普通函数
//
//	Run(ctx, cfg Config, ch chan<- struct{}) error
//
// 不抽接口：单实现不配抽象，v2 出现第二种调度策略（并发数驱动 Pacer）
// 时再从第二个实现里长出接口。参数收进 Config，加字段不改签名。
//
// 截断语义（Deadline 驱动）：循环只判定“预定时刻 next 是否 ≤ Deadline”，
// 越线即停。不预计算 total，避免浮点取整边界与两套判定不一致。
//
// 背压语义（SPEC §5）：ch 由调用方创建，缓冲容量即 burst。下游 worker
// 全忙导致 ch 满时，调度器阻塞在发送上，实测 RPS 自然下降，属正常降级。
// 因按绝对时间表调度，阻塞结束后会“追赶”预定时刻，平均 RPS 仍对齐目标。
//
// close 责任：Run 是唯一 close 责任方——正常到点、ctx 取消、参数错误，
// 任何退出路径都先 close ch 再返回。调用方绝不 close。
// ctx 取消时立即返回 ctx.Err()，缓冲中遗留的信号由消费端丢弃
// （P2 的 worker 必须在 select 里同时监听 ctx.Done()，不能闷头 range）。
//
// 发射时刻打点：Config.OnEmit(n, at) 为可空回调，在调度器侧记录权威
// 发射时刻（不被消费延迟污染）；nil 时零开销，热路径不为测试买单。
package rate
