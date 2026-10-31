package builtins

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"winbash/internal/errs"
	"winbash/internal/winmgmt"
)

// defaultPathExt 是没有 PATHEXT 时的可执行扩展名顺序(与 cmd.exe 的默认值一致)。
const defaultPathExt = ".COM;.EXE;.BAT;.CMD"

// Which 在给定的搜索路径里定位一个可执行文件,返回它的绝对位置。
//
// 刻意**不用** `exec.LookPath`:后者只认进程 PATH,而我们构造的搜索路径
// (Git Bash 的 usr\bin + 真实 PATH)不写回环境。
// (这个函数从 shell 包搬到这里:它是 ① 层 `which` 命令的核心,且与 shell 的会话状态无关。)
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

// findExecutable 依次尝试:自带扩展名的写法、PATHEXT 拼出来的名字、无扩展名的 shebang 脚本。
//
// PATHEXT 那条跟 cmd.exe 一致:PATH 目录里同名的普通文件(README、许可证之类)
// 不该被当成命令,那会让 ③ 层"找到一个跑不起来的东西"。
//
// 无扩展名那条是本轮补的:Git Bash 的 usr\bin 里有 **44 个**这种脚本
// (ldd、bzgrep、c_rehash、bzless …),它们靠 shebang 说明自己是什么。
// 判据就是头两个字节是 `#!` —— 比"在 PATH 里就当命令"稳得多。
// (找到只是第一步,执行它们还得靠 Git Bash,见 shell.runExternal。)
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

	if isScript(base) {
		return base, true
	}
	return "", false
}

