package shell

import (
	"os"
	"path/filepath"
	"strings"
)

// defaultPathExt 是没有 PATHEXT 时的可执行扩展名顺序(与 cmd.exe 的默认值一致)。
const defaultPathExt = ".COM;.EXE;.BAT;.CMD"

// gitBashUsrBinCandidates 是 Git Bash 的 usr\bin 可能所在的位置。
//
// 本机实测(spec 第 1 节):真 Git Bash 在 `C:\Program Files\Git\bin\bash.exe`,
// **不在 PATH 里**;而 PATH 里的 `bash` 指向的是损坏的 WSL relay。
// 所以这里的候选表不是"以防万一",是这台机器上的事实。
var gitBashUsrBinCandidates = []string{
	`C:\Program Files\Git\usr\bin`,
	`C:\Program Files (x86)\Git\usr\bin`,
}

// BuildSearchPath 在真实 PATH 之后追加 Git Bash 的 usr\bin(若存在)。
//
// 为什么要加它:sed / awk / xargs 这些是硬骨头,重写不划算,而 Git Bash 里
// 现成有一套(而且 AI 要跑 git,Git Bash 必然在这台机器上)。
//
// **返回新字符串,绝不改进程环境**:Global Constraints 明令不改 os.Environ() ——
// 从 winbash 里启动的其他进程不该被我们塞进 Git Bash 的路径。
// 这个函数的全部作用域就是「给 Which 用」。
func BuildSearchPath(realPath string) string {
	extra := gitBashUsrBin()
	if extra == "" {
		return realPath
	}

	// 已经在 PATH 里就不追加。重复追加会让搜索路径每调用一次长一截。
	for _, dir := range filepath.SplitList(realPath) {
		if dir == "" {
			continue
		}
		if strings.EqualFold(filepath.Clean(dir), filepath.Clean(extra)) {
			return realPath
		}
	}

	if realPath == "" {
		return extra
	}
	return realPath + string(filepath.ListSeparator) + extra
}

// gitBashUsrBin 返回存在的 Git Bash usr\bin,没有则返回空串。
func gitBashUsrBin() string {
	for _, dir := range gitBashUsrBinCandidates {
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			return dir
		}
	}
	return ""
}

// Which 在给定的搜索路径里定位一个可执行文件,返回它的绝对位置。
//
// 刻意**不用** exec.LookPath:后者只认进程 PATH,而我们构造的搜索路径
// (真实 PATH + Git Bash 的 usr\bin)不写回环境。这一条是 R3 决定的核心 ——
// ③ 层能看见的工具集由这里决定,与进程环境无关。
func Which(cmd, searchPath string) (string, bool) {
	if cmd == "" {
		return "", false
	}
	exts := pathExts()

	// 自带路径的写法:只查它本身,不去 PATH 里找(与 bash 的语义一致)。
	if strings.ContainsAny(cmd, `/\`) {
		return findExecutable(cmd, exts)
	}

	for _, dir := range filepath.SplitList(searchPath) {
		if dir == "" {
			continue
		}
		if p, ok := findExecutable(filepath.Join(dir, cmd), exts); ok {
			return p, true
		}
	}
	return "", false
}

// findExecutable 检查 base 本身或 base+<扩展名> 是否存在。
//
// 只认 PATHEXT 里列出的扩展名(跟 cmd.exe 一致):PATH 目录里同名的普通文件
// (README、许可证之类)不该被当成命令,那会让 ③ 层"找到一个跑不起来的东西"。
// 自带扩展名的写法(probe-tool.exe)则只查它本身,不去拼第二个扩展名。
func findExecutable(base string, exts []string) (string, bool) {
	if filepath.Ext(base) != "" {
		if st, err := os.Stat(base); err == nil && !st.IsDir() {
			return base, true
		}
		return "", false
	}

	for _, e := range exts {
		p := base + e
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, true
		}
	}
	return "", false
}

// pathExts 返回可执行扩展名列表(小写、带点)。读不到 PATHEXT 就用默认值。
func pathExts() []string {
	raw := os.Getenv("PATHEXT")
	if raw == "" {
		raw = defaultPathExt
	}
	var out []string
	for _, e := range strings.Split(raw, ";") {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if !strings.HasPrefix(e, ".") {
			e = "." + e
		}
		out = append(out, strings.ToLower(e))
	}
	if len(out) == 0 {
		return strings.Split(strings.ToLower(defaultPathExt), ";")
	}
	return out
}
