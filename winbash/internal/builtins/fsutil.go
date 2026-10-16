package builtins

import (
	"io"
	"path/filepath"
	"strings"
)

// ResolvePath 把用户写下的路径解析成 Windows 原生形式。
//
// 接受四种写法:
//   - MSYS 挂载点 /tmp、/usr/bin、/bin、/ —— 真 Git Bash 的挂载表(见 msyspath.go)
//   - POSIX 盘符 /c/Users/x —— Git Bash 用户的肌肉记忆,也是 AI 从训练数据里学到的
//   - Windows 原生 C:\Users\x
//   - 相对路径(基于 cwd 拼接)
//
// 关键:**不要**把 POSIX 形式交给 filepath.Abs。在 Windows 上它会把 /c/Users/x
// 解释成「当前盘符根目录下的 c\Users\x」,得到的路径看着完全合法却指向不存在的地方。
// 这类错误最糟糕 —— 它不报错,只是找不到文件,而「静默地找错地方」正是要消灭的翻车形态。
//
// 挂载点必须在原生绝对路径之前判:否则 `/tmp/x` 会被 `filepath.IsAbs` 认成
// 「当前盘根下的 tmp\x」,同样静默指错。
func ResolvePath(cwd, p string) (string, error) {
	if p == "" {
		return cwd, nil
	}
	// 盘符形式(`/c/...`)比挂载点更具体,先判 —— 否则 `/c/Windows` 会被 `/` 挂载点
	// 抢走,变成 `C:\Program Files\Git\c\Windows`(实测 `cd /c/Windows` 就栽在这里)。
	if drive, rest, ok := splitPOSIXDrive(p); ok {
		return filepath.Join(drive+`\`, rest), nil
	}
	if win, ok := msysMountFor(p); ok {
		return win, nil
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p), nil
	}
	return filepath.Join(cwd, p), nil
}

// splitPOSIXDrive 识别 /c/... 形式,返回盘符(形如 "C:")与剩余部分。
//
// 也接受**只有盘符**的 `/c`(没有尾斜杠):真 bash 里它解析到 `C:/`,是合法路径 ——
// 实测 `[ -e /c ]` 给 EXISTS、`cd /c` 给 `C:/`。早先只认 `/c/...`,于是 `/c` 落到了
// `/` 挂载点手里,变成"Git 安装根下的 c 目录"(不存在)。
func splitPOSIXDrive(p string) (drive, rest string, ok bool) {
	if len(p) < 2 || p[0] != '/' {
		return "", "", false
	}
	c := p[1]
	if !isASCIILetter(c) {
		return "", "", false
	}
	if len(p) == 2 {
		return strings.ToUpper(string(c)) + ":", "", true // `/c`
	}
	if p[2] != '/' {
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
