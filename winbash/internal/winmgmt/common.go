// Package winmgmt 是四层路由的 ② 层:Windows 管理命令。
//
// 这一层的**语法是我们的**(`svc list`、`reg get`、`ps`),干活的引擎是常驻 pwsh
// (Task 7 的 `internal/pwsh`)。这样做的理由(spec §4 决策 1/2):
//   - 使用者只需要记一套语法,不必在 bash 与 PowerShell 之间切换
//   - 命令名由我们定,从根上避开与 Unix 工具的重名冲突
//   - 输出形状由我们决定,「结构化」是默认而不是可选项
//
// 本文件是这一层共用的底座:pwsh 调用、JSON 解码、TSV/JSON 渲染、以及
// 「把外部输入安全地放进 PowerShell 脚本」的那一个函数。
package winmgmt

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"winbash/internal/output"
	"winbash/internal/pwsh"
)

// Cmd 是 ② 层命令的统一形状。
//
// 比 ① 层的 Builtin 多两个参数:常驻 pwsh 引擎,以及全局的 --json 开关。
// ② 层的输出默认是 TSV(固定列序),--json 时给类型化 JSON 数组。
type Cmd func(ctx context.Context, conn *pwsh.Conn, argv []string, jsonOut bool, out, errw io.Writer) int

// Registry 把 ② 层命令名映射到实现。
//
// 与 ① 层用同一套命名习惯:紧跟 bash 的命令名,不引入 `Get-Service` 这种词。
// 新增 ② 层命令时在这里加一行。
var Registry = map[string]Cmd{
	"svc": Svc,
	"reg": Reg,
	"ps":  Ps,
	"sys": Sys,
}

// runRecords 执行一段 PowerShell 脚本,把它返回的 JSON 解析成记录集。
//
// 脚本必须「把对象 return 出来」而不是写主机流 —— 写主机流会破坏帧协议
// (见 internal/pwsh/session.ps1 的约定),那边会明确报错而不是静默给垃圾。
func runRecords(ctx context.Context, conn *pwsh.Conn, script string) ([]map[string]any, error) {
	raw, err := conn.Run(ctx, script)
	if err != nil {
		return nil, err
	}
	return decodeRecords(raw)
}

// decodeRecords 把 pwsh 侧的 JSON 文本解析成记录集。
//
// 单条记录时 pwsh 给的是**对象**而不是数组(PowerShell 的输出流语义,
// 见 Task 7 的 R17)。这里把它包成单元素切片,让「条数不改变形状」这件事
// 在 ② 层的调用侧成立 —— 否则 `svc status X` 这类单条命令会走进另一条代码路径。
func decodeRecords(raw string) ([]map[string]any, error) {
	s := strings.TrimSpace(raw)
	if s == "" || s == "null" {
		return nil, nil
	}

	if strings.HasPrefix(s, "{") {
		var one map[string]any
		if err := json.Unmarshal([]byte(s), &one); err != nil {
			return nil, fmt.Errorf("② 层的返回值不是合法 JSON: %w", err)
		}
		return []map[string]any{one}, nil
	}

	var items []map[string]any
	if err := json.Unmarshal([]byte(s), &items); err != nil {
		return nil, fmt.Errorf("② 层的返回值不是 JSON 数组: %w", err)
	}
	return items, nil
}

// renderRecords 按**固定列序**把记录渲染成 TSV 或 JSON。
//
// 列序由调用方给出的 header 决定,不由数据决定:同一命令每次的输出列必须一致,
// 使用者才能写死第几列,而不是每轮重新推断(不固定列序的 TSV 等于没有结构)。
// JSON 模式同样只输出 header 里声明的字段,免得将来 pwsh 侧多给一列就把下游搞乱。
func renderRecords(records []map[string]any, header []string, withHeader, jsonOut bool, out io.Writer) error {
	if jsonOut {
		items := make([]any, 0, len(records))
		for _, r := range records {
			row := make(map[string]any, len(header))
			for _, k := range header {
				row[k] = r[k]
			}
			items = append(items, row)
		}
		s, err := output.JSONArray(items)
		if err != nil {
			return err
		}
		_, err = io.WriteString(out, s+"\n")
		return err
	}

	rows := make([][]string, 0, len(records))
	for _, r := range records {
		row := make([]string, 0, len(header))
		for _, k := range header {
			row = append(row, cellString(r[k]))
		}
		rows = append(rows, row)
	}
	_, err := io.WriteString(out, output.TSV(header, rows, withHeader))
	return err
}

// cellString 把一个值渲染成一个 TSV 单元格。
//
// 两处刻意的处理:
//   - 数字走 strconv 而不是 fmt.Sprint:JSON 解出来的是 float64,fmt.Sprint 在
//     大数/整数上会给出 "1e+06" 这类形态,而「值必须无歧义」是本项目的底线。
//   - 值里的 Tab/换行替换成空格:TSV 的结构(一行一条记录、Tab 分列)优先于极端值。
//     需要原值就走 --json(那边是 JSON 转义,不受此限)。这不是静默改写,是取舍,
//     而且服务名/注册表值里出现 Tab 本身就不是合法输入。
func cellString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return sanitizeCell(t)
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case json.Number:
		return t.String()
	case []any:
		// 多值类型(REG_MULTI_SZ)与二进制(REG_BINARY)在这里落地。
		// TSV 只有一列,装不下结构 —— 用空格连起来是取舍,需要完整结构就走 --json。
		parts := make([]string, 0, len(t))
		for _, e := range t {
			parts = append(parts, cellString(e))
		}
		return sanitizeCell(strings.Join(parts, " "))
	default:
		return sanitizeCell(fmt.Sprint(t))
	}
}

func sanitizeCell(s string) string {
	if !strings.ContainsAny(s, "\t\r\n") {
		return s
	}
	r := strings.NewReplacer("\t", " ", "\r", " ", "\n", " ")
	return r.Replace(s)
}

// psQuote 把任意文本变成 PowerShell 的单引号字面量。
//
// 单引号字符串里唯一需要处理的就是单引号本身(PowerShell 用两个单引号表示一个),
// 所以这一道既挡注入、又不会碰坏路径里的空格、中文和反斜杠。
// **② 层的所有外部输入都必须过这一道**:服务名、注册表路径、进程名都是使用者给的。
func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// stripEnginePrefix 去掉引擎错误开头的 "pwsh: "。
//
// ② 层的三行式已经写明了是哪个命令("winbash: svc"),「问题」那一行再叠一个引擎名,
// 读起来像两层前缀,而使用者要的是「服务名错了」这件事本身。
func stripEnginePrefix(msg string) string {
	return strings.TrimPrefix(msg, "pwsh: ")
}
