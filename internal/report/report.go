package report

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/babypluto/pulse-tui/internal/metrics"
)

// LatencyMS 毫秒浮点延迟统计——TUI 画曲线直接可用。
type LatencyMS struct {
	Mean float64 `json:"mean"`
	P50  float64 `json:"p50"`
	P90  float64 `json:"p90"`
	P99  float64 `json:"p99"`
	P999 float64 `json:"p999"`
}

// JSONReport P4 TUI 的数据契约（P3 grill-me 决议 #3，定稿后只增不改）。
type JSONReport struct {
	Total       int64            `json:"total"`
	Success     int64            `json:"success"`
	Failed      int64            `json:"failed"`
	DurationMS  float64          `json:"duration_ms"`
	RPS         float64          `json:"rps"`
	LatencyMS   LatencyMS        `json:"latency_ms"`
	StatusCodes map[string]int64 `json:"status_codes"`
	Errors      map[string]int64 `json:"errors"`
}

// JSON 按契约渲染 JSON 报告。
func JSON(s metrics.Snapshot, w io.Writer) error {
	out := JSONReport{
		Total:       s.Total,
		Success:     s.Success,
		Failed:      s.Failed,
		DurationMS:  float64(s.Duration) / 1e6,
		RPS:         s.RPS,
		LatencyMS:   toLatencyMS(s),
		StatusCodes: make(map[string]int64, len(s.StatusCodes)),
		Errors:      make(map[string]int64, len(s.Errors)),
	}
	for code, n := range s.StatusCodes {
		out.StatusCodes[fmt.Sprint(code)] = n
	}
	for msg, n := range s.Errors {
		out.Errors[msg] = n
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// Text 渲染人读报告，排版习惯对齐 hey：总览 → 延迟 → 状态码分布 → 错误分布。
func Text(s metrics.Snapshot, w io.Writer) error {
	pct := func(n int64) string {
		if s.Total == 0 {
			return "—"
		}
		return fmt.Sprintf("%.1f%%", float64(n)/float64(s.Total)*100)
	}

	if _, err := fmt.Fprintf(w, `Summary:
  Total:      %d
  Success:    %d (%s)
  Failed:     %d (%s)
  RPS:        %.2f
  Duration:   %s

Latency (ms):
  Mean:   %.2f
  P50:    %.2f
  P90:    %.2f
  P99:    %.2f
  P99.9:  %.2f
`,
		s.Total,
		s.Success, pct(s.Success),
		s.Failed, pct(s.Failed),
		s.RPS,
		s.Duration.Round(1e5), // 0.1ms 粒度，别打出 9.992417875s 这种
		ms(s.LatencyMean), ms(s.P50), ms(s.P90), ms(s.P99), ms(s.P999),
	); err != nil {
		return err
	}

	// 状态码按数值升序，人类扫起来是单调的
	codes := make([]int, 0, len(s.StatusCodes))
	for c := range s.StatusCodes {
		codes = append(codes, c)
	}
	sort.Ints(codes)

	if _, err := fmt.Fprintln(w, "Status code distribution:"); err != nil {
		return err
	}
	if len(codes) == 0 {
		if _, err := fmt.Fprintln(w, "  (none)"); err != nil {
			return err
		}
	}
	for _, c := range codes {
		label := fmt.Sprintf("%d", c)
		if c == 0 {
			label = "0 (no response)"
		}
		if _, err := fmt.Fprintf(w, "  [%s]  %d  %s\n", label, s.StatusCodes[c], pct(s.StatusCodes[c])); err != nil {
			return err
		}
	}

	// 传输错误按数量降序、同数量按键名升序，排障时大头永远在最上
	if _, err := fmt.Fprintln(w, "Errors (transport):"); err != nil {
		return err
	}
	if len(s.Errors) == 0 {
		_, err := fmt.Fprintln(w, "  (none)")
		return err
	}
	msgs := make([]string, 0, len(s.Errors))
	for m := range s.Errors {
		msgs = append(msgs, m)
	}
	sort.Slice(msgs, func(i, j int) bool {
		if s.Errors[msgs[i]] != s.Errors[msgs[j]] {
			return s.Errors[msgs[i]] > s.Errors[msgs[j]]
		}
		return msgs[i] < msgs[j]
	})
	for _, m := range msgs {
		if _, err := fmt.Fprintf(w, "  %6d  %s\n", s.Errors[m], m); err != nil {
			return err
		}
	}
	return nil
}

func toLatencyMS(s metrics.Snapshot) LatencyMS {
	return LatencyMS{
		Mean: ms(s.LatencyMean),
		P50:  ms(s.P50),
		P90:  ms(s.P90),
		P99:  ms(s.P99),
		P999: ms(s.P999),
	}
}

// ms 纳秒 → 毫秒浮点（报告层的唯一换算点，聚合层永远存 ns 不丢精度）。
func ms(d time.Duration) float64 {
	return float64(d) / 1e6
}
