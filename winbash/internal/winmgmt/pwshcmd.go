package winmgmt

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"winbash/internal/errs"
	"winbash/internal/output"
	"winbash/internal/pwsh"
)

// 这个文件让 `pwsh` 这个命令名走 ② 层的常驻引擎,而不是 ③ 层每次都新起一个
// pwsh.exe(本机实测 351ms 对 ~26ms)。
//
// 它来自 pwshx 那个项目的残余价值:winbash 的 ② 层本来就守着一个常驻 pwsh,
// 只是原先只暴露了 svc/reg/ps/sys 四个封装命令,跑不了任意 PowerShell。
// 与其再维护一个 daemon,不如把这条路接到已有的引擎上 —— 一处实现,和四层路由一致。
//
// 为什么是"覆盖"而不是新名字(比如 pwshx):AI 已经会写 `pwsh -Command '...'`,
// 让它自动变快,比让它学一个新名字有用。代价是必须与原生 pwsh 语义一致 ——
// 尤其是位置参数按**文件路径**解释,不是"一段脚本文本"。
//
// 为什么不接管的形式**报错**而不是静默回落原生 pwsh:两者差 20 倍启动成本,
// 悄悄换一种执行方式会让人以为"反正都一样"。不知道的事就该说出来。

// envKey 是 ① 层环境变量的 context 键。
//
// 走 context 而不是改 Cmd 的签名:只有这一个命令需要环境,没必要让
// svc/reg/ps/sys 都多接一个用不上的参数。
type envKey struct{}

// WithEnv 把 ① 层当前的环境变量(KEY=VALUE 列表)附到 ctx 上。
func WithEnv(ctx context.Context, env []string) context.Context {
	return context.WithValue(ctx, envKey{}, env)
}

// envFrom 取出 WithEnv 附上的环境变量;没有就返回 nil。
func envFrom(ctx context.Context) []string {
	env, _ := ctx.Value(envKey{}).([]string)
	return env
}

const pwshUsage = "用法: pwsh -c '<脚本>' | pwsh -Command '<脚本>' | pwsh <脚本文件.ps1>"

// Pwsh 执行任意 PowerShell 脚本,走 ② 层的常驻引擎。
//
// 输出形状与其它 ② 层命令一致:默认 TSV(列名来自数据本身,见 renderAny),
// --json 给类型化数组。
func Pwsh(ctx context.Context, conn *pwsh.Conn, argv []string, jsonOut bool, out, errw io.Writer) int {
	var (
		script     string // -c / -Command 给的脚本文本
		file       string // -File 或位置参数给的文件
		withHeader bool
	)

	for i := 0; i < len(argv); i++ {
		a := argv[i]

		switch {
		case a == "--header":
			withHeader = true

		case isHarmlessSwitch(a):
			// 恒真的启动开关(引擎本来无 profile、非交互),接受但不做事。
			// AI 顺手写 `pwsh -NoProfile -Command '...'` 是常态,不该为此报错。

		case isCommandSwitch(a):
			if i+1 >= len(argv) {
				fmt.Fprintln(errw, errs.Errorf("pwsh", a+" 后面要给脚本", pwshUsage))
				return errs.ExitUsage
			}
			i++
			if script != "" || file != "" {
				fmt.Fprintln(errw, errs.Errorf("pwsh", "脚本只能给一次", pwshUsage))
				return errs.ExitUsage
			}
			script = argv[i]

		case a == "-File":
			if i+1 >= len(argv) {
				fmt.Fprintln(errw, errs.Errorf("pwsh", "-File 后面要给脚本文件路径", pwshUsage))
				return errs.ExitUsage
			}
			i++
			if script != "" || file != "" {
				fmt.Fprintln(errw, errs.Errorf("pwsh", "脚本只能给一次", pwshUsage))
				return errs.ExitUsage
			}
			file = argv[i]

		case strings.HasPrefix(a, "-"):
			// 不接管的形式(-EncodedCommand、-Version、-Command:xxx 等)。
			// 报错并指路,不悄悄换成原生 pwsh。
			fmt.Fprintln(errw, errs.Errorf("pwsh", "不接管这个参数: "+a, unsupportedHint()))
			return errs.ExitUsage

		default:
			// 位置参数 = 脚本文件路径,和原生 pwsh 一致(原生 `pwsh x.ps1` 就是跑文件)。
			// 这里若当"脚本文本"解释,`pwsh script.ps1` 的行为就与原生不同了。
			if script != "" || file != "" {
				fmt.Fprintln(errw, errs.Errorf("pwsh", "脚本只能给一次", pwshUsage))
				return errs.ExitUsage
			}
			file = a
		}
	}

	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			// 最常见的误用是"把一段脚本当位置参数给了"。所以报错里直接点出 -Command,
			// 而不是丢一句"系统找不到指定的文件"让人自己去想。
			fmt.Fprintln(errw, errs.Errorf("pwsh", "读不到脚本文件 "+file+" : "+err.Error(),
				"要执行一段脚本文本请用 pwsh -Command '<脚本>'"))
			return errs.ExitError
		}
		script = string(b)
	}

	if strings.TrimSpace(script) == "" {
		fmt.Fprintln(errw, errs.Errorf("pwsh", "没有脚本可执行(winbash 不支持交互式 shell)",
			"交互式请用原生 pwsh 的全路径(where.exe pwsh 可查);"+pwshUsage))
		return errs.ExitUsage
	}

	// 环境前导必须在脚本之前:脚本里可能立刻就读 $env:。
	raw, err := conn.Run(ctx, envPrelude(envFrom(ctx))+script)
	if err != nil {
		fmt.Fprintln(errw, errs.Errorf("pwsh", stripEnginePrefix(err.Error()),
			"这段脚本在常驻 pwsh 里失败了;要看原生的报错可以用 pwsh 的全路径调它"))
		return errs.ExitError
	}
	if err := renderAny(raw, withHeader, jsonOut, out); err != nil {
		fmt.Fprintln(errw, errs.Errorf("pwsh", err.Error(), "写输出时出错"))
		return errs.ExitError
	}
	return errs.ExitOK
}

