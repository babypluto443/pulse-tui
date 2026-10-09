package report

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/babypluto/pulse-tui/internal/attack"
	"github.com/babypluto/pulse-tui/internal/metrics"
)

// buildSnapshot 用已知数据喂聚合器，得到稳定的测试快照。
func buildSnapshot() metrics.Snapshot {
	agg := metrics.New()
	for i := 0; i < 90; i++ {
		agg.Add(attack.Result{Timestamp: time.Unix(1_000_000, 0), Latency: time.Millisecond, StatusCode: 200})
	}
	for i := 0; i < 9; i++ {
		agg.Add(attack.Result{Timestamp: time.Unix(1_000_000, 0), Latency: 3 * time.Millisecond, StatusCode: 404})
	}
	agg.Add(attack.Result{Timestamp: time.Unix(1_000_000, 0), Latency: 10 * time.Millisecond, StatusCode: 0, Error: "dial tcp: connection refused"})
	return agg.Snapshot()
}

// TestTextAndJSONAgree —— M3 验收一的自动化版：
// 同一份 Snapshot 渲染 Text 与 JSON，数字必须一致（结构性保证的回归钉）。
func TestTextAndJSONAgree(t *testing.T) {
	snap := buildSnapshot()

	var textBuf bytes.Buffer
	if err := Text(snap, &textBuf); err != nil {
		t.Fatalf("Text: %v", err)
	}
	var jsonBuf bytes.Buffer
	if err := JSON(snap, &jsonBuf); err != nil {
		t.Fatalf("JSON: %v", err)
	}

	var jr JSONReport
	if err := json.Unmarshal(jsonBuf.Bytes(), &jr); err != nil {
		t.Fatalf("JSON 反序列化失败: %v\n%s", err, jsonBuf.String())
	}

	extract := func(pattern string) string {
		re := regexp.MustCompile(pattern)
		m := re.FindStringSubmatch(textBuf.String())
		if m == nil {
			t.Fatalf("文本中找不到 %q:\n%s", pattern, textBuf.String())
		}
		return m[1]
	}

	if got, want := extract(`Total:\s+(\d+)`), fmtInt(jr.Total); got != want {
		t.Errorf("Total 不一致: text=%s json=%d", got, jr.Total)
	}
	if got, want := extract(`Success:\s+(\d+)`), fmtInt(jr.Success); got != want {
		t.Errorf("Success 不一致: text=%s json=%d", got, jr.Success)
	}
	if got, want := extract(`Failed:\s+(\d+)`), fmtInt(jr.Failed); got != want {
		t.Errorf("Failed 不一致: text=%s json=%d", got, jr.Failed)
	}
	textRPS := parseFloat(t, extract(`RPS:\s+([\d.]+)`))
	if diff := textRPS - jr.RPS; diff > 0.01 || diff < -0.01 {
		t.Errorf("RPS 不一致: text=%.4f json=%.4f", textRPS, jr.RPS)
	}
	textP50 := parseFloat(t, extract(`P50:\s+([\d.]+)`))
	if diff := textP50 - jr.LatencyMS.P50; diff > 0.01 || diff < -0.01 {
		t.Errorf("P50 不一致: text=%.4fms json=%.4fms", textP50, jr.LatencyMS.P50)
	}
	if jr.StatusCodes["200"] != 90 || jr.StatusCodes["404"] != 9 || jr.StatusCodes["0"] != 1 {
		t.Errorf("JSON 状态码分布不对: %v", jr.StatusCodes)
	}
	if jr.Errors["dial tcp: connection refused"] != 1 {
		t.Errorf("JSON errors 分组不对: %v", jr.Errors)
	}
}

// TestJSONKeysFixed —— 契约字段名回归：P4 只认这些键，改名即破坏契约。
func TestJSONKeysFixed(t *testing.T) {
	var buf bytes.Buffer
	if err := JSON(buildSnapshot(), &buf); err != nil {
		t.Fatalf("JSON: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(buf.Bytes(), &raw); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	for _, key := range []string{"total", "success", "failed", "duration_ms", "rps", "latency_ms", "status_codes", "errors"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("契约字段缺失: %q", key)
		}
	}
	lat, ok := raw["latency_ms"].(map[string]any)
	if !ok {
		t.Fatalf("latency_ms 应为对象: %v", raw["latency_ms"])
	}
	for _, key := range []string{"mean", "p50", "p90", "p99", "p999"} {
		if _, ok := lat[key]; !ok {
			t.Errorf("latency_ms 契约字段缺失: %q", key)
		}
	}
}

func fmtInt(n int64) string {
	return strconv.FormatInt(n, 10)
}

func parseFloat(t *testing.T, s string) float64 {
	t.Helper()
	var f float64
	if _, err := fmt.Sscanf(s, "%f", &f); err != nil {
		t.Fatalf("解析数字 %q 失败: %v", s, err)
	}
	return f
}
