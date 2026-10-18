// `/dev/null` 作为**参数**必须静默成功 —— 与真 bash 一致(rc=0、无输出)。
//
// 修之前它报 `/dev/null: No such file or directory`,因为参数这条路与重定向
// 那条路完全不同:
//
//	cat < /dev/null   走 mvdan/sh 的 OpenHandler → ResolveShellPath(msyspath.go 的
//	                  /dev/null 特判原样放行)→ 默认 handler 转 NUL —— 一直是对的
//	cat /dev/null     走 ① 层内建 cat.go → ResolvePath → os.Open —— 完全不过
//	                  OpenHandler,而 `/` 挂载点兜底把它解析成
//	                  `<Git 安装根>\dev\null`(不存在),于是报"找不到文件"
//
// 所以这组测试钉的是**参数**这条(核心)、重定向那条别被改坏(回归),
// 以及同一个修点顺带覆盖到的 grep / ls —— 证明改的是 ① 层路径的唯一入口
// ResolvePath,而不是只在 cat 里打补丁。
package builtins

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ★ 映射目标必须是 `"NUL"` 这个字符串,与 third_party/mvdan-sh/interp/handler.go:404
// (`path = "NUL"`)互相钉住:重定向走那个 handler,参数走这里,两边映射到不同的
// 名字,同一个 `/dev/null` 就会指向两个不同的东西(一边是 Windows 的 NUL 设备、
// 一边是某个真实文件)。改成 `\\.\NUL` 也能 open,但就和 handler 分叉了。
func TestResolvePathDevNullMapsToWindowsNUL(t *testing.T) {
	got, err := ResolvePath(`C:\work`, "/dev/null")
	if err != nil {
		t.Fatalf("ResolvePath(/dev/null) 不该报错: %v", err)
	}
	if got != "NUL" {
		t.Fatalf("ResolvePath(/dev/null) = %q,应当是 %q(handler.go:404 用的同一个名字)", got, "NUL")
	}
	// 能真的打开、且立即 EOF —— 否则 cat 那条会挂住或报错。
	f, err := os.Open(got)
	if err != nil {
		t.Fatalf("映射到 %q 之后 open 失败: %v", got, err)
	}
	defer f.Close()
	buf := make([]byte, 1)
	if n, rerr := f.Read(buf); n != 0 || rerr != io.EOF {
		t.Fatalf("NUL 设备应当立即 EOF,got n=%d err=%v", n, rerr)
	}
}

// ★ 核心:`cat /dev/null`(参数形式)→ rc=0、无输出、无报错。
// 真 bash 实测就是这三个性质;修之前是 rc=1 + `/dev/null: No such file or directory`。
func TestCatDevNullArgumentIsSilentSuccess(t *testing.T) {
	var out, errw bytes.Buffer
	code := Cat(context.Background(), t.TempDir(), []string{"/dev/null"}, emptyIn(), &out, &errw)
	if code != 0 {
		t.Fatalf("cat /dev/null 应当 rc=0,got %d(stderr=%q)", code, errw.String())
	}
	if out.Len() != 0 {
		t.Fatalf("cat /dev/null 应当无输出,got %q", out.String())
	}
	if errw.Len() != 0 {
		t.Fatalf("cat /dev/null 应当无报错,got %q", errw.String())
	}
}

// ★ 回归:`cat < /dev/null` 这条**已经是对的**,不许被上一条的修改弄坏。
//
// 重定向链路走的是 OpenHandler + 默认 handler 的 NUL 特例(internal/shell/shell.go,
// handler.go:404),不经 ResolvePath;端到端由 internal/shell/readerr_test.go 的
// `TestDevNullAndSpecialTargetsAreNotTreatedAsDirectories` 钉着(`cat < /dev/null`
// → rc=0)。这里钉的是同一层的另一半:ResolvePath 的产物 `"NUL"` 打开后**立即 EOF**,
// 传给 cat 当 stdin 时静默成功 —— 映射改成别的名字就在这里变红。
func TestCatStdinFromDevNullStaysSilent(t *testing.T) {
	f, err := os.Open(devNullWindows) // 与 `cat < /dev/null` 打开的是同一个东西
	if err != nil {
		t.Fatalf("open %q: %v", devNullWindows, err)
	}
	defer f.Close()

	var out, errw bytes.Buffer
	code := Cat(context.Background(), t.TempDir(), nil, f, &out, &errw)
	if code != 0 {
		t.Fatalf("cat < /dev/null 应当 rc=0,got %d(stderr=%q)", code, errw.String())
	}
	if out.Len() != 0 {
		t.Fatalf("cat < /dev/null 应当无输出,got %q", out.String())
	}
}