// isHarmlessSwitch 判断一个开关是否"恒真、可以接受但不做事"。
func isHarmlessSwitch(a string) bool {
	switch strings.ToLower(a) {
	case "-noprofile", "-nologo", "-noninteractive":
		return true
	}
	return false
}

// isCommandSwitch 判断这个开关是不是"后面跟一段脚本文本"。
//
// 不带 -Command:xxx 那种冒号写法:它罕见,而且一旦支持就要处理参数里带冒号的
// 各种转义,收益远不抵复杂度 —— 报错更诚实。
func isCommandSwitch(a string) bool {
	switch strings.ToLower(a) {
	case "-c", "-command":
		return true
	}
	return false
}

// unsupportedHint 告诉使用者"要用原生 pwsh"该怎么走。
//
// 为什么这里不给具体路径:这个包不能导入 builtins —— 那边(which.go)反过来要
// 依赖这个包的 Registry 才能认识四层,互相依赖就成环了。所以查不了 PATH,
// 只能指一条**使用者自己能查**的路。where.exe 在 ③ 层(真实 PATH),一定能用;
// 写死一条 `C:\Program Files\PowerShell\7\pwsh.exe` 反而会在别的机器上过期。
func unsupportedHint() string {
	return "要跑这个形式请用原生 pwsh 的全路径(路径可用 where.exe pwsh 查);" +
		"winbash 只接管 -c / -Command / 脚本文件"
}

// envPrelude 把 ① 层当前的环境变量写进常驻引擎。
//
// 为什么需要:引擎是在第一次用到 ② 层时启动的,继承的是**那一刻**的环境;
// 而 ① 层的 export 发生在那之后。不显式同步,`export X=1; pwsh -c '$env:X'`
// 会静默给出空值 —— 透明覆盖最不能有的就是这种"看起来对"。
//
// 为什么用 [Environment]::SetEnvironmentVariable 而不是 `$env:NAME = ...`:
// Windows 的环境变量名不限于合法标识符(可以带括号、连字符),走 .NET API
// 就不会因为名字里的字符而解析失败。
func envPrelude(env []string) string {
	if len(env) == 0 {
		return ""
	}
	var b strings.Builder
	for _, kv := range env {
		i := strings.IndexByte(kv, '=')
		// i <= 0 同时挡掉两种:空名,以及 Windows 上 `=C:` 这类内部项
		// (它们以 = 开头,既设不进去也不该设)。
		if i <= 0 {
			continue
		}
		b.WriteString("[Environment]::SetEnvironmentVariable(")
		b.WriteString(psQuote(kv[:i]))
		b.WriteByte(',')
		b.WriteString(psQuote(kv[i+1:]))
		b.WriteString(");")
	}
	return b.String()
}

