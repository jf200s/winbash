// Package output 负责把值渲染成 AI 可解析的形式。
//
// 本包存在的唯一理由是 spec §6 的「值保证」:
// 0xa → 10、STATE: 1 → Stopped、86,488 K → 86488064、无 ANSI 色码。
// 对 AI 来说,确定性的值比"好看的输出"重要得多 —— 人眼能忽略噪音,解析器不能。
//
// 关于值保证**在哪实现**:`0xa → 10` 与 `STATE: 1 → Stopped` 实际由 PowerShell 侧
// 的 `.ToString()` 完成(见 winmgmt/svc.go、reg.go),数据在源头就已经是干净的。
// 这里原先还留着两个 Go 侧函数做同样的事,但**生产链路从不调用它们** ——
// 只有它们自己的测试在调。那是一种负债:读代码的人会以为 Go 侧有一层兜底,
// 而实际上兜底不存在。既然实测值保证确实成立(语料与真机都验过),就删掉空转的那层。
//
// 唯一真正**需要**在这里兜的是 ANSI:色码是从 pwsh 的终端检测里漏出来的,
// 一旦走 ConPTY 这类路径就会直接进到数据里,而它没有任何"源头修复"可言。
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

// ansiRe 匹配 SGR 之类的 CSI 序列。pwsh 7 在终端下会自动上色,
// 那些字节对 AI 是纯噪音,而且会让同一份数据在有无终端时长得不一样。
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

// StripANSI 删除 ANSI 转义序列。
//
// 它是**链路里唯一**的色码兜底,由 sanitizeCell / cellText 在每个单元格上调用。
// 当前 pwsh 子进程拿的是 CREATE_NO_WINDOW,通常检测不到终端、不会上色 ——
// 但"通常"不是保证:换成 ConPTY 或换个 pwsh 版本,色码就会直接进到数据里。
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
