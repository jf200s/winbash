package builtins

import (
	"io"
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

// SafeStdin 把一个可能是 nil 的 stdin 变成"空输入"。
//
// nil 的 io.Reader 读起来是 **panic**,不是 EOF —— 实测过:`interp.StdIO(nil, …)` 下
// `winbash -c 'grep xyz'`(无文件、无管道)直接把 daemon 打崩,client 只看到一句
// "An existing connection was forcibly closed by the remote host",完全指不到根因。
//
// 内建这一层兜住它,是因为 stdin 还可能从别的路径变回 nil。
// shell 侧也做了同样的防护(给 interp 一个真正的空 reader) —— 两处是各自的边界。
func SafeStdin(in io.Reader) io.Reader {
	if in == nil {
		return eofReader{}
	}
	return in
}

// eofReader 立即返回 EOF。
type eofReader struct{}

func (eofReader) Read([]byte) (int, error) { return 0, io.EOF }
