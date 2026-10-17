// 读侧的措辞:打开一个**目录**时,Windows 上 `os.Open` 会**成功**(O_RDONLY),
// 失败要等到第一次 Read 才发生,给的是 Win32 的 ERROR_INVALID_FUNCTION。
//
// 这一点与 POSIX 不同,而 mvdan/sh 的几处内建都假设"open 成功就能读",于是错误
// 从**读**那里冒出来,带着三种毛病:
//
//	source adir    → source: read C:\...\adir: Incorrect function.
//	                 ↑ 泄漏 Win32 API 名(read)、绝对路径、Win32 长句
//	mapfile < adir → mapfile: unable to read, read C:\...\adir: Incorrect function.
//	cat < adir     → cannot read standard input: I/O error (Win32 error 1)
//	                 ↑ 不泄漏 API 名,但没人看得懂
//	read < adir    → **完全静默**(只有 rc=1)
//
// 真 Git Bash(2026-10-18 实测):
//
//	source adir    → source: adir: is a directory
//	cat < adir     → cat: -: Is a directory
//	read < adir    → read: 0: read error: Is a directory
//	mapfile < adir → 静默、n=0
//
// 四处根因**相同**:这个 fd 读不出东西。所以修法统一 —— 让它**在 open 阶段就失败**,
// 于是走上面那条已经修好的 ShellPathError 路径,得到 `adir: Is a directory`。

package builtins

import (
	"io"
	"os"
	"syscall"
)

// OpenErrorOrDir 处理 baseOpenHandler 的成功结果。
//
// 目标不是目录 → 原样返回(正常文件零开销,Stat 走已打开的 fd)。
// 目标是目录   → 关掉 fd,返回一个 `Is a directory` 的 ShellPathError。
//
// ## 为什么在 open 阶段就拒绝,而不是包一个"读时才报错"的 reader
//
// 我先试过后者(包一层 reader),结果**更糟**:mvdan/sh 的 `newStdinFile` 只在
// reader 是 `*os.File` 时直通,否则走 default 分支 —— 起一个 goroutine
// `io.Copy(pw, r); pw.Close()`,而 `io.Copy` 的返回值**没人检查**。
// 于是我的错误被吞掉,`cat < adir` 变成 rc=0、无输出、无报错 ——
// 从"报 Win32 长句"退化成**静默给出空结果**,比修之前更坏。
// (项目里 SetStdinBytes 的注释记着同一个机制:非 *os.File 的 reader 会被抽干。)
//
// ## 语义代价(诚实记录)
//
// `read < 目录` / `mapfile < 目录` 在真 bash 里是 **open 成功、失败在读**:
// `read` 报错后返回 1、脚本继续;`mapfile` 干脆静默、n=0。
// 我们改成"重定向失败、命令不执行":
//   - `read` 那条:脚本层面一致(都 rc=1、都不终止脚本),只是不再执行 read;
//   - `mapfile` 那条:我们从"静默成功"变成"响亮失败" —— 按项目底线
//     「不静默做错事」,响亮是**更好的**那一边。
//
// 之所以接受这个代价:mvdan/sh 里 read/mapfile/source/cat 四处各自吃错误,
// 想在"读"那一层统一修得分别 patch 四处(且 read 分支本来就静默吞错误);
// 而在 open 这一层拦,一处覆盖全部,且错误措辞与重定向失败天然一致。
func OpenErrorOrDir(f io.ReadWriteCloser, userPath, resolvedPath string) (io.ReadWriteCloser, error) {
	of, ok := f.(*os.File)
	if !ok || of == nil {
		return f, nil
	}
	// 用 fd 的 Stat(不重新按路径打开)。拿不到 Stat 就按"不是目录"处理 ——
	// 宁可不拦,也不要因为 Stat 失败把一个本来能读的文件变成读不了。
	//
	// **`err != nil` 这条分支是不可达的防御**(变异验证实测记录):对一个刚 Open
	// 成功的 fd 调 Stat 不会失败,所以把它改成"Stat 失败也当目录"不会有任何测试
	// 变红 —— 那属于**等价变异**,不是测试缺口。保留它的理由在方向性:万一将来
	// 某种文件系统让 Stat 失败,我们该退化成"照常读"(能看见的读错误),而不是
	// "所有重定向全线报 Is a directory"(灾难性误报)。
	fi, err := of.Stat()
	if err != nil || !fi.IsDir() {
		return f, nil
	}
	_ = of.Close()
	// 传 syscall.EISDIR 而不是构造 *os.PathError:posixErrText 里已经有
	// `errors.Is(err, syscall.EISDIR) -> "Is a directory"` 这条,复用它。
	return nil, ShellPathError(userPath, syscall.EISDIR, resolvedPath)
}
