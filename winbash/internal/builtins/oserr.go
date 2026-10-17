// 路径错误的**措辞层**:把 Go / Win32 的原始错误翻成 POSIX 说法。
//
// 为什么需要它:Go 在 Windows 上的路径错误长这样 ——
//
//	open C:\Users\...\nope.txt: The system cannot find the file specified.
//	GetFileAttributesEx C:\...\a<b>.txt: The filename, directory name, or volume
//	  label syntax is incorrect.
//
// 三处不对劲,都直接影响「AI 能不能看懂并自己修」:
//
//  1. **泄漏 Win32 API 名**(`open` / `GetFileAttributesEx` / `CreateFile`)。
//     那是实现细节,使用者拿它无从下手。
//  2. **给的是绝对路径**。用户写的是 `ls nope.txt`,却被回了一个带临时目录的
//     长路径 —— 既噪音,又让人以为自己写错了路径。
//  3. **英文 Win32 措辞**。AI 对 POSIX 那套(`No such file or directory` /
//     `Permission denied`)有极强的先验,对 Win32 长句没有。
//
// 目标格式(与真 Git Bash 的"问题"部分逐字对齐):
//
//	cannot access 'nope.txt': No such file or directory
//	cannot open 'x.txt': Permission denied
//	cannot access 'plain.txt/sub': Not a directory
//
// 三行式的外壳(`winbash: 命令 / 问题 / 怎么办`)保持不变 —— 那是本项目
// 自己的风格,`怎么办` 那行比 bash 多给一步指路。

package builtins

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// Win32 errno 里需要特殊翻译的几个。其余走 errors.Is 的标准类别。
//
// 数值来自实测(2026-10-18,见提交说明):
//
//	2   ERROR_FILE_NOT_FOUND     stat/open 一个不存在的文件
//	3   ERROR_PATH_NOT_FOUND     stat 深层不存在 / 中间某段是文件
//	5   ERROR_ACCESS_DENIED      受保护的系统文件
//	123 ERROR_INVALID_NAME       路径里有非法字符
//	267 ERROR_DIRECTORY          该是目录的却不是
//
// **刻意不映射 ERROR_INVALID_FUNCTION(1)**:读目录时 Windows 给的就是它
// (`Incorrect function.`),但那是**通用错误码**,别的场景完全可能表示别的意思。
// 把它翻成 `Is a directory` 是**猜** —— 而猜错会让使用者往错的方向修。
// 真正的做法是调用方先 stat 判目录(`cat` 已经这么做),这里只留兜底。
//
// 这条取舍是变异验证逼出来的:当初写了这条映射,于是「去掉 cat 的显式判目录」
// 那个变异**抓不到** —— 说明那条投机映射在替正确实现打掩护。
const (
	errnoFileNotFound = syscall.Errno(2)
	errnoPathNotFound = syscall.Errno(3)
	errnoAccessDenied = syscall.Errno(5)
	errnoInvalidName  = syscall.Errno(123)
	errnoDirectory    = syscall.Errno(267)
)

// userPathProblem 生成路径错误的"问题"行。
//
// 两种格式,**按命令选**,不是随意的风格问题 —— 真 GNU coreutils 内部就不统一
// (2026-10-18 实测真 Git Bash):
//
//	ls nope     → ls: cannot access 'nope': No such file or directory
//	cat nope    → cat: nope: No such file or directory
//	grep x nope → grep: nope: No such file or directory
//	wc nope     → wc: nope: No such file or directory
//	head nope   → head: cannot open 'nope' for reading: No such file or directory
//	sed nope    → sed: can't read nope: No such file or directory
//
// 也就是说 `cannot access` 是 **ls 的**措辞,而 cat / grep / wc 给的是裸的
// `<路径>: <原因>`。所以:
//
//	verb == "cannot access" → `cannot access 'x': ...`   (ls)
//	verb == ""              → `x: ...`                    (cat / grep)
//
// verb 是动作;userPath 必须是**用户写的那串**(不是 ResolvePath 之后的绝对路径);
// absPath 只用于内部判断(区分 `Not a directory`),绝不进输出。
//
// 一条重要取舍:描述里**不带** `winbash:` 或命令名前缀。调用方本来就用
// errs.Errorf 包了三行式(`winbash: ls / 问题: … / 怎么办: …`),这里再带一次
// 就成了 `问题: ls: cannot access …`,与真 bash 的 `ls: cannot access …` 差一层。
// 实测真 bash 的措辞就是「命令名 + 空格 + 后半段」,而三行式已经在第一行给了
// 命令名,所以这里只出后半段。
func userPathProblem(verb, userPath string, err error, absPath string) string {
	if verb == "" {
		// cat / grep 的裸格式:与真 coreutils 一致(它们不用 cannot access)。
		return fmt.Sprintf("%s: %s", userPath, posixErrText(err, absPath))
	}
	return fmt.Sprintf("%s '%s': %s", verb, userPath, posixErrText(err, absPath))
}

