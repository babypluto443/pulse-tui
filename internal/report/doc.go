// Package report 实现报告渲染：文本版 + JSON 版（P3 / M3）。
//
// 核心思想：两种格式消费同一份 metrics.Snapshot——数字想不一致都难，
// 这是 M3 验收“文本与 --json 数字一致”的结构性保证，而不是靠对账。
//
// JSON 契约（P3 grill-me 决议 #3）：JSONReport 的字段是 P4 Bubble Tea
// TUI 的数据源，定稿后只增不改。latency 一律转毫秒浮点（TUI 画曲线
// 直接可用）；status_codes/errors 的键为字符串（JSON 对象键的限制）。
package report
