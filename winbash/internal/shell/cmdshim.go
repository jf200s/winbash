package shell

import (
	"os"
	"path/filepath"
	"strings"
)

// 这个文件解决一件事:Windows 上大量工具的入口是 `.cmd` 垫片(npm / npx / pnpm /
// tsc / 各种 node CLI),而 CreateProcess 只能启动 PE 映像、Git Bash 的 env.exe 只能
// 读 shebang —— 两者都跑不了批处理。修之前 winbash 对它们是**响亮拒绝**并提示
// `cmd /c`:语义没错,但把整个 Node 生态的默认入口挡在门外 —— AI 每写一条
// `npm run build` 都得多包一层,而多包的那层引号正是最容易出错的地方。
//
// 做法不是「统统交给 cmd /c」:那会让 `&` `|` `^` `%VAR%` 经 cmd 的解析器再解释
// 一遍,与项目「不静默做错事」的底线冲突。这里是**认出 npm 系垫片的形状**,把
// 「它在调哪个 node 脚本」直接翻译成一次 exec —— 使用者传来的参数一个字节都不
// 经过 cmd。
//
// 认不出来的一律保持原来的拒绝(不猜);兜底开关默认关(见 envCmdShellFallback)。

// shimCall 是一次翻译的结果。
type shimCall struct {
	Prog string   // 可执行文件绝对路径
	Args []string // 垫片里写死、必须排在前面的参数(脚本路径等)
}

// resolveNodeShim 尝试把一个 .cmd / .bat 垫片翻译成直接 exec。
//
// searchPath 用来解析裸 `node`(垫片里的 ELSE 分支就是这个)。
// ok=false 表示「不是我们能安全翻译的形状」,调用方必须保持原样拒绝。
func resolveNodeShim(shimPath, searchPath string) (shimCall, bool) {
	raw, err := os.ReadFile(shimPath)
	if err != nil {
		return shimCall{}, false
	}
	body := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if strings.ContainsRune(body, 0) {
		return shimCall{}, false
	}
	dir := filepath.Dir(shimPath)

	vars := collectShimVars(body)
	line, ok := lastExecLine(body)
	if !ok {
		return shimCall{}, false
	}
	toks, outside, ok := splitShimLine(line)
	if !ok || !safeOutside(outside) || len(toks) == 0 {
		return shimCall{}, false
	}

	prog, ok := pickProgram(candsFor(toks[0], vars, dir), searchPath)
	if !ok {
		return shimCall{}, false
	}

	// 其余 token 都是垫片写死的参数(脚本路径)。允许为空(有的垫片直接 exec 一个 exe)。
	var scriptArgs []string
	for _, tok := range toks[1:] {
		p, ok := pickScript(candsFor(tok, vars, dir))
		if !ok {
			return shimCall{}, false
		}
		scriptArgs = append(scriptArgs, p)
	}

	return shimCall{Prog: prog, Args: scriptArgs}, true
}

// ── 变量 ────────────────────────────────────────────────────────────────

