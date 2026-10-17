// Package testenv 只有一件职责:给"参照实现"子进程准备一份**可信的**环境。
//
// 为什么值得单独一个包:差异测试的价值完全取决于参照物是否可信,而"真 bash"/"真 grep"
// 会被宿主环境改写 —— 有些 agent 沙箱/工具链会导出 `safe-delete` 这类 shell 函数来包住
// `rm`(靠 BASH_ENV 注入文件、靠导出的 BASH_FUNC_* 传函数)。一旦被改写,"与真 bash 一致"
// 就不再是事实断言,失败信息还会把人引向"winbash 的 rm 不对",误导性极强,而且在干净
// 机器上复现不了。
//
// 这份规则(滤哪些变量)只有一条,所以只该有一份实现:它原先在 builtins 与 shell 两个包
// 里被逐字抄了两遍,而两份就会有一份先漂移。
package testenv

import (
	"os"
	"strings"
)

// CleanBashEnv 返回一份环境,滤掉会让"参照 bash"不再可信的启动注入项。
//
//   - BASH_ENV / ENV:启动脚本
//   - BASH_FUNC_<name>%%:导出的 shell 函数。只裁前者不够 —— 垫片本身仍然会被导入
//
// 另外把 MSYS 的路径转换与 locale 固定下来:golden 表是"已提交的事实",它的可信度取决于
// **生成时**的通道是否稳定。GNU grep 对字符类里反斜杠的处理在实测中被观察到与调用通道
// 相关(同一份字节经 `-f` 读入与经 argv 传入给出过不同结果),所以生成侧不留下这几个变量
// 的自由取值 —— 否则换一台机器重跑 -update 会把另一种语义写进 golden。
func CleanBashEnv() []string {
	drop := map[string]bool{
		"BASH_ENV": true,
		"ENV":      true,
	}
	fixed := map[string]string{
		"MSYS2_ARG_CONV_EXCL": "*", // 不改写任何参数
		"MSYS_NO_PATHCONV":    "1", // 同上
		"MSYSTEM":             "",  // 不声明成 MINGW/MSYS 的某一支
		"LC_ALL":              "C.UTF-8",
		"LANG":                "C.UTF-8",
		"GREP_OPTIONS":        "", // 不注入隐式 flag
	}

	out := make([]string, 0, len(os.Environ())+len(fixed))
	seen := map[string]bool{}
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if drop[name] || strings.HasPrefix(strings.ToUpper(name), "BASH_FUNC_") {
			continue
		}
		if v, ok := fixed[strings.ToUpper(name)]; ok {
			// 统一成大写键名写出去,避免同一个变量出现两份(Windows 环境块不区分大小写)。
			out = append(out, strings.ToUpper(name)+"="+v)
			seen[strings.ToUpper(name)] = true
			continue
		}
		out = append(out, kv)
	}
	for k, v := range fixed {
		if !seen[k] {
			out = append(out, k+"="+v)
		}
	}
	return out
}

// WithPosixPath 把 dir 插到 PATH 最前面,返回新的环境列表。
//
// 为什么需要:差分测试的参照物是 Git Bash,而它要用的 Unix 工具(ls/wc/tr/sort/find/
// xargs)都在 Git 的 usr\bin 里 —— **宿主 PATH 不保证含它**。从 PowerShell 跑
// `go test` 时 PATH 里只有 `Git\cmd`(那个只有 git.exe),于是"真 bash"连 ls 都找不到:
//
//	command -v ls   → 参照侧 exit 1,而 winbash 给 0
//
// 这种红与 winbash 无关,纯属参照物环境不完整,却会把人引向"我们的 ls 有问题"。
// 参照侧的环境必须由我们自己给全,不能指望调用者的 shell —— 与 CleanBashEnv
// 固定 MSYS/locale 是同一条理由(参照物不可信,整组差分测试就没有意义)。
func WithPosixPath(env []string, dir string) []string {
	if dir == "" {
		return env
	}
	out := make([]string, 0, len(env)+1)
	replaced := false
	for _, kv := range env {
		name, val, ok := strings.Cut(kv, "=")
		if ok && strings.EqualFold(name, "PATH") {
			out = append(out, "PATH="+dir+string(os.PathListSeparator)+val)
			replaced = true
			continue
		}
		out = append(out, kv)
	}
	if !replaced {
		out = append(out, "PATH="+dir)
	}
	return out
}