// renderAny 渲染"形状不定"的输出。
//
// 这是透传命令与其它 ② 层命令的根本差别:svc/reg/ps/sys 知道自己的列(固定列序),
// 而这里列来自数据本身。规则如下,都是为了让下一个读它的人不必猜:
//
//	空 / null          什么都不输出
//	单个标量           就一行,裸的值(不套列名 —— 套了反而像表格)
//	对象(单条或数组)  表格,列 = 所有键的并集,按字母序固定
//	标量数组           每行一个,列名 Value
//	混合数组           进表格,标量进 Value 列(免得出现缺列的行)
func renderAny(raw string, withHeader, jsonOut bool, out io.Writer) error {
	v, err := decodeAny(raw)
	if err != nil {
		return err
	}

	if jsonOut {
		items := []any{}
		switch t := v.(type) {
		case []any:
			items = t
		case nil:
			// 空结果给 []:形状不随数据变化,消费端不用先判断类型。
		default:
			items = []any{t}
		}
		s, err := output.JSONArray(items)
		if err != nil {
			return err
		}
		_, err = io.WriteString(out, s+"\n")
		return pipeClosedIsOK(err)
	}

	rows, header := tableOf(v)
	if len(rows) == 0 {
		return nil
	}
	if header == nil {
		_, err := io.WriteString(out, rows[0][0]+"\n")
		return pipeClosedIsOK(err)
	}
	_, err = io.WriteString(out, output.TSV(header, rows, withHeader))
	return pipeClosedIsOK(err)
}

// tableOf 把任意形状摊成 (行, 列名);列名为 nil 表示"这是单个标量,不是表"。
//
// 列名为什么排序而不是保持 PowerShell 的属性顺序:Go 的 map 迭代顺序是随机的,
// 而「同一命令每次输出列必须一致」在 ② 层是不让步的(见 renderRecords 的注释)。
// 要保属性顺序就得自己流式解析 JSON 的键序,收益(好看)不抵复杂度。
func tableOf(v any) (rows [][]string, header []string) {
	switch t := v.(type) {
	case nil:
		return nil, nil

	case []any:
		if len(t) == 0 {
			return nil, nil
		}
		if !hasAnyMap(t) {
			rows = make([][]string, 0, len(t))
			for _, item := range t {
				rows = append(rows, []string{cellText(item)})
			}
			return rows, []string{"Value"}
		}
		// 规范化:标量项包成 {"Value": ...},这样每一行都有同样的列可填。
		items := make([]map[string]any, 0, len(t))
		keys := map[string]bool{}
		for _, item := range t {
			m, ok := item.(map[string]any)
			if !ok {
				m = map[string]any{"Value": item}
			}
			items = append(items, m)
			for k := range m {
				keys[k] = true
			}
		}
		header = sortedKeys(keys)
		rows = make([][]string, 0, len(items))
		for _, m := range items {
			row := make([]string, 0, len(header))
			for _, k := range header {
				row = append(row, cellText(m[k]))
			}
			rows = append(rows, row)
		}
		return rows, header

	case map[string]any:
		keys := map[string]bool{}
		for k := range t {
			keys[k] = true
		}
		header = sortedKeys(keys)
		row := make([]string, 0, len(header))
		for _, k := range header {
			row = append(row, cellText(t[k]))
		}
		return [][]string{row}, header

	default:
		return [][]string{{cellText(v)}}, nil
	}
}

func hasAnyMap(items []any) bool {
	for _, item := range items {
		if _, ok := item.(map[string]any); ok {
			return true
		}
	}
	return false
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// cellText 把一个 JSON 值变成一个 TSV 单元格。
//
// 数字必须原样:走 json.Number 而不是 float64,否则大整数会丢精度、整数会写成浮点形态。
// 布尔用 PowerShell 的 True/False(值是从那边来的,形态也该像那边)。
func cellText(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case json.Number:
		return t.String()
	case string:
		return sanitizeCell(t)
	case bool:
		if t {
			return "True"
		}
		return "False"
	default:
		// 嵌套对象/数组:给紧凑 JSON。TSV 单元格里没法更好看,但信息不丢。
		b, err := json.Marshal(t)
		if err != nil {
			return sanitizeCell(fmt.Sprint(t))
		}
		return sanitizeCell(string(b))
	}
}

// decodeAny 解析 ② 层给的 JSON,保留数字的原始写法。
func decodeAny(raw string) (any, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, nil
	}
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("② 层的返回值不是合法 JSON: %w", err)
	}
	return v, nil
}
