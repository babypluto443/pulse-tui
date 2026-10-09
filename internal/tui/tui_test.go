package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/babypluto/pulse-tui/internal/attack"
	"github.com/babypluto/pulse-tui/internal/metrics"
)

// TestSparkline —— 已知序列精确断言块字符映射。
func TestSparkline(t *testing.T) {
	// max=8ms：1ms→▁(0)、4ms→▄(3)、8ms→█(7)
	if got, want := sparkline([]float64{1, 4, 8}, 10), "▁▄█"; got != want {
		t.Errorf("sparkline = %q, 期望 %q", got, want)
	}
	if got := sparkline(nil, 10); got != "" {
		t.Errorf("空输入应返回空串, got %q", got)
	}
	if got := sparkline([]float64{1, 2, 3}, 0); got != "" {
		t.Errorf("width=0 应返回空串, got %q", got)
	}
	// 全 0：有数据但无起伏 → 全部最矮块，不是空白
	if got, want := sparkline([]float64{0, 0, 0}, 10), "▁▁▁"; got != want {
		t.Errorf("全 0 应画最矮块, got %q", got)
	}
	// 截尾：width=2 只留最后两个（最新数据永远在最右）
	if got, want := sparkline([]float64{8, 1, 8}, 2), "▁█"; got != want {
		t.Errorf("超宽应截尾, got %q", got)
	}
}

// TestInstantRPS —— 瞬时 RPS 差分口径：total 差 / 窗口时长差。
func TestInstantRPS(t *testing.T) {
	prev := &metrics.Snapshot{Total: 50, Duration: 500 * time.Millisecond}
	cur := metrics.Snapshot{Total: 60, Duration: 600 * time.Millisecond}

	if got := instantRPS(prev, cur); got != 100.0 {
		t.Errorf("瞬时 RPS = %v, 期望 100（10 个 / 0.1s）", got)
	}
	// 首帧无 prev：退化为累计口径
	if got := instantRPS(nil, metrics.Snapshot{Total: 100, Duration: time.Second, RPS: 100}); got != 100 {
		t.Errorf("首帧 RPS = %v, 期望 100", got)
	}
	// 窗口无增长（排水期）：沿用累计口径，防除零
	flat := &metrics.Snapshot{Total: 50, Duration: 600 * time.Millisecond}
	if got := instantRPS(flat, metrics.Snapshot{Total: 60, Duration: 600 * time.Millisecond, RPS: 50}); got != 50 {
		t.Errorf("零增长帧 RPS = %v, 期望沿用累计 50", got)
	}
}

// TestModelKeys —— 决议 #2/#3 的按键语义：
// 压测中 q/ctrl+c → 调 cancel 且不退出；报告态 → 产生退出命令。
func TestModelKeys(t *testing.T) {
	canceled := false
	cancel := func() { canceled = true }

	// 压测中 q：触发 cancel，状态仍 running
	m := New(metrics.New(), cancel)
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if !canceled {
		t.Error("压测中 q 应触发引擎 cancel（raw mode 下唯一的排水入口）")
	}
	if m2.(Model).state != stateRunning {
		t.Error("压测中 q 不应直接退出，要等 DoneMsg")
	}

	// 压测中 ctrl+c：同样只 cancel
	canceled = false
	m2, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if !canceled {
		t.Error("压测中 ctrl+c 应触发 cancel")
	}
	_ = m2

	// 报告态 q / esc / ctrl+c：产生退出命令
	m.state = stateDone
	for _, key := range []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune{'q'}},
		{Type: tea.KeyEsc},
		{Type: tea.KeyCtrlC},
	} {
		if _, cmd := m.Update(key); cmd == nil {
			t.Errorf("报告态按键 %v 应产生退出命令", key)
		}
	}
}

// TestModelDone —— DoneMsg 后切报告视图，报告全文复用 report.Text。
func TestModelDone(t *testing.T) {
	agg := metrics.New()
	agg.Add(attack.Result{Timestamp: time.Now(), Latency: time.Millisecond, StatusCode: 200})
	agg.Add(attack.Result{Timestamp: time.Now(), Latency: time.Millisecond, StatusCode: 500})
	m := New(agg, func() {})

	m2, _ := m.Update(DoneMsg{Snap: agg.Snapshot()})
	tm := m2.(Model)

	if tm.state != stateDone {
		t.Fatalf("状态 = %v, 期望 done", tm.state)
	}
	for _, want := range []string{"Total:", "Success:", "Status code distribution:", "[q/Esc/Ctrl-C] 退出"} {
		if !strings.Contains(tm.View(), want) {
			t.Errorf("done 视图应含 %q:\n%s", want, tm.View())
		}
	}
}

// TestModelTickSamples —— tick 推进采样历史，且永不越过容量上限。
func TestModelTickSamples(t *testing.T) {
	agg := metrics.New()
	m := New(agg, func() {})
	for i := 0; i < historyCap+5; i++ {
		m2, _ := m.Update(tickMsg(time.Time{}))
		m = m2.(Model)
	}
	if len(m.rpsHist) > historyCap {
		t.Errorf("历史长度 %d 越过上限 %d", len(m.rpsHist), historyCap)
	}
	// 空聚合器的快照也应有采样点（全 0 曲线），不是 nil
	if len(m.rpsHist) == 0 {
		t.Error("tick 后应有采样点")
	}
	// prev 指向的快照是拷贝：连续两次 tick 不应 panic（浅拷贝会）
	m2, _ := m.Update(DoneMsg{Snap: agg.Snapshot()})
	_ = m2.(Model).View()
}
