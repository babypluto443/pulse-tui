# pulse-tui — 分阶段开发计划（PLAN）

> 版本 v0.1 · 2026-10-08 · 依据 docs/SPEC.md（v0.1）制定
> 执行原则：每个阶段只做一件事；阶段结束必须先过验收标准，再进下一阶段。
> 约定：每完成一个里程碑，在 `document/` 下写一篇开发记录（格式参照 02-T1.1-开发记录.md）。

## 总览

```
P0 脚手架 ──→ P1 控速器 ──→ P2 攻击器 ──→ P3 统计+报告 ──→ P4 实时 TUI ──→ P5 发布
（半天）      （对应M1）     （对应M2）     （对应M3）       （对应M4）      （对应M5）
```

依赖关系是单向的：P1 产出的"发射信号 channel"是 P2 的输入，P2 产出的 Result 流是 P3 的输入，P3 的 JSON 快照是 P4 的输入。任何阶段卡住，退回上一阶段检查接口约定，不要在当前阶段堆补丁。

---

## P0 · 项目脚手架（对应里程碑：无，约半天）

**目标**：仓库能编译、CLI 骨架能跑，后续每个包都有落点。

**任务清单**
1. `go mod init github.com/babypluto/pulse-tui`
2. 建目录骨架：`cmd/pulse-tui/`、`internal/rate/`、`internal/attack/`、`internal/metrics/`、`internal/report/`（空包先放 doc.go 说明职责）
3. `main.go`：用 stdlib `flag` 解析 §3 的全部参数（-r/-d/-c/-X/-H/-b/-timeout/-json），解析后先只打印一句"参数确认"，不发请求
4. `.gitignore`（忽略二进制）、README 占位

**验收**
- `go build ./...` 通过；`./pulse-tui -r 100 -d 10s http://x` 能打印参数回显
- flag 类型想清楚：`-r` 用 `float64`（RPS 数值，支持亚赫兹速率；原 spec 类型列误写 duration，2026-10-08 已订正）、`-H` 用自定义 `flag.Value` 支持重复

**易踩的坑**
- `-H` 可重复参数必须自定义类型实现 `flag.Value` 接口，stdlib 不直接支持
- duration 型 flag 的默认值写法是 `flag.Duration("d", 10*time.Second, ...)`，别手滑写成字符串

---

## P1 · rate 控速器：绝对时间表调度（对应 M1）

**目标**：造出项目的"心脏"——一个按恒定 RPS 精确发号、无累积漂移的调度 goroutine。

**核心思想（先想明白再写码）**
- 错误做法：`time.Tick(1/rate)` 等间隔发号——每次 sleep 都有误差，误差会**累积**，压 60s 后实际 RPS 明显偏离目标
- 正确做法：**绝对时间表**。记下 `start`，第 n 个号的发射时刻 = `start + n/rate`，每次睡到"下一个绝对时刻"再发。单次误差不累积
- 背压：发号通过有缓冲 channel（容量即 burst）送出，下游工人全忙时调度器阻塞 → 实测 RPS 自然下降，spec §5 的"正常降级"就来自这里

**任务清单**
1. 定义接口：`rate.Limiter`（或直接函数 `Run(ctx, rate, total, ch chan<- struct{})`），支持 context 取消
2. 实现 `internal/rate/`：绝对时间表 + `time.Timer` 精确睡眠
3. 写 `cmd/ratetest`（或 `_test.go`）：连读发射时刻验证间隔
4. 单元测试：`rate=100` 连续 100 个号，间隔 10ms ± 20%

**验收（spec M1 原文）**
- rate=100 连读 100 个时刻，间隔 10ms ± 20%

**易踩的坑**
- 用 `time.Sleep(until)` 时 `until` 要在循环里重算，别在循环外算一次
- `-d 10s -r 100` 应发约 1000 个号：总量的封顶逻辑（duration 截断）放在调度器里，channel 关闭时机要和它对齐
- 测试别用真实墙钟硬断言单次间隔，抖动会偶发红；按 spec 的 ±20% 宽松断言整体分布

---

## P2 · attack 攻击器：工人池 + HTTP + 优雅排水（对应 M2）

**目标**：接上真实网络 IO，闭环跑通"发号 → 在途请求 → 取消/结束 → 排水"。

**核心思想**
- `-c` 是工人池大小，不是并发驱动——发射节奏永远由 P1 决定，工人只管"拿到号就发一个请求"
- 一个 `http.Client` 全局复用（连接池由 Transport 管），别每个请求 new Client
- 优雅排水（spec §6）：`signal.NotifyContext` 捕获 SIGINT → 先关掉 rate 发号（立即停止发射）→ `WaitGroup.Wait()` 等全部在途请求返回 → 才进报告

**任务清单**
1. 定义 `attack.Result`（latency、status code、error、时间戳）——这是 P3 的输入契约，字段一次定好
2. 实现 worker pool：N 个 goroutine 从号 channel 取号、发请求、把 Result 送进 metrics 聚合口
3. Transport 参数：`MaxIdleConns`/`MaxIdleConnsPerHost` 调大（否则高 RPS 下大量 TIME_WAIT 拖慢实测）
4. 串联 main：flag → rate → attack → Ctrl-C 排水 →（先打印 total 数占位）

**验收（spec M2 原文）**
- 对本地起一个简单服务（`python3 -m http.server` 或自己写个 gin hello），`-r 100 -d 10s`，实测 RPS 100 ± 5%
- 压测中途 Ctrl-C，程序不悬挂、能出报告（哪怕是简陋的 total 统计）

