# pulse-tui

**A constant-RPS HTTP load testing CLI with a live terminal UI.**

Watch RPS, error rate and a latency sparkline *while* the test runs — no more staring at a black box. Written in Go, core engine has **zero third-party dependencies** (the TUI's Bubble Tea is the only one, and only for the view layer).

```
$ pulse-tui -r 100 -d 10s http://localhost:8080/api/v1/feed?limit=20

pulse-tui · 压测进行中 (5s)          [q/Ctrl-C] 停止并出报告

RPS(瞬时): 99.9     累计: 500    成功: 500    失败: 0    错误率: 0.00%

延迟 P50 (ms) ████▇▇▇▇▇▇▇▇▇▇▇▇▇▇▇▇
当前 P50: 3.85ms   P99: 21.83ms
```

*(real terminal output captured from a run against a local Gin + MySQL service)*

## Why

| | pulse-tui | hey | vegeta | k6 |
|---|---|---|---|---|
| Live view during the run | ✅ TUI | ❌ black box | ❌ batch (attack → report) | ⚠️ web dashboard |
| Constant-RPS driven | ✅ absolute schedule, no drift | ❌ concurrency driven | ✅ | ✅ (scripted) |
| Ctrl-C keeps partial results | ✅ drains, then reports | ❌ | ❌ | ⚠️ |
| Setup | single binary, zero flags needed | single binary | CLI + files | JS scripting |
| Result formats | text + JSON (same numbers) | text | HDR histogram | many |

The pitch: **see the pulse while it beats.** Set parameters wrong? Hit `q`, get the report for everything already measured.

## Install

```bash
go install github.com/babypluto/pulse-tui/cmd/pulse-tui@latest
```

Or build from source:

```bash
git clone https://github.com/babypluto/pulse-tui && cd pulse-tui
./build.sh            # cross-builds linux/darwin amd64+arm64 into dist/
```

## Usage

```
pulse-tui [flags] <URL>

  -r float        requests per second, constant-RPS semantics (default 50)
  -d duration     test duration (default 10s)
  -c int          worker pool size = max in-flight requests (default 10)
  -X string       HTTP method (default GET)
  -H header       custom header, repeatable (-H "Authorization: Bearer x")
  -b string       request body (use with -X POST)
  -timeout dur    per-request timeout (default 30s)
  -json           output the report as JSON (pipe-friendly)
```

Examples:

```bash
# feed endpoint at 100 RPS for 30 seconds
pulse-tui -r 100 -d 30s http://localhost:8080/feed

# authenticated POST with a JSON body
pulse-tui -r 20 -d 60s -X POST \
  -H "Authorization: Bearer xxx" \
  -b '{"title":"hello","content":"world"}' \
  http://localhost:8080/posts

# CI / scripts: JSON only, live UI skipped automatically when piped
pulse-tui -r 100 -d 10s -json http://localhost:8080/feed | jq .rps
```

### How stopping works

Press `q` (or Ctrl-C) mid-run: emission stops immediately, in-flight requests
finish naturally, and the full report covers everything already measured —
nothing is wasted. Worst-case drain time is bounded by `-timeout`.

### Metric definitions

- **latency**: request written → entire response body read (matches hey/vegeta)
- **success**: HTTP 2xx; **failed**: everything else (incl. transport errors)
- **RPS**: total / (last response done − first request started) — drain time excluded
- percentiles: P50/P90/P99/P99.9, `ceil(q*n)-1` indexing

## Real-world numbers

Benchmarked against a real Gin + GORM + MySQL service (local feed endpoint,
`limit=20` payload), 100 RPS × 10s, `-c 20`:

```
Summary:
  Total:      999        Success:    999 (100.0%)
  Failed:     0 (0.0%)   RPS:        100.07
  Duration:   9.9827s

Latency (ms):
  Mean: 3.98   P50: 3.72   P90: 4.97   P99: 10.21   P99.9: 54.50
```

Same numbers as JSON (`-json`), byte for byte — both renderers consume one
immutable snapshot.

## Architecture

```
scheduler goroutine (1)          workers 1..N (-c)
  next = start + n/rate    ─┐     ┌─ send HTTP request
  absolute schedule,        ├─ chan ─┤  (net/http + -timeout)
  zero accumulated drift   ─┘ burst └─ Result → metrics (atomic) → report
```

- `internal/rate` — constant-RPS scheduler: absolute-time-table emission,
  backpressure via bounded channel
- `internal/attack` — worker pool + net/http, graceful drain on SIGINT
- `internal/metrics` — lock-free counters, fixed-array status-code histogram,
  mutex-shielded latency samples
- `internal/report` — text + JSON from one snapshot
- `internal/tui` — Bubble Tea consumer; polls snapshots, never blocks emission

## 中文简介

pulse-tui 是一个以并发架构为核心的 HTTP 压测 CLI：恒定 RPS 精确发号
（绝对时间表调度，误差不累积）、工人池消费、Ctrl-C 优雅排水——
压测进行中可以实时看到瞬时 RPS、错误率和延迟曲线，随时停下拿走已有数据。

- 安装：`go install github.com/babypluto/pulse-tui/cmd/pulse-tui@latest`
- 指标口径对齐 hey/vegeta（延迟 = 写出请求到读完响应 body，成功 = 2xx）
- 核心引擎零第三方依赖；Bubble Tea 仅用于视图层

## Roadmap

- [x] P0–P4: scaffold → rate scheduler → attack engine → metrics/report → live TUI
- [ ] GIF demo
- [ ] v2: concurrency-driven pacer, request scenarios, httptrace breakdown

## License

MIT
