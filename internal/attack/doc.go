// Package attack 实现攻击器：工人池 + net/http + 优雅排水（P2 / M2）。
//
// 核心思想（P2 grill-me 定稿）：
//   - -c 是工人池大小，不是并发驱动。发射节奏永远由 rate 包决定，
//     worker 只负责“拿到号就发一个请求”。
//   - 全局复用一个 http.Client（连接池由 Transport 管），
//     MaxIdleConnsPerHost = MaxConnsPerHost = Workers，
//     避免 TIME_WAIT 与连接层排队拖慢实测。
//
// 排水语义（SPEC §6）：Ctrl-C → 立即停止发射 → 在途请求**自然跑完** →
// 才进报告。为此请求不挂主 ctx，而挂内部 drain ctx（全部工人完工后才
// 取消）——否则在途请求会被中断，报告末尾出现人为错误尖峰。
// 最坏排水时长 = -timeout（请求挂死的保险丝）。
//
// Result 契约（P3 的输入，字段一次定好）：4 字段原始记录，
// Error 只装传输层失败；2xx/4xx/5xx 的分类裁判权在 P3。
//
// 对 P1 契约的履约：signals 由调用方创建（容量即 burst，P2 定为 -c）；
// rate.Run 是它的唯一 close 方；worker 消费用两级 select 优先检查
// ctx.Done()，取消后绝不执行缓冲遗留号；本包绝不 close signals。
package attack