// collectShimVars 收集垫片里的简单 SET 赋值,按出现顺序保留全部候选值。
//
// 只认 `SET "NAME=VALUE"` 与 `SET NAME=VALUE`。同一个名字可能被赋值多次(典型是
// IF EXIST 的两个分支),全部留着 —— 哪个值成立由 pickProgram / pickScript 判,
// 这里不猜。
//
// `FOR /F … DO (SET …)` 里的赋值展开不出来(值来自运行时子进程),于是 npm 垫片的
// `%NPM_PREFIX_NPM_CLI_JS%` 永远没有候选值 —— 这没关系:同一个变量的第一个候选
// 就是字面路径,已经够用;若哪天它不存在,翻译会失败而不是猜。
func collectShimVars(body string) map[string][]string {
	vars := map[string][]string{}
	for _, ln := range strings.Split(body, "\n") {
		t := strings.TrimSpace(ln)
		if !hasPrefixFold(t, "SET ") {
			continue
		}
		rest := strings.TrimSpace(t[4:])
		if strings.HasPrefix(rest, "/") { // SET /A、SET /P
			continue
		}
		var kv string
		if strings.HasPrefix(rest, `"`) {
			end := strings.Index(rest[1:], `"`)
			if end < 0 {
				continue
			}
			kv = rest[1 : 1+end]
		} else {
			kv = rest
		}
		name, val, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		// 名字里出现非标识符字符说明不是普通赋值(如 `SET "%~1=..."`)。
		if name == "" || strings.ContainsAny(name, ` %"'/\`) {
			continue
		}
		vars[strings.ToUpper(name)] = append(vars[strings.ToUpper(name)], strings.TrimSpace(val))
	}
	return vars
}

// maxExpandDepth 挡住变量互相引用形成的环。
const maxExpandDepth = 4

// candsFor 把一个 token 展开成候选字符串(去重后保序)。
//
// 展开规则:
//   - `%~dp0` / `%dp0%` → 垫片所在目录
//   - `%NAME%`          → 该变量的每个候选值(递归,带上限)
//
// **某个取值路径上遇到未知变量时,只作废那一条候选**,不影响其它候选 —— 这正是
// npm 垫片需要的:同一个变量既可能被赋成字面路径,也可能被赋成依赖 FOR 变量的
// 表达式,后者展开不出来就丢掉。
func candsFor(tok string, vars map[string][]string, dir string) []string {
	seen := map[string]bool{}
	var out []string
	var walk func(s string, depth int) []string
	walk = func(s string, depth int) []string {
		if depth > maxExpandDepth {
			return nil // 变量互相引用成环
		}
		// 空串是**合法**的“无后缀”,不是失败 —— 否则 `%NODE_EXE%`(整个 token 就是
		// 一个变量)会因为 rest 为空而整条作废。这个区分踩过一次:所有候选都变 nil,
		// 于是每一条 shim 都翻译失败,而失败又被上层当成“不认识的形状”。
		if s == "" {
			return []string{""}
		}
		i := strings.IndexByte(s, '%')
		if i < 0 {
			return []string{s}
		}
		// `%~dp0` 这种带修饰符的形式:先认它,再走普通变量。
		if hasPrefixFold(s[i:], "%~dp0") {
			return joinCands(dir, walk(s[i+5:], depth+1))
		}
		end := strings.IndexByte(s[i+1:], '%')
		if end < 0 {
			return nil // 落单的 % :不可信
		}
		name := s[i+1 : i+1+end]
		rest := s[i+2+end:]
		if n := strings.ToUpper(name); n == "DP0" {
			return joinCands(dir, walk(rest, depth+1))
		}
		vals, ok := vars[strings.ToUpper(name)]
		if !ok {
			return nil
		}
		var res []string
		for _, v := range vals {
			// 变量值本身可能还带变量(npm 的 NPM_CLI_JS 就有 `%NPM_PREFIX_…%` 这一支),
			// 所以拼回去之后要**再走一遍** walk —— 直接拼会留下未展开的 %VAR%。
			res = append(res, walk(v+rest, depth+1)...)
		}
		return res
	}
	for _, c := range walk(tok, 0) {
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out
}

// joinCands 把前缀 prefix 拼到每条后缀上。
func joinCands(prefix string, suffixes []string) []string {
	out := make([]string, 0, len(suffixes))
	for _, s := range suffixes {
		out = append(out, prefix+s)
	}
	return out
}

// ── 行定位与分词 ────────────────────────────────────────────────────────

// lastExecLine 找垫片里**真正执行外部程序**的那一行。
//
// 认的形态(@ECHO/GOTO/SETLOCAL/CALL/IF 是 npm 与 pnpm 共有的脚手架):
//
//	"%dp0%\node_modules\pkg\bin\x.js" %*
//	endLocal & goto #_undefined_# 2>NUL || title %COMSPEC% & "%_prog%" "%dp0%\…" %*
//	"%dp0%\node_modules\@scope\pkg\bin\x.exe"   %*
//
// 判据是「这一行以 `%*` 结尾」,不按行号也不按固定前缀 —— 脚手架随 npm/pnpm
// 版本改过好几轮,钉形状比钉位置稳。多条命中时取最后一条(脚手架里的模板行在前)。
func lastExecLine(body string) (string, bool) {
	var found string
	for _, ln := range strings.Split(body, "\n") {
		t := strings.TrimSpace(ln)
		if t == "" || strings.HasPrefix(t, "::") || hasPrefixFold(t, "REM ") {
			continue
		}
		if !strings.HasSuffix(t, "%*") {
			continue
		}
		found = t
	}
	return found, found != ""
}

// splitShimLine 把一行切成「引号内的 token」与「引号外的片段」。
//
// 引号外只允许脚手架(见 safeOutside),所以这里不处理引号外的裸 token。
func splitShimLine(line string) (toks []string, outside string, ok bool) {
	var out strings.Builder
	var cur strings.Builder
	inQuote := false
	for _, r := range line {
		switch r {
		case '"':
			if inQuote {
				toks = append(toks, cur.String())
				cur.Reset()
			}
			inQuote = !inQuote
			continue
		}
		if inQuote {
			cur.WriteRune(r)
		} else {
			out.WriteRune(r)
		}
	}
	if inQuote {
		return nil, "", false // 引号没闭合:内容不可信
	}
	return toks, out.String(), true
}

// safeOutside 判断引号之外的部分是否只由已知脚手架构成。
//
// 判据是**整段精确匹配**(先把连续空白归并),不是逐字符白名单 —— `foo & bar`
// 那种「看起来像脚手架」的写法必须被拒,而不是被放过。
func safeOutside(s string) bool {
	t := strings.Join(strings.Fields(s), " ")
	if t == "" || t == "%*" {
		return true
	}
	t = strings.TrimSpace(strings.TrimSuffix(t, "%*"))
	switch strings.TrimSpace(t) {
	case "",
		// npm 9/10 与 pnpm 的垫片收尾。
		"endLocal & goto #_undefined_# 2>NUL || title %COMSPEC% &":
		return true
	}
	return false
}

// ── 候选判定 ────────────────────────────────────────────────────────────

// pickProgram 从候选里挑出可执行程序:存在的绝对路径优先,其次裸 `node`。
func pickProgram(cands []string, searchPath string) (string, bool) {
	for _, c := range cands {
		if c == "" {
			continue
		}
		if filepath.IsAbs(c) {
			if st, err := os.Stat(c); err == nil && !st.IsDir() {
				return c, true
			}
			continue
		}
	}
	for _, c := range cands {
		if strings.EqualFold(strings.TrimSpace(c), "node") {
			if p, ok := whichOnPath("node.exe", searchPath); ok {
				return p, true
			}
		}
	}
	return "", false
}

// pickScript 从候选里挑出**唯一**确定的脚本参数。
//
// 只接受存在且不是目录的绝对路径;多个候选指向不同文件时拒绝 —— 那说明这条
// 垫片到运行时才决定跑哪个脚本,替它做主就会跑错东西。
func pickScript(cands []string) (string, bool) {
	var found string
	for _, c := range cands {
		if c == "" || !filepath.IsAbs(c) {
			continue
		}
		if st, err := os.Stat(c); err != nil || st.IsDir() {
			continue
		}
		if found == "" {
			found = c
			continue
		}
		if !strings.EqualFold(found, c) {
			return "", false
		}
	}
	return found, found != ""
}

// whichOnPath 在给定搜索路径里找**具体的文件名**(带扩展名),不走 PATHEXT 猜。
func whichOnPath(name, searchPath string) (string, bool) {
	for _, d := range filepath.SplitList(searchPath) {
		if d == "" {
			continue
		}
		p := filepath.Join(d, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, true
		}
	}
	return "", false
}

// hasPrefixFold 是大小写不敏感的 HasPrefix。
func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// envCmdShellFallback 是「翻译不了就用 cmd /c」的开关名。
//
// 默认**关**:打开后使用者传来的参数会交给 cmd.exe 重新解析(`&` `|` `^` `%VAR%`
// 都会被解释),与「不静默做错事」冲突 —— 必须由使用者显式打开,不替人做主。
const envCmdShellFallback = "WINBASH_CMD_SHELL"

// cmdShellFallbackEnabled 报告环境里是否打开了兜底。
func cmdShellFallbackEnabled(getenv func(string) string) bool {
	switch strings.ToLower(strings.TrimSpace(getenv(envCmdShellFallback))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
