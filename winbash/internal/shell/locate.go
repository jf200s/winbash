package shell

import (
	"strings"
	"sync"

	"winbash/internal/builtins"
	"winbash/internal/winmgmt"
)

// layer 是四层路由里的一层,编号与 spec §4 保持一致,便于对着文档读代码。
type layer int

const (
	layerBuiltin layer = iota // ① 纯 Go 内建:ls / cat / grep …
	layerWinmgmt              // ② Go 内建,底层转常驻 pwsh:svc / reg / ps / sys / pwsh
	layerPath                 // ③ 真实 PATH(含 Git Bash 的 usr\bin)
	layerMissing              // ④ 都没有:报错 + 可操作建议
)

// locate 是 ③/④ 层命令定位的**单入口**:判层与找路径对同一条命令只扫一遍,
// (层, 绝对路径)一起返回,由调用方带走。
//
// 改动前的形状是同一条命令扫两遍:execHandler 判层调一次 Which,runExternal
// 拿路径又对同一个 searchPath 完整跑一遍 —— 每遍都是逐目录 ×(1+len(PATHEXT))
// 次 os.Stat,外加无扩展名命中的 isScript os.Open(本机实测:进程 PATH 47 目录、
// PATHEXT 11 项,searchPath 前置 usr\bin 后一次全表 miss ≈ 48×12 次 os.Stat)。
//
// 判定顺序就是优先级,而且 ①/② 命中后就不再去看进程 PATH —— 这是刻意的:
// `ls` 永远是我们的 ls,不是某个碰巧排在前面的 exe。Windows 上 find / sort / where
// 这类重名冲突,根源就是「谁排在 PATH 前面谁说话」;这里的名字由我们自己定义。
//
// ③ 与 ④ 的分界靠 Which:能定位到可执行文件就是 ③,否则是 ④(报错 + 建议)。
// searchPath 由调用方给出(真实 PATH + Git Bash 的 usr\bin),**不是**进程 PATH ——
// 进程环境在整个生命周期里都不被改动(Global Constraints)。
//
// 自带路径的写法(含 / 或 \)**不进负缓存**:Which 对它只做一次 stat,且基准是
// **进程 cwd**,而 runExternal 会按会话 cwd 重新解析后再定位 —— 这类写法的 ③/④
// 最终判定以 runExternal 为准,这里的结论只用来排除 ①/②。不缓存也是同一个原因:
// miss 与 cwd 相关,而缓存键里没有 cwd(见 locateMisses)。
func locate(cmd, searchPath string) (layer, string) {
	if _, ok := builtins.Registry[cmd]; ok {
		return layerBuiltin, ""
	}
	if _, ok := winmgmt.Registry[cmd]; ok {
		return layerWinmgmt, ""
	}
	if strings.ContainsAny(cmd, `/\`) {
		abs, ok := builtins.Which(cmd, searchPath)
		if ok {
			return layerPath, abs
		}
		return layerMissing, ""
	}
	if locateMissKnown(cmd, searchPath) {
		// 已知 miss:直接判 ④,完整 PATH 一眼都不扫。
		return layerMissing, ""
	}
	abs, ok := builtins.Which(cmd, searchPath)
	if !ok {
		locateMissRecord(cmd, searchPath)
		return layerMissing, ""
	}
	return layerPath, abs
}

// 负结果缓存(进程级):键 = 完整 searchPath 串,值 = 这条搜索路径下已知 miss 的
// 命令名集合。命中的 miss 直接判 ④ 层。
//
// 只缓存负结果:④ 层打错名必然扫完整条 searchPath,而打错名在真实使用里会**重复
// 发生**;命中型扫描停在第一个命中目录,通常几次 stat 就完,不值得为它冒陈旧风险。
//
// 正结果刻意**不**缓存。方向上 bash 的 hash 表是先例,但 hash 有 `hash -r` 作废
// 手段,这里没有任何作废钩子:文件被删/被同名新工具顶替后,缓存的旧绝对路径会把
// CreateProcess 指向不存在或非预期的文件,那种错误比「重扫一遍」难排查得多
// (同一条命令判层→执行的重复扫描已由 locate 合成掉,跨请求的重扫是命中型,代价小)。
//
// 盲区(已知差异与限制.md 有同名条目):**会话中途**往 PATH 目录里装了新工具,
// 之前 miss 过的名字会一直判 ④ 到进程结束 —— 刻意的取舍,不是 bug。键含完整
// searchPath:未来 PATH 动态化时改 PATH 即换键,缓存天然失效;会话语义专项设计里
// 的 export PATH 方案接的正是这一点。
//
// 不设上限与 msysBinaryCache 同一先例:键空间是「实际打错过」的名字,人打字量级,
// 撑不满一个常驻进程的内存。
var (
	locateMissMu sync.Mutex
	locateMisses = map[string]map[string]struct{}{}
)

// locateMissKnown 报告 (cmd, searchPath) 是否已记录为 miss。
func locateMissKnown(cmd, searchPath string) bool {
	locateMissMu.Lock()
	defer locateMissMu.Unlock()
	set, ok := locateMisses[searchPath]
	if !ok {
		return false
	}
	_, hit := set[cmd]
	return hit
}

// locateMissRecord 把一次**真实扫描**得到的 miss 记进缓存。只接受裸名字 ——
// 路径写法的 miss 与 cwd 相关(见 locate 顶上的注释),不进缓存。
func locateMissRecord(cmd, searchPath string) {
	locateMissMu.Lock()
	defer locateMissMu.Unlock()
	set, ok := locateMisses[searchPath]
	if !ok {
		set = make(map[string]struct{})
		locateMisses[searchPath] = set
	}
	set[cmd] = struct{}{}
}