// posixErrText 把错误翻成 POSIX 措辞。
//
// 顺序有讲究:先走 errors.Is 的标准类别(跨平台稳定),再走 Win32 errno 的细表。
// `Not a directory` 必须排在 `No such file or directory` 之前判断 —— 两者在
// Windows 上是**同一个 errno(3)**,只能靠"路径里有没有把文件当目录用"来区分。
func posixErrText(err error, absPath string) string {
	if err == nil {
		return "unknown error"
	}

	// ① 中间某段是普通文件 —— Windows 给 errno 3,与"深层不存在"撞车,
	//    所以要先做一次结构检查才能给出真 bash 的 `Not a directory`。
	if absPath != "" && isNotADirectory(absPath) {
		return "Not a directory"
	}

	// ② 标准错误类别。
	if errors.Is(err, fs.ErrNotExist) {
		return "No such file or directory"
	}
	if errors.Is(err, fs.ErrPermission) {
		return "Permission denied"
	}
	// 目标是目录(往目录上写)。Go 在 Windows 上把它映射成 syscall.EISDIR,
	// 实测 errno=536870954、Error() 给**小写** "is a directory";而真 bash 给
	// `Is a directory`(大写 I)。所以不能直接用 errno.Error()。
	//
	// 注意这类错误**不是** fs.ErrNotExist 也不是 fs.ErrPermission,
	// 不加这条会掉到下面的兜底给 `I/O error (Win32 error 536870954)` ——
	// 把一句人话变成没人看得懂的数字。
	if errors.Is(err, syscall.EISDIR) {
		return "Is a directory"
	}

	// ③ Win32 errno 细表。
	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno {
		case errnoFileNotFound, errnoPathNotFound, errnoInvalidName:
			// 非法字符(123)真 bash 也给 `No such file or directory` —— 实测确认。
			return "No such file or directory"
		case errnoAccessDenied:
			return "Permission denied"
		case errnoDirectory:
			return "Not a directory"
		}
		// 未知 errno:不把 Win32 长句原样端出去(那是 API 层噪音),给个可定位的短形式。
		// Win32 错误号比消息更有用 —— 搜错误号能查到官方文档。
		return fmt.Sprintf("I/O error (Win32 error %d)", uintptr(errno))
	}

	// ④ 非 errno 的错误(例如 fmt.Errorf 包的):用它的文本,但只保留最后一段,
	//    避免把 `open C:\...: xxx` 这种带路径与 API 名的形态带出去。
	msg := err.Error()
	if i := lastColon(msg); i >= 0 {
		if tail := msg[i+1:]; len(tail) > 1 {
			return trimSpace(tail)
		}
	}
	return msg
}

