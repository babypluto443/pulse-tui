// Package metrics 实现统计聚合与分位数（P3 / M3）。
//
// 热路径并发策略（P3 grill-me 定稿，混合方案）：
//   - total/success/failed：3 个 atomic.Int64，零锁；
//   - 状态码分布：[1000]atomic.Int64 定长数组——状态码只可能落在
//     0~999（0 = 传输失败桶），比“分片 map”更简单、更快、零锁零分片；
//   - latency 切片与 errors 分组：共用一把 mutex（append 均摊 ~50ns，
//     10k RPS 下锁占用可忽略——PLAN 预警的锁竞争在此量级是伪问题）。
//
// 成败口径（SPEC §5 + P3 补缺）：success = 2xx；failed = 其余一切
// （含 3xx/1xx/传输错误 code=0）。Result 只当记录员，分类裁判在这里。
//
// RPS 口径：请求窗口 = 最后响应完成 − 首请求开始（用 atomic CAS 维护
// min/max），排水尾巴不计入分母。
//
// Snapshot() 返回深拷贝：latency 拷贝后排序（不动内部切片），
// map 全部重建——文本与 JSON 报告消费同一份快照，双轨数字天然一致。
package metrics