**易踩的坑**
- worker 里必须用 `http.NewRequestWithContext(ctx, ...)`，否则 -timeout 之外没有第二条取消通道
- Result channel 忘了 close 或 metrics 端没读完，都会让排水卡死——close 的责任方要唯一（约定谁生产谁 close）
- 测 RPS 时先确认本机服务跟得上，服务先垮了会误判成控速器不准

---

## P3 · metrics 统计 + report 报告（对应 M3）

**目标**：把 Result 流变成可信数字——这是"口径对齐竞品"承诺的落地。

**核心思想**
- 热路径（每个请求一次）只用 `sync/atomic`：total/success/fail 计数、状态码分布用分片 map 或 atomic per-code
- latency 采样收集到切片，报告时一次排序算分位数——spec 已锁定 MVP 方案，别上 HDR 直方图
- 文本和 JSON 用**同一份**聚合结果渲染，天然保证"两轨数字一致"

**任务清单**
1. `internal/metrics/`：聚合器（atomic 计数 + latency 切片 + 状态码分布），提供 `Snapshot()` 返回不可变汇总
2. 分位数：排序后取 P50/P90/P99/P99.9（注意下标取整规则写进注释，口径要稳定）
3. `internal/report/`：文本版（对齐 hey 的排版习惯）+ JSON 版（`-json`，字段命名定死，这是 P4 TUI 的数据契约）
4. 对比验证：同参数与 hey 各跑几轮

**验收（spec M3 原文）**
- 文本与 --json 数字一致
- 同参数与 hey 结果数量级一致（RPS、P50/P99 允许合理抖动，量级不能差）

**易踩的坑**
- latency 单位统一存纳秒（`time.Duration`），报告层才转 ms，别在中途丢精度
- 排序分位数的 index：`ceil(q*n)-1` 还是 `int(q*n)` 差一位就差一截，固定一种并注释
- 状态码分布若用普通 map + mutex，高并发下锁竞争会偷偷吃掉 RPS——至少用分片

---

## P4 · 实时 TUI：Bubble Tea（对应 M4）

**目标**：项目的差异化卖点落地——边压边看 RPS、延迟曲线、错误率。

**核心思想**
- TUI 是消费者，不是参与者：压测引擎照旧跑，TUI 每 250ms 左右从 metrics 拉 `Snapshot()` 渲染，绝不让渲染阻塞发射
- 数据流：metrics 里加一个带锁的 snapshot 口（或 channel），`tea.Tick` 定时拉取 → `Update` → `View`

**任务清单**
1. 引入 Bubble Tea（`go get github.com/charmbracelet/bubbletea`）——这是唯一允许的第三方依赖，写进 go.mod
2. Model/Update/View 三件套：头部 RPS/错误率实时数字 + 延迟 sparkline（可用 ASCII 手画或 lipgloss）
3. 结束后 TUI 底部直接展示 P3 的最终报告（复用 report 包，别写第二遍）
4. 终端尺寸自适应 + `-json` 模式下跳过 TUI 直接输出（管道场景）

**验收（spec M4 原文）**
- 压测中实时刷新 RPS / 延迟曲线 / 错误率；Ctrl-C 行为不变（照常排水出报告）

**易踩的坑**
- `tea.Tick` 的间隔别太密（<100ms 会闪且费 CPU）
- Snapshot 必须是拷贝，别把内部切片直接递给 View，排序时会被并发写炸
- Bubble Tea 程序退出后要恢复终端状态，用它的标准退出路径，别 os.Exit 硬退

---

## P5 · 发布（对应 M5）

**目标**：陌生人一行 `go install` 可用。

**任务清单**
1. README：一句话定位 + GIF/asciinema 演示 + flags 表 + 与 hey/vegeta/k6 的定位对比（复用 spec §1 的差异化叙事）
2. 交叉编译：GOOS/GOCOMPILER 组合脚本或最小 goreleaser 配置（linux/darwin amd64+arm64）
3. `git tag v0.1.0` + push，验证 `go install github.com/babypluto/pulse-tui/cmd/pulse-tui@latest`
4. 自己先当第一批用户：拿 pluto_feed / DiTing-Go 的接口真实压一轮，把结果截图进 README

**验收（spec M5 原文）**
- 陌生人 `go install` 一行可用；README 自解释

---

## 风险与依赖提示

| 风险 | 影响 | 对策 |
|---|---|---|
| P1 时间表精度不达标 | 整个"恒定 RPS"卖点崩 | M1 验收先行，不达标不进 P2；必要时换 `time.Timer` 替代 Sleep |
| 状态码 map 锁竞争 | P3 实测 RPS 被统计拖低 | 分片 map / atomic per-code，压测期留意 CPU 占用 |
| Bubble Tea 与 Ctrl-C 冲突 | 排水语义被 TUI 破坏 | 信号仍由引擎层 ctx 管理，TUI 只消费；P4 验收必须复测 Ctrl-C |
| 范围膨胀 | 永远到不了 v0.1.0 | 非目标清单在 spec §2，动手前重读一遍 |

## 排期建议（每周 10h 口径）

- 第 1 周：P0 + P1（含 ratetest 与单测）
- 第 2 周：P2（排水语义多测几轮 Ctrl-C）
- 第 3 周：P3（含 hey 对比）
- 第 4 周：P4
- 第 5 周：P5 + 真实项目实战 + 发布