// ShellPathError 把 shell 层一次 open 失败包成**可直接打印**的措辞。
//
// 为什么需要它:重定向(`> /bad/x`、`< /bad/x`)与 `source` 的失败由 mvdan/sh
// 的 OpenHandler 产生,不走上面那些命令内建的措辞层。原先原样端出 Go 的错误:
//
//	open C:\Program Files\Git\nonexistent-dir\x: The system cannot find the path specified.
//
// 三处都不对(与 dba69c7 修过的那批同源):泄漏 Win32 API 名 `open`、把用户写的
// `/nonexistent-dir/x` 换成绝对路径、用 Win32 长句而不是 POSIX 措辞。
//
// 目标格式对着真 Git Bash 实测(2026-10-18):
//
//	> /nonexistent-dir/x   真 bash: line 1: /nonexistent-dir/x: No such file or directory
//	> adir                 真 bash: line 1: adir: Is a directory
//	> /c/Windows/.../__nope 真 bash: line 1: /c/Windows/.../__nope: Permission denied
//	source nope.sh         真 bash: line 1: nope.sh: No such file or directory
//
// 我们少 `line N:`(mvdan/sh 的 errf 不带行号,且三行式风格本来也不加),其余逐字对齐。
//
// userPath 必须是 **OpenHandler 收到的那个 path** —— 它是 `r.literal(rd.Word)`,
// 也就是用户写的(展开后的)路径,不是 ResolveShellPath 之后的 Windows 绝对路径。
// absPath 传转换后的,只用于区分 `Not a directory`,绝不进输出。
//
// 返回的是**包装类型**而不是裸 *os.PathError:Go 的 PathError.Error() 是
// `Op + " " + Path + ": " + Err`,三个字段都会出现在输出里,无法自定义成 `x: 原因`
// 这个形态(Op 置空会留下一个前导空格)。包装类型让 Error() 完全可控,同时
// Unwrap() 到 *os.PathError 保住 mvdan/sh 的"非 fatal"判定 —— 那需要上游那个
// errors.As 补丁(见 third_party/mvdan-sh/WINBASH-PATCH.md §3)。
func ShellPathError(userPath string, err error, absPath string) error {
	return &shellPathError{
		msg:   userPathProblem("", userPath, err, absPath),
		inner: &os.PathError{Op: "open", Path: absPath, Err: err},
	}
}

// shellPathError 让 Error() 自定义、Unwrap() 回到 *os.PathError。
//
// 两者缺一不可:
//   - 没有自定义 Error(),输出就带 `open <绝对路径>:` 前缀(Win32 泄漏);
//   - 没有 Unwrap 到 *os.PathError,mvdan/sh 的 r.open 会当成 handler 的 fatal
//     错误,把"这条命令失败"升级成"整个脚本终止" —— 那是行为改变,不只是文案。
type shellPathError struct {
	msg   string
	inner *os.PathError
}

func (e *shellPathError) Error() string { return e.msg }

// Unwrap 返回内层 PathError。注意它**同时**让 errors.Is(err, fs.ErrNotExist)
// 这类判定沿链生效 —— 与原生 *os.PathError 的语义一致,不会削弱调用方能力。
func (e *shellPathError) Unwrap() error { return e.inner }

// lastColon 找最后一个 `: ` 的位置(用于剥掉 `open <path>:` 前缀)。
func lastColon(s string) int {
	for i := len(s) - 2; i >= 0; i-- {
		if s[i] == ':' && s[i+1] == ' ' {
			return i
		}
	}
	return -1
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t' || s[len(s)-1] == '\n') {
		s = s[:len(s)-1]
	}
	return s
}

// isNotADirectory 判断「路径找不到」是不是其实「中间某段是普通文件」。
//
// 为什么必须查:Windows 把两种情况压成同一个 errno 3,而真 bash 分得很清 ——
//
//	ls a/b/c.txt      → No such file or directory   (a 不存在)
//	ls plain.txt/sub  → Not a directory             (plain.txt 是文件)
//
// 判据:从最深的前缀往上找,只要有一层**存在且不是目录**,而它后面还有内容,
// 就是"把文件当目录用"。
//
// 只在错误路径上调用,不做缓存也不求快 —— 一次 `ls` 失败多两次 Stat 无所谓。
func isNotADirectory(abs string) bool {
	dir := filepath.Dir(abs)
	for {
		parent := filepath.Dir(dir)
		if parent == dir {
			// 到盘根了,没有更上层的段可查。
			return false
		}
		// dir 本身存在吗?存在且不是目录 → 它就是那个"被当成目录的文件"。
		if fi, err := os.Stat(dir); err == nil && !fi.IsDir() {
			return true
		}
		dir = parent
	}
}
