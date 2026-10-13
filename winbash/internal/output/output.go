// Package output 负责把值渲染成 AI 可解析的形式。
//
// 本包存在的唯一理由是 spec §6 的「值保证」:
// 0xa → 10、STATE: 1 → Stopped、86,488 K → 86488064、无 ANSI 色码。
// 对 AI 来说,确定性的值比"好看的输出"重要得多 —— 人眼能忽略噪音,解析器不能。
package output

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// FormatBytes 渲染字节数。human=false 时返回纯十进制数字。
//
// 默认不给人看:"86,488 K" 这种带千分位和单位的字符串正是要消灭的东西 ——
// 它看起来像数字,但不是数字,AI 得先猜单位才能比大小。
func FormatBytes(n int64, human bool) string {
	if !human {
		return strconv.FormatInt(n, 10)
	}
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + "B"
	}
	units := []string{"K", "M", "G", "T", "P"}
	v := float64(n)
	i := -1
	for v >= unit && i < len(units)-1 {
		v /= unit
		i++
	}
	return strconv.FormatFloat(v, 'f', 1, 64) + units[i]
}

// FormatDWORD 把注册表 DWORD 渲染成十进制。0x0000000a → "10"。
//
// 注册表导出的十六进制在 AI 眼里是个陷阱:0x0000000a 既可能是十进制 10,
// 也可能被当成字符串 "a"。这里一律落到十进制,不保留原样。
func FormatDWORD(v uint32) string {
	return strconv.FormatUint(uint64(v), 10)
}

var serviceStates = map[int]string{
	1: "Stopped",
	2: "StartPending",
	3: "StopPending",
	4: "Running",
	5: "ContinuePending",
	6: "PausePending",
	7: "Paused",
}

// FormatServiceState 把 Windows 服务状态常量码翻译成词。
//
// 未知值返回数字本身并标记 ok=false —— 不认识就不猜,
// 宁可让调用方看到裸数字,也不能编一个看起来很像的状态名。
func FormatServiceState(v int) (string, bool) {
	if s, ok := serviceStates[v]; ok {
		return s, true
	}
	return strconv.Itoa(v), false
}

// ansiRe 匹配 SGR 之类的 CSI 序列。pwsh 7 在终端下会自动上色,
// 那些字节对 AI 是纯噪音,而且会让同一份数据在有无终端时长得不一样。
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

// StripANSI 删除 ANSI 转义序列。
func StripANSI(s string) string { return ansiRe.ReplaceAllString(s, "") }

// TSV 渲染成 Tab 分隔的表。
//
// 列顺序由调用方固定给出,不由数据决定 —— 同一命令每次的输出列必须一致,
// 这样 AI 才能写死第几列,而不是每轮重新推断。
func TSV(header []string, rows [][]string, withHeader bool) string {
	var b strings.Builder
	if withHeader && len(header) > 0 {
		b.WriteString(strings.Join(header, "\t"))
		b.WriteByte('\n')
	}
	for _, row := range rows {
		b.WriteString(strings.Join(row, "\t"))
		b.WriteByte('\n')
	}
	return b.String()
}

// JSONArray 把一组值渲染成 JSON 数组。
//
// 单个元素无法序列化时降级为它的字符串形式,而不是让整个输出失败或静默丢值。
// 逐个 Marshal 而不是 Marsahl 整个切片,还有个副作用好处:
// 一个坏元素不会带走其余好的元素(Review Focus 第 3 条)。
func JSONArray(items []any) (string, error) {
	parts := make([]string, 0, len(items))
	for _, it := range items {
		b, err := json.Marshal(it)
		if err != nil {
			// 降级:用其字符串形式占位,保证条数与顺序不变。
			if b, err = json.Marshal(fmt.Sprint(it)); err != nil {
				return "", err
			}
		}
		parts = append(parts, string(b))
	}
	return "[" + strings.Join(parts, ",") + "]", nil
}