// ★ 证明改的是对的那一层:grep / ls 与 cat 共用 ResolvePath,也必须解析到 NUL。
// 只在 cat 里特判的话,这两个照旧报「找不到文件」,这组断言就会红。
func TestGrepAndLsDevNullResolveToNUL(t *testing.T) {
	dir := t.TempDir()

	// grep x /dev/null:读 NUL 立即 EOF、无匹配 → rc=1(grep 的"没命中"),不是"文件不存在"。
	var out, errw bytes.Buffer
	if code := Grep(context.Background(), dir, []string{"x", "/dev/null"}, nil, &out, &errw); code != 1 {
		t.Errorf("grep x /dev/null 应当 rc=1(无匹配),got %d(stderr=%q)", code, errw.String())
	}
	if strings.Contains(errw.String(), "No such file") {
		t.Errorf("grep x /dev/null 不该报找不到文件: %q", errw.String())
	}
	if out.Len() != 0 {
		t.Errorf("grep x /dev/null 应当无输出,got %q", out.String())
	}

	// ls /dev/null:按 bash 惯例把它当单个文件列出(名字用参数原文),不报错。
	out.Reset()
	errw.Reset()
	if code := Ls(context.Background(), dir, []string{"/dev/null"}, &out, &errw); code != 0 {
		t.Errorf("ls /dev/null 应当 rc=0,got %d(stderr=%q)", code, errw.String())
	}
	if strings.Contains(errw.String(), "No such file") {
		t.Errorf("ls /dev/null 不该报找不到文件: %q", errw.String())
	}
	if !strings.Contains(out.String(), "/dev/null") {
		t.Errorf("ls /dev/null 应当列出参数原文,got %q", out.String())
	}
}

// ★ 反面:真失败不能被顺手吞掉。`/no-such-path` 不是 /dev/null,照旧要响亮报错。
// 少了这条,有人把特判放宽成「一切 /dev/... 都成功」也不会有测试变红。
func TestCatMissingPathStillFailsLoudly(t *testing.T) {
	var out, errw bytes.Buffer
	code := Cat(context.Background(), t.TempDir(), []string{"/no-such-path"}, emptyIn(), &out, &errw)
	if code == 0 {
		t.Fatal("cat /no-such-path 必须非零退出 —— 真失败不能被 /dev/null 特判吞掉")
	}
	if !strings.Contains(errw.String(), "No such file or directory") {
		t.Fatalf("应当报 No such file or directory(真 bash 的措辞),got %q", errw.String())
	}
}

// ★ 反面:`cat <目录>` 报 `Is a directory` 是上一轮刚修的行为,不许回归
// (Windows 上 os.Open 目录会成功,失败要到第一次 Read,所以拦在 open 之后)。
// 与 errfmt_test.go 的格式用例成对:那条钉措辞格式,这条钉「/dev/null 特判
// 没有把目录也变成静默成功」。
func TestCatDirectoryArgStillIsADirectory(t *testing.T) {
	dir := t.TempDir()
	adir := filepath.Join(dir, "adir")
	if err := os.Mkdir(adir, 0o755); err != nil {
		t.Fatal(err)
	}
	var out, errw bytes.Buffer
	code := Cat(context.Background(), dir, []string{"adir"}, emptyIn(), &out, &errw)
	if code == 0 {
		t.Fatal("cat adir 必须非零退出")
	}
	if !strings.Contains(errw.String(), "adir: Is a directory") {
		t.Fatalf("应当报 `adir: Is a directory`(真 bash 的措辞),got %q", errw.String())
	}
}
