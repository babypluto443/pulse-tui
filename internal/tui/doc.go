// Package tui 实现实时压测视图（P4 / M4，Bubble Tea——项目唯一直接依赖）。
//
// 定位（PLAN P4）：TUI 是消费者，不是参与者。压测引擎照旧在后台跑，
// TUI 通过 tea.Tick 定时拉取 metrics.Aggregator.Snapshot()（并发安全
// 深拷贝，P3 地基）渲染，绝不阻塞发射。
//
// Ctrl-C 语义（P4 grill-me 决议 #2）：Bubble Tea 的 raw mode 关闭了
// ISIG，Ctrl-C 不再产生 SIGINT——signal.NotifyContext 在 TUI 模式下
// 收不到。因此 model 持有引擎的 cancel 函数：压测中 q/ctrl+c 触发
// 排水（与 -json 路径同一语义），报告态下才是退出。
package tui
