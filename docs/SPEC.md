# pulse-tui — 项目规格书（SPEC）

> 版本 v0.1 · 2026-10-08 · 定稿于 grill-me 拷问会
> 本文档是唯一事实来源。改需求先改这里，再改代码。

## 1. 定位与痛点

**一句话**：一个并发架构为核心的 HTTP 压测 CLI——跑起来就能实时看到 RPS、延迟曲线和错误率的终端工具。

**痛点**：
- hey 输出干巴巴一行结果，压测进行中是黑盒
- vegeta 是批处理风格（attack → 落盘 → report），中途不可视
- k6 需要写 JS 脚本，上手成本高
- **体验代差 = 我们的生存空间**：边压边看，参数设错当场 Ctrl-C 拿走数据

**受众**：全球开发者。第一批真实用户：作者自己的 pluto_feed / DiTing-Go 项目。

## 2. 非目标（同样重要，防止范围膨胀）

以下能力明确**不在** v1 范围内，写进 spec 以免未来的自己手痒：
- ❌ wrk 式并发数驱动模式（v2：为 Pacer 接口加第二种实现）
- ❌ 多接口比例编排、请求参数模板化、登录态自动管理（v2"场景"能力）
- ❌ 分布式压测（主从合流）
- ❌ 分段耗时拆分（DNS/连接/TLS/首字节，httptrace，v2）
- ❌ HTML/图片格式报告导出
- ❌ 除 Bubble Tea 外的任何第三方运行时依赖

## 3. CLI 接口

```
pulse-tui [flags] <URL>

  -r float        每秒发射请求数，恒定 RPS 语义（默认 50；float64，支持 0.5 亚赫兹速率）
  -d duration     压测持续时间（默认 10s）
  -c int          工人池大小，仅控制同时在途的请求数上限（默认 10）
  -X string       HTTP method（默认 GET）
  -H header       自定义 header，可重复（-H "Authorization: Bearer x"）
  -b string       请求 body（配合 -X POST 等使用）
  -timeout dur    单请求超时（默认 30s）
  -json           以 JSON 输出报告（供管道/TUI 复用）
```

**行为模型（分叉 1 决议）**：`-r 100` = 每秒精确发射 100 个请求，与响应快慢无关；`-c` 只是工人池大小。若工人全忙导致发射信号积压，触发背压——见 §5。

**示例**：
```bash
pulse-tui -r 100 -d 30s http://localhost:8080/feed
pulse-tui -r 20 -d 60s -X POST -H "Authorization: Bearer xxx" \
  -b '{"title":"hello","content":"world"}' http://localhost:8080/posts
```

## 4. 架构与数据流

```
调度 goroutine（1 个）          worker 1..N（-c 决定 N）
  next = start + n/rate    ─┐     ┌─ 发送 HTTP 请求
  按绝对时间表发号           ├─ channel ─┤   （net/http + -timeout）
  无累积漂移               ─┘  limiter.C └─ Result → metrics（原子聚合）
                                              ↓ 自然结束或 Ctrl-C 排水后
                                          report（文本 / --json）
```

包结构（对标 vegeta 四模块）：
```
pulse-tui/
├── cmd/pulse-tui/main.go   # CLI 组装（stdlib flag）
├── internal/rate/          # 控速器：绝对时间表调度
├── internal/attack/        # 攻击器：工人池 + net/http + 优雅排水
├── internal/metrics/       # 统计：sync/atomic 聚合 + 分位数
├── internal/report/        # 报告：文本 / JSON
├── internal/tui/           # 实时 TUI（P4 起，Bubble Tea——唯一第三方依赖）
└── docs/SPEC.md
```

## 5. 指标口径（硬定义，分叉 3 决议）

| 指标 | 定义 |
|---|---|
| latency | 从请求写出 → 读完整个响应 body（对齐 hey/vegeta，数字可对比） |
| 成功 | HTTP 2xx |
| 失败 | 4xx / 5xx / 网络错误 / 超时；同时保留状态码全量分布 |
| 超时 | 默认 30s，`-timeout` 可调；超时请求计入失败，不悬空 |
| 报告指标 | total 数、实测 RPS、latency mean/P50/P90/P99/P99.9、错误数及占比、状态码分布 |

**背压语义**：channel 缓冲（burst）满时调度器阻塞 → 实际 RPS 低于目标 RPS 属于**正常降级**，报告中的实测 RPS 会如实反映。

## 6. 停止语义（分叉 5 决议）

- 自然结束：`-d` 时长到 → 停止发射 → 排水 → 报告
- Ctrl-C：`signal.NotifyContext` 捕获 SIGINT → **立即停止发射** → **等所有在途请求返回** → 照常输出报告（前半段数据不浪费）

## 7. 依赖策略（分叉 6 决议）

- M1–M3：**零第三方依赖**（stdlib flag / net/http / sync/atomic / sort）
- M4：引入 Bubble Tea（唯一例外）
- 分位数实现：MVP 收集全部 latency 切片后排序（3000~60 万样本下内存与耗时均可控），HDR 直方图留 v2

## 8. 里程碑与验收标准

| 里程碑 | 内容 | 验收标准 |
|---|---|---|
| M1 | rate 控速器 + cmd/ratetest | rate=100 连读 100 个时刻，间隔 10ms ± 20% |
| M2 | attack（工人池 + http + 排水） | 对本地服务 -r 100 -d 10s，实测 RPS 100 ± 5%；Ctrl-C 能出报告 |
| M3 | metrics + report | 文本与 --json 数字一致；同参数与 hey 结果数量级一致 |
| M4 | 实时 TUI（Bubble Tea） | 压测中实时刷新 RPS / 延迟曲线 / 错误率 |
| M5 | 发布 | README + 交叉编译；陌生人 `go install` 一行可用 |

## 9. 已锁定设计决策记录（分叉编号对应拷问会）

1. **恒定 RPS 驱动**——延迟统计可解释；控速器即心脏；并发驱动留 v2
2. **method+header+body**——第一批用户（作者自己）当天要压 POST+鉴权接口
3. **口径对齐竞品**——数字可信、可与 hey/vegeta 对比
4. **文本+JSON 双轨**——JSON 是 TUI 与管道复用的地基
5. **优雅排水**——中途数据不浪费；顺便实战 context 取消
6. **MVP 零依赖**——单二进制哲学 + 面试叙事"核心零依赖"
7. **命名 pulse-tui**——module `github.com/babypluto/pulse-tui`
