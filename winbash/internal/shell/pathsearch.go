package shell

import (
	"os"
	"path/filepath"
	"strings"
)

// gitBashUsrBinCandidates 是 Git Bash 的 usr\bin 可能所在的位置。
//
// 本机实测(spec 第 1 节):真 Git Bash 在 `C:\Program Files\Git\bin\bash.exe`,
// **不在 PATH 里**;而 PATH 里的 `bash` 指向的是损坏的 WSL relay。
// 所以这里的候选表不是"以防万一",是这台机器上的事实。
var gitBashUsrBinCandidates = []string{
	`C:\Program Files\Git\usr\bin`,
	`C:\Program Files (x86)\Git\usr\bin`,
}

// BuildSearchPath 构造 ③ 层的搜索路径:Git Bash 的 usr\bin **在最前**,后面跟真实 PATH。
//
// 顺序不是小事 —— 这是语料扫描实测出来的:
// Windows 的 System32 里也有 `sort.exe` 与 `find.exe`,而它们与 Unix 同名工具**语义完全不同**。
// 把它们排在前面等于让 AI 的肌肉记忆必然失效:
//
//	`find . -maxdepth 1 -name '*.mod'`  →  FIND: 参数格式不正确   (Windows 的 find 是"在文件里找字符串")
//	`printf 'b\na\n' | sort`            →  乱码                    (Windows 的 sort 按本地代码页处理)
//
// 依据不是偏好,是**与 Git Bash 保持一致**:实测 Git Bash 自己解析 `sort`/`find` 拿到的是
// `/usr/bin/{sort,find}`,而 AI 对这些工具长相的预期正是从 Git Bash 来的。
//
// 仍然**返回新字符串,绝不改进程环境**(Global Constraints):这条路径只活在 Session 里,
// 以及我们派生的 ③ 层子进程的 cmd.Env 里(见 envList)。
func BuildSearchPath(realPath string) string {
	extra := gitBashUsrBin()
	if extra == "" {
		return realPath
	}

	// 把输入里的同一个目录去掉,再前置 —— 这样无论输入是什么样,
	// 结果都是「usr\bin 恰好出现一次,而且在最前」。
	parts := filepath.SplitList(realPath)
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			continue
		}
		if strings.EqualFold(filepath.Clean(p), filepath.Clean(extra)) {
			continue
		}
		kept = append(kept, p)
	}
	if len(kept) == 0 {
		return extra
	}
	return extra + string(filepath.ListSeparator) + strings.Join(kept, string(filepath.ListSeparator))
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