// WhichAll 在搜索路径里找出**所有**匹配位置(按 PATH 顺序),给 `which -a` 用。
//
// 与 Which 的差别只有一处:不提前返回。自带路径的写法(含 / 或 \)仍只有一处 ——
// 它不是"在 PATH 里搜",GNU which 对那种写法也只给一行。
func WhichAll(cmd, searchPath string) []string {
	if cmd == "" {
		return nil
	}
	exts := pathExts()
	if strings.ContainsAny(cmd, `/\`) {
		if p, ok := findExecutable(cmd, exts); ok {
			return []string{p}
		}
		return nil
	}
	var hits []string
	for _, dir := range filepath.SplitList(searchPath) {
		if dir == "" {
			continue
		}
		if p, ok := findExecutable(filepath.Join(dir, cmd), exts); ok {
			hits = append(hits, p)
		}
	}
	return hits
}

// isScript 报告 p 是不是一个以 shebang 开头的文件。
//
// 只读头两个字节,所以再大的文件也不会被拖进来;文件不存在、读不出来、
// 或者比两个字节还短,一律当"不是脚本"(那种情况下去执行它本来也没意义)。
func isScript(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()

	var head [2]byte
	if _, err := io.ReadFull(f, head[:]); err != nil {
		return false
	}
	return head[0] == '#' && head[1] == '!'
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

// searchPathKey 是 searchPath 在 ctx 里的键。
//
// 为什么走 ctx 而不是给 Builtin 加参数:Registry 里只有一种形状是它的价值所在
// (见 registry.go 的说明)。`which` 需要搜索路径,而搜索路径是**会话**的属性 ——
// 用 ctx 传一个值,既不动形状,也不会让别的命令凭空"看见"一个它们不该依赖的东西。
type searchPathKey struct{}

// WithSearchPath 把会话的搜索路径放进 ctx,供 ① 层命令读取。
func WithSearchPath(ctx context.Context, sp string) context.Context {
	return context.WithValue(ctx, searchPathKey{}, sp)
}

// SearchPathFrom 取出 ctx 里的搜索路径(没有就是空串)。
func SearchPathFrom(ctx context.Context) string {
	sp, _ := ctx.Value(searchPathKey{}).(string)
	return sp
}

// WhichCmd 是 ① 层的 `which`:回答「这个名字在 winbash 里会落到哪里」。
//
// 它必须认识**四层**,否则就自相矛盾:
//   - ① 层(spec §5.2 的清单里本来就有 which)与 ② 层:输出名字本身。
//     这与 bash 的 `command -v` 对内建的做法一致 —— 它们没有文件路径可言。
//   - ③ 层:输出绝对路径(搜索路径里找到的那个)。
//   - 都没有:报错并返回非零,而不是安静地什么都不打印。
//
// 存在的理由很具体:AI 常写 `command -v X >/dev/null && …` 做存在性判断,
// 而 mvdan/sh 的 `command -v` 只认进程 PATH 与它自己的内建表 ——
// 于是它对 `ls`(我们的内建)和 `sed`(只在 Git Bash 的 usr\bin 里)都会说"没有"。
func WhichCmd(ctx context.Context, cwd string, argv []string, out, errw io.Writer) int {
	// -a / --all:列出**所有**匹配位置,而不只是第一个。
	//
	// 为什么必须单独处理(而不是当未知参数拒掉):AI 判断「装了哪个版本」时常用
	// `which -a python`,而原先这里会把它当**名字**去搜,报一句"找不到: -a"、
	// 退出码 1 —— 但 stdout 里已经把首个位置打出来了。那份「既有输出又报错」的
	// 组合最容易被误读成"某个名字没找到"。
	//
	// 语义照 GNU which:
	//   - 每个名字的每一处匹配各打一行(内置/②层只有一处);
	//   - 一个都没找到时非零,否则 0(-a 与否都一样)。
	all := false
	layers := false
	// `--` 之后全是名字(照 POSIX 惯例)。必须**置位**而不只是空 case:空 case 只把
	// `--` 本身消费掉,后面的 -a 仍走 flag 分支、-x 仍被拒 —— 实测 `which -- -a`
	// 会报「需要至少一个名字」(exit 2)。照 ls 的 operandsOnly 写法,-- 之后的
	// 参数全部当名字;名字找不到时的行为照旧(报「找不到」、exit 1)。
	operandsOnly := false
	filtered := make([]string, 0, len(argv))
	for _, a := range argv {
		if operandsOnly {
			filtered = append(filtered, a)
			continue
		}
		switch a {
		case "-a", "--all":
			all = true
		case "--layers":
			// 逐层报告:这个名字在 ①②③ 层分别命中哪里、在 ③ 层的每个目录里找没找到。
			//
			// 为什么值得单独一个开关:默认输出与 GNU which 一致(命中给一条、找不到
			// 报错),「只报了 ffprobe 找不到」本身是**对的**语义 —— 但排查的人往往
			// 想知道的是「另一层是不是有、我的 PATH 到底搜了哪几个目录」。
			// 那件事没有 GNU 对应物,所以不塞进默认输出,单开一个开关。
			layers = true
		case "--":
			operandsOnly = true
		default:
			// 其它以 - 开头的参数照旧响亮拒绝 —— 不静默当名字(项目底线)。
			if strings.HasPrefix(a, "-") && a != "-" {
				fmt.Fprintln(errw, errs.Errorf("which", "不认识的参数: "+a,
					"① 层的 which 只支持 -a / --all;要看 GNU which 的完整行为请用 usr\\bin 里的 which"))
				return errs.ExitUsage
			}
			filtered = append(filtered, a)
		}
	}
	argv = filtered
	if len(argv) == 0 {
		fmt.Fprintln(errw, errs.Errorf("which", "需要至少一个名字", "用法: which [-a] 名字 [名字...]"))
		return errs.ExitUsage
	}

	searchPath := SearchPathFrom(ctx)
	code := errs.ExitOK

	for _, name := range argv {
		if name == "" {
			continue
		}
		// --layers 必须在 ①② 层的提前返回**之前**:那两处的语义是「命中就打印名字、
		// 结束」,而诊断要的是「每一层分别是什么结果」。放在后面会让 `which --layers ls`
		// 退化成普通的 `which ls`(实测踩过)。
		//
		// 自带路径的写法不受它影响:逐层搜索对「./x」这种写法没有意义,照旧走下面的
		// 解析 + 存在性判断。
		if layers && !strings.ContainsAny(name, `/\`) {
			if reportLayers(out, errw, name, searchPath) {
				continue
			}
			code = errs.ExitError
			continue
		}
		if IsBuiltin(name) {
			fmt.Fprintln(out, name)
			continue
		}
		if _, ok := winmgmt.Registry[name]; ok {
			fmt.Fprintln(out, name)
			continue
		}
		// 自带路径的写法(./x、/c/x、C:\x)要按**会话 cwd** 解析:Which 里 os.Stat 的
		// 基准是**进程** cwd,而 cwd 这个参数是会话目录 —— 两者通常不同。理由与
		// runExternal 里那段完全相同,两处必须一致:否则 `which ./t.sh` 说"找不到",
		// 而那条命令其实能跑 —— 我们的错误信息还叫人拿 which 自查,指路就成了坏的。
		target := name
		if strings.ContainsAny(target, `/\`) {
			target = ResolveShellPath(cwd, target)
			if !filepath.IsAbs(target) {
				target = filepath.Join(cwd, target)
			}
			if p, ok := Which(target, searchPath); ok {
				fmt.Fprintln(out, p)
				continue
			}
		} else if layers {
			// --layers:逐层报告,不打印"最终位置"也不报错 —— 它的产物是一份诊断,
			// 不是"这个命令在哪"。退出码仍照 GNU 语义:一处都没命中就 1。
			if reportLayers(out, errw, name, searchPath) {
				continue
			}
		} else if all {
			if hits := WhichAll(target, searchPath); len(hits) > 0 {
				for _, p := range hits {
					fmt.Fprintln(out, p)
				}
				continue
			}
		} else if p, ok := Which(target, searchPath); ok {
			fmt.Fprintln(out, p)
			continue
		}
		if !layers {
			fmt.Fprintln(errw, errs.Errorf("which", "找不到: "+name,
				"①/② 层的名字由 winbash 提供(不在 PATH 里),③ 层会搜 Git Bash 的 usr\\bin;确认拼写"))
		}
		// 与 `which` / `command -v` 的惯例一致:找不到就非零。用 1 而不是 127 ——
		// 这两个命令在真环境里报的是 1,而 AI 常拿退出码做存在性判断。
		code = errs.ExitError
	}
	return code
}

// reportLayers 打出「这个名字在每一层各命中什么」,返回是否至少命中一处。
//
// 输出形状固定、逐行一格,便于人读也便于 grep:
//
//	$ winbash -c 'which --layers ffprobe'
//	① 内建      未命中
//	② 内建      未命中
//	③ PATH      (14 个目录全部未命中)
//	结论        所有层都没有: ffprobe
//
// 为什么不并进默认输出:默认输出要与 GNU which 逐字一致(命中给一条、找不到报错),
// 而这个开关回答的是另一个问题 ——「为什么没落到」。两者混在一起会让默认输出分叉。
func reportLayers(out, errw io.Writer, name, searchPath string) bool {
	hit := false

	if IsBuiltin(name) {
		fmt.Fprintf(out, "① 内建      %s\n", name)
		hit = true
	} else {
		fmt.Fprintln(out, "① 内建      未命中")
	}

	if _, ok := winmgmt.Registry[name]; ok {
		fmt.Fprintf(out, "② 内建      %s\n", name)
		hit = true
	} else {
		fmt.Fprintln(out, "② 内建      未命中")
	}

	// ③ 层:逐目录报。搜索顺序必须与 Which 一致(filepath.SplitList,按 PATH 顺序),
	// 否则这份诊断会和真正的落点打架 —— 那比不诊断更坏。
	exts := pathExts()
	var dirs []string
	for _, d := range filepath.SplitList(searchPath) {
		if d != "" {
			dirs = append(dirs, d)
		}
	}
	missed := 0
	for _, d := range dirs {
		if p, ok := findExecutable(filepath.Join(d, name), exts); ok {
			fmt.Fprintf(out, "③ PATH      %s\n", p)
			hit = true
		} else {
			missed++
		}
	}
	switch {
	case len(dirs) == 0:
		fmt.Fprintln(errw, "③ PATH      搜索路径为空")
	case missed == len(dirs):
		fmt.Fprintf(out, "③ PATH      (%d 个目录全部未命中)\n", missed)
	default:
		fmt.Fprintf(out, "③ PATH      (其余 %d 个目录未命中)\n", missed)
	}

	if !hit {
		fmt.Fprintf(out, "结论        所有层都没有: %s\n", name)
	}
	return hit
}
