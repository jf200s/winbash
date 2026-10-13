package builtins

import (
	"path/filepath"
	"strings"
)

// ResolvePath 把用户写下的路径解析成 Windows 原生形式。
//
// 同时接受两种写法:
//   - POSIX 风格 /c/Users/x —— Git Bash 用户的肌肉记忆,也是 AI 从训练数据里学到的
//   - Windows 原生 C:\Users\x
//
// 相对路径基于 cwd 拼接。
//
// 关键:**不要**把 POSIX 形式交给 filepath.Abs。在 Windows 上它会把 /c/Users/x
// 解释成「当前盘符根目录下的 c\Users\x」,得到的路径看着完全合法却指向不存在的地方。
// 这类错误最糟糕 —— 它不报错,只是找不到文件,而「静默地找错地方」正是要消灭的翻车形态。
func ResolvePath(cwd, p string) (string, error) {
	if p == "" {
		return cwd, nil
	}
	if drive, rest, ok := splitPOSIXDrive(p); ok {
		return filepath.Join(drive+`\`, rest), nil
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p), nil
	}
	return filepath.Join(cwd, p), nil
}

// splitPOSIXDrive 识别 /c/... 形式,返回盘符(形如 "C:")与剩余部分。
func splitPOSIXDrive(p string) (drive, rest string, ok bool) {
	if len(p) < 3 || p[0] != '/' {
		return "", "", false
	}
	c := p[1]
	if !isASCIILetter(c) || p[2] != '/' {
		return "", "", false
	}
	return strings.ToUpper(string(c)) + ":", p[3:], true
}

func isASCIILetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}
