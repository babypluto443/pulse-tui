package tui

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/babypluto/pulse-tui/internal/metrics"
	"github.com/babypluto/pulse-tui/internal/report"
)

// tickInterval 拉快照的节拍。PLAN 提示 <100ms 会闪且费 CPU，250ms 够顺眼。
const tickInterval = 250 * time.Millisecond

// historyCap 采样历史上限：240 帧 = 60s @ 250ms，足够覆盖常规压测；
// View 按 width 再截尾，这里只是防内存无界。
const historyCap = 240

type state int

const (
	stateRunning state = iota
	stateDone
)

// DoneMsg 引擎收工通知（含最终快照），由 main 的引擎 goroutine 经
// p.Send 发给 TUI。导出是因为 main 要构造它。
type DoneMsg struct {
	Snap metrics.Snapshot
}

type tickMsg time.Time

// Model Bubble Tea 三件套。持有聚合器引用与引擎 cancel（决议 #2 的桥）。
type Model struct {
	agg    *metrics.Aggregator
	cancel context.CancelFunc // 压测中 q/ctrl+c 触发排水

	state state
	width int

	startedAt time.Time
	prev      *metrics.Snapshot // 上一帧，差分的被减数
	rpsHist   []float64         // 瞬时 RPS 历史
	p50Hist   []float64         // P50（ms）历史

	final    metrics.Snapshot
	reported string // done 态展示的报告全文（report.Text 渲染，不写第二遍）
}

// New 构造 Model。cancel 是 signal.NotifyContext 返回的 stop——
// 键盘桥接后走的是与系统信号完全相同的取消路径。
func New(agg *metrics.Aggregator, cancel context.CancelFunc) Model {
	return Model{
		agg:       agg,
		cancel:    cancel,
		state:     stateRunning,
		startedAt: time.Now(),
	}
}

// Init 启动定时拉取。
func (m Model) Init() tea.Cmd {
	return tickCmd()
}

func tickCmd() tea.Cmd {
	return tea.Tick(tickInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// Update 处理三类消息：tick（拉快照）、DoneMsg（引擎收工）、按键与窗口。
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tickMsg:
		if m.state == stateRunning {
			snap := m.agg.Snapshot() // 深拷贝，随便存
			m.pushSample(snap)
			m.prev = &snap
		}
		return m, tickCmd()

	case DoneMsg:
		m.final = msg.Snap
		var buf bytes.Buffer
		if err := report.Text(m.final, &buf); err != nil {
			m.reported = "报告渲染失败: " + err.Error()
		} else {
			m.reported = buf.String()
		}
		m.state = stateDone
		return m, nil

	case tea.WindowSizeMsg:
		m.width = msg.Width
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			if m.state == stateRunning {
				// raw mode 下没有 SIGINT，这里是排水的唯一入口：
				// 触发后不退，等引擎排干发来 DoneMsg 再看报告
				m.cancel()
				return m, nil
			}
			return m, tea.Quit
		case "esc":
			if m.state == stateDone {
				return m, tea.Quit
			}
		}
	}
	return m, nil
}

// pushSample 把一帧快照变成曲线采样点。瞬时 RPS 由纯函数 instantRPS 计算，
// metrics 里不存任何“实时状态”（决议 #4）。
func (m *Model) pushSample(s metrics.Snapshot) {
	m.rpsHist = append(m.rpsHist, instantRPS(m.prev, s))
	m.p50Hist = append(m.p50Hist, msOf(s.P50))
	if len(m.rpsHist) > historyCap {
		m.rpsHist = m.rpsHist[1:]
		m.p50Hist = m.p50Hist[1:]
	}
}

// instantRPS 瞬时 RPS = 相邻两帧 total 差 / 窗口时长差。
// 首帧（无 prev）退化为累计口径；窗口无增长（dt<=0，如排水期）沿用累计值。
func instantRPS(prev *metrics.Snapshot, cur metrics.Snapshot) float64 {
	if prev == nil {
		return cur.RPS
	}
	if dt := cur.Duration - prev.Duration; dt > 0 {
		return float64(cur.Total-prev.Total) / dt.Seconds()
	}
	return cur.RPS
}

// View running 态：头部指标 + P50 sparkline；done 态：报告全文常驻。
func (m Model) View() string {
	if m.state == stateDone {
		return m.reported + "\n[q/Esc/Ctrl-C] 退出"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "pulse-tui · 压测进行中 (%v)          [q/Ctrl-C] 停止并出报告\n\n",
		time.Since(m.startedAt).Round(100*time.Millisecond))

	if m.prev == nil {
		b.WriteString("等待第一帧快照…")
		return b.String()
	}

	errPct := 0.0
	if m.prev.Total > 0 {
		errPct = float64(m.prev.Failed) / float64(m.prev.Total) * 100
	}
	fmt.Fprintf(&b, "RPS(瞬时): %-8.1f 累计: %-6d 成功: %-6d 失败: %-4d 错误率: %.2f%%\n\n",
		m.latestRPS(), m.prev.Total, m.prev.Success, m.prev.Failed, errPct)

	// sparkline 宽度自适应：留出行首缩进与行尾余量
	lineW := m.width - 20
	if m.width == 0 {
		lineW = 60 // 首帧前 WindowSizeMsg 未到，给缺省宽度
	}
	if lineW < 10 {
		lineW = 10
	}
	fmt.Fprintf(&b, "延迟 P50 (ms) %s\n", sparkline(m.p50Hist, lineW))
	fmt.Fprintf(&b, "当前 P50: %.2fms   P99: %.2fms", msOf(m.prev.P50), msOf(m.prev.P99))
	return b.String()
}

func (m Model) latestRPS() float64 {
	if len(m.rpsHist) == 0 {
		return 0
	}
	return m.rpsHist[len(m.rpsHist)-1]
}

// msOf 纳秒 → 毫秒（视图层换算，聚合层永远存 ns——与 report.ms 同规矩）。
func msOf(d time.Duration) float64 {
	return float64(d) / 1e6
}
