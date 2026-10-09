package tui

import (
	"strings"
)

// blocks 八级块字符，从最矮到最高。sparkline 的“像素”。
const blocks = "▁▂▃▄▅▆▇█"

// sparkline 把数值序列渲染成块字符迷你图（纯函数，可单测）。
//
// 标度：0 基（latency/RPS 没有负值，从 0 起才能看出增长），
// 最大值映射到最高块；全 0 时全部画最矮块（有数据但无起伏，别画空白）。
// 超过 width 时只取**尾部**——最新的数据永远在最右边。
func sparkline(values []float64, width int) string {
	if width <= 0 || len(values) == 0 {
		return ""
	}
	if len(values) > width {
		values = values[len(values)-width:]
	}

	max := 0.0
	for _, v := range values {
		if v > max {
			max = v
		}
	}

	// 块字符是 3 字节 UTF-8：必须按 rune 取，WriteByte 会把
	// "▁"(e2 96 81) 拆成碎字节（第一次实现就栽在这）
	runes := []rune(blocks)

	var b strings.Builder
	for _, v := range values {
		idx := 0
		if max > 0 {
			// v/max ∈ [0,1] 映射到 0..7；乘 7.99 防止 v==max 时浮点落在 8 越界
			idx = int(v / max * 7.99)
		}
		b.WriteRune(runes[idx])
	}
	return b.String()
}
