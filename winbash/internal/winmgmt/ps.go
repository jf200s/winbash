package winmgmt

import (
	"context"
	"fmt"
	"io"
	"strings"

	"winbash/internal/errs"
	"winbash/internal/pwsh"
)

// psColumns 是 `ps` 的**固定列序**。列名用 PID(而不是 Id):使用者脑子里
// 想的就是 PID,而 `Id` 在 PowerShell 里是另一个意思。
var psColumns = []string{"PID", "Name", "MemoryBytes"}

const psUsage = "用法: ps [名称或PID] [--header]"

// psProjection 把进程对象压成三列。
//
// WorkingSet64 是 int64 原生值,直接交给 ConvertTo-Json 就是 JSON 数字 ——
// 中间不做任何格式化操作,`86,488 K` 那种带千分位与单位的字符串因此无从产生
// (spec §6 点名要消灭的形态之一)。
const psProjection = " | ForEach-Object { [ordered]@{ PID = $_.Id; Name = [string]$_.ProcessName; MemoryBytes = [int64]$_.WorkingSet64 } }"

// Ps 列出进程。
//
// 名字沿用 bash 的 `ps` 而不是新造 `procs`:它与 bash 的 ps 语义本来就吻合
// (列出进程),AIN 不必多记一个词。无参数列全部;给参数则按名称(子串、
// 不区分大小写)或 PID(纯数字、精确)过滤。
func Ps(ctx context.Context, conn *pwsh.Conn, argv []string, jsonOut bool, out, errw io.Writer) int {
	var (
		pos        []string
		withHeader bool
	)
	for _, a := range argv {
		switch {
		case a == "--header":
			withHeader = true
		case strings.HasPrefix(a, "--"):
			fmt.Fprintln(errw, errs.Errorf("ps", "不认识的参数: "+a, psUsage))
			return errs.ExitUsage
		default:
			pos = append(pos, a)
		}
	}
	if len(pos) > 1 {
		fmt.Fprintln(errw, errs.Errorf("ps", "最多给一个过滤条件(名称或 PID)", psUsage))
		return errs.ExitUsage
	}

	var script string
	switch {
	case len(pos) == 0:
		script = "Get-Process" + psProjection

	case isAllDigits(pos[0]):
		// 纯数字 = PID 精确匹配。找不到就**空列表**,不是错误 ——
		// ps 的语义是「列表」,遍历一批 pid 时打错一个不该让整条命令失败。
		script = "Get-Process -Id " + pos[0] + " -ErrorAction SilentlyContinue" + psProjection

	default:
		// 名称按子串匹配、不区分大小写(Windows 的进程名本身不区分大小写)。
		// 刻意不用 -like:那是通配符匹配,使用者得知道要转义 `[`、`*` 这些字符,
		// 而他给的只是一个名字。
		script = "$__wb_f = " + psQuote(strings.ToLower(pos[0])) + `
Get-Process | Where-Object { ([string]$_.ProcessName).ToLower().Contains($__wb_f) }` + psProjection
	}

	// 排序是为了确定性:同一次查询两次运行必须给出同样的行序,
	// 否则 `ps | grep x` 的比对结果会时对时错。
	script += " | Sort-Object -Property PID"

	records, err := runRecords(ctx, conn, script)
	if err != nil {
		fmt.Fprintln(errw, errs.Errorf("ps", stripEnginePrefix(err.Error()),
			"确认过滤条件: 纯数字按 PID 精确匹配,其它按进程名子串匹配(例如 ps pwsh)"))
		return errs.ExitError
	}
	if err := renderRecords(records, psColumns, withHeader, jsonOut, out); err != nil {
		fmt.Fprintln(errw, errs.Errorf("ps", err.Error(), "写输出时出错"))
		return errs.ExitError
	}
	return errs.ExitOK
}

// isAllDigits 判断一个参数是否该按 PID 解释。
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
