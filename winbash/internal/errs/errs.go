// Package errs 定义 winbash 的退出码与「可操作错误」模型。
//
// spec §7:错误信息固定三行 —— 哪个命令 / 什么错了 / 下一步怎么办。
// 只回一句 "command not found" 等于把问题原样丢回给 AI,而 AI 的下一步动作
// 恰恰取决于你给不给它下一步 —— 这是「不再翻车」这条首要标准最便宜的落地点。
package errs

import (
	"fmt"
	"strings"
)

// 退出码沿用 shell 惯例,不另发明一套。
const (
	ExitOK       = 0
	ExitError    = 1
	ExitUsage    = 2
	ExitNoExec   = 126
	ExitNotFound = 127
)

// Errorf 生成固定的三行式错误:命令 / 问题 / 怎么办。
func Errorf(cmd, problem, remedy string) string {
	return fmt.Sprintf("winbash: %s\n         问题: %s\n         怎么办: %s", cmd, problem, remedy)
}

// TranslateKnownTrap 把本机已知的坑翻译成可操作的解释。
//
// 这些坑不是"可能的猜测",是在这台机器上实测过的:
//   - PATH 里的 bash 指向损坏的 WSL relay(layer ④)
//   - Git Bash 的 MSYS 会改写单斜杠参数,如 reg query /v
//
// 返回 ok=false 表示这只是一次普通失败,别硬套解释。
func TranslateKnownTrap(cmd string, argv []string, raw string) (problem, remedy string, ok bool) {
	// bash → 损坏的 WSL relay。这台机器上 bash 在 PATH 里,
	// 但它 execvpe 一个不存在的 /bin/bash。
	if cmd == "bash" && strings.Contains(raw, "execvpe") {
		return "这台机器上 PATH 里的 'bash' 指向损坏的 WSL relay,不是真的 bash",
			`你要的多半是 Git Bash: C:\Program Files\Git\bin\bash.exe`,
			true
	}

	// MSYS 改写单斜杠参数(Git Bash 特有)。
	if hasSingleSlashFlag(argv) && strings.Contains(raw, "Invalid syntax") {
		return "参数被 MSYS 改写了 —— Git Bash 会把 /v 当成路径,这是它特有的坑",
			"改用 winbash 的子命令形式,不要转发 pwsh 的参数风格: winbash -c 'reg get <key> <name>'",
			true
	}

	return "", "", false
}

// hasSingleSlashFlag 判断参数里有没有会被 MSYS 当成路径的那种单斜杠开关。
func hasSingleSlashFlag(argv []string) bool {
	for _, a := range argv {
		if len(a) >= 2 && a[0] == '/' && a[1] != '/' && isLetter(a[1]) {
			return true
		}
	}
	return false
}

func isLetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}
