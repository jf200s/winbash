package builtins

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"winbash/internal/errs"
)

// 计划里的 6 个用例(TestGrepMatchesLineAndPrintsIt … TestGrepNoMatchExitsOne)
// 照抄自 plan Task 6;其后是本轮补的 6 个,补的理由写在各自注释里。

func TestGrepMatchesLineAndPrintsIt(t *testing.T) {
	r := strings.NewReader("alpha\nbeta\ngamma\n")
	var out bytes.Buffer
	Grep(context.Background(), ".", []string{"beta"}, r, &out, io.Discard)
	if out.String() != "beta\n" {
		t.Fatalf("got %q", out.String())
	}
}

func TestGrepDefaultIsLiteralNotRegex(t *testing.T) {
	r := strings.NewReader("a.c\nabc\n")
	var out bytes.Buffer
	Grep(context.Background(), ".", []string{"a.c"}, r, &out, io.Discard)
	if out.String() != "a.c\n" {
		t.Fatalf("dot must be literal without -E: %q", out.String())
	}
}

func TestGrepDashEUsesPosixERE(t *testing.T) {
	r := strings.NewReader("alpha\nbeta\n")
	var out bytes.Buffer
	Grep(context.Background(), ".", []string{"-E", "alpha|beta"}, r, &out, io.Discard)
	if out.String() != "alpha\nbeta\n" {
		t.Fatalf("got %q", out.String())
	}
}

func TestGrepPerlClassGivesExplicitError(t *testing.T) {
	// Review Focus 第 2 条：方言差异必须明确，不得静默给错结果
	r := strings.NewReader("123\n")
	var errw bytes.Buffer
	code := Grep(context.Background(), ".", []string{"-E", `\d+`}, r, io.Discard, &errw)
	if code == 0 {
		t.Fatal("expected non-zero for unsupported Perl class")
	}
	if !strings.Contains(errw.String(), `\d`) {
		t.Fatalf("error must name the offending class: %q", errw.String())
	}
	// 错误必须是三行式(spec §7):命令 / 问题 / 怎么办。
	if n := strings.Count(strings.TrimRight(errw.String(), "\n"), "\n"); n != 2 {
		t.Fatalf("want 3 lines, got %d: %q", n+1, errw.String())
	}
	// 而且要把替代写法给出来 —— 没有下一步的错误等于把问题丢回给使用者。
	if !strings.Contains(errw.String(), "[0-9]") {
		t.Fatalf("error must suggest a POSIX replacement: %q", errw.String())
	}
}

func TestGrepPerlClassDetectionSkipsEscapedBackslash(t *testing.T) {
	// `\\d` 是「字面反斜杠 + d」,不是 Perl 类,不能误报。
	r := strings.NewReader("x\n\\d\n")
	var out bytes.Buffer
	if code := Grep(context.Background(), ".", []string{"-E", `\\d`}, r, &out, io.Discard); code != 0 {
		t.Fatalf("escaped backslash must not be treated as a Perl class, code=%d", code)
	}
	if out.String() != "\\d\n" {
		t.Fatalf("got %q", out.String())
	}
}

func TestGrepFlagsInvertCountLineNumber(t *testing.T) {
	r := strings.NewReader("a\nb\na\n")
	var out bytes.Buffer
	Grep(context.Background(), ".", []string{"-c", "a"}, r, &out, io.Discard)
	if strings.TrimSpace(out.String()) != "2" {
		t.Fatalf("count: %q", out.String())
	}

	out.Reset()
	Grep(context.Background(), ".", []string{"-v", "a"}, strings.NewReader("a\nb\n"), &out, io.Discard)
	if out.String() != "b\n" {
		t.Fatalf("invert: %q", out.String())
	}

	out.Reset()
	Grep(context.Background(), ".", []string{"-n", "a"}, strings.NewReader("x\na\n"), &out, io.Discard)
	if out.String() != "2:a\n" {
		t.Fatalf("line number: %q", out.String())
	}
}

func TestGrepNoMatchExitsOne(t *testing.T) {
	code := Grep(context.Background(), ".", []string{"zzz"}, strings.NewReader("abc\n"), io.Discard, io.Discard)
	if code != 1 {
		t.Fatalf("code=%d want 1", code)
	}
}

// -i 与合并写法。AI 常写 `grep -in`;`(?i)` 在 Go 的 POSIX 模式里不被接受,
// 所以这条同时钉住了「-i 与 -E 能共存」这个实现细节。
func TestGrepIgnoreCaseAndCombinedFlags(t *testing.T) {
	var out bytes.Buffer
	Grep(context.Background(), ".", []string{"-i", "ALPHA"}, strings.NewReader("alpha\nbeta\n"), &out, io.Discard)
	if out.String() != "alpha\n" {
		t.Fatalf("-i: %q", out.String())
	}

	out.Reset()
	Grep(context.Background(), ".", []string{"-iE", "alpha|BETA"}, strings.NewReader("ALPHA\nbeta\n"), &out, io.Discard)
	if out.String() != "ALPHA\nbeta\n" {
		t.Fatalf("-iE: %q", out.String())
	}

	out.Reset()
	Grep(context.Background(), ".", []string{"-in", "alpha"}, strings.NewReader("x\nALPHA\n"), &out, io.Discard)
	if out.String() != "2:ALPHA\n" {
		t.Fatalf("-in: %q", out.String())
	}
}

// 多文件时行前带文件名(bash 行为)。少了前缀,`grep pat a.txt b.txt` 的结果就没法归属。
func TestGrepMultipleFilesPrefixName(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x\ny\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("x\n"), 0o644)

	var out bytes.Buffer
	code := Grep(context.Background(), dir, []string{"x", "a.txt", "b.txt"}, strings.NewReader(""), &out, io.Discard)
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	if out.String() != "a.txt:x\nb.txt:x\n" {
		t.Fatalf("got %q", out.String())
	}

	out.Reset()
	Grep(context.Background(), dir, []string{"-c", "x", "a.txt", "b.txt"}, strings.NewReader(""), &out, io.Discard)
	if out.String() != "a.txt:1\nb.txt:1\n" {
		t.Fatalf("count with names: %q", out.String())
	}
}

func TestGrepDashLListsFileNamesOnce(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x\nx\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("x\n"), 0o644)

	var out bytes.Buffer
	code := Grep(context.Background(), dir, []string{"-l", "x", "a.txt", "b.txt"}, strings.NewReader(""), &out, io.Discard)
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	if out.String() != "a.txt\nb.txt\n" {
		t.Fatalf("-l 每个文件只能出现一次: %q", out.String())
	}
}

func TestGrepRecursiveSearchesDirectory(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "sub", "f.txt"), []byte("nope\nhit\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "top.txt"), []byte("hit\n"), 0o644)

	var out bytes.Buffer
	code := Grep(context.Background(), dir, []string{"-r", "hit", "."}, strings.NewReader(""), &out, io.Discard)
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	got := out.String()
	// 路径一律用正斜杠输出 —— ① 层的文本语义按 bash 来,不把 Windows 反斜杠漏出去。
	if !strings.Contains(got, "top.txt:hit\n") || !strings.Contains(got, "sub/f.txt:hit\n") {
		t.Fatalf("recursive output: %q", got)
	}

	// 不给 -r 时目录是错误(bash: Is a directory),不是静默跳过。
	var errw bytes.Buffer
	if code := Grep(context.Background(), dir, []string{"hit", "sub"}, strings.NewReader(""), io.Discard, &errw); code == 0 {
		t.Fatal("directory without -r must fail")
	}
	if !strings.Contains(errw.String(), "目录") {
		t.Fatalf("error must say it is a directory: %q", errw.String())
	}
}

func TestGrepUnknownFlagExitsUsage(t *testing.T) {
	var errw bytes.Buffer
	code := Grep(context.Background(), ".", []string{"-Z", "x"}, strings.NewReader("x\n"), io.Discard, &errw)
	if code != errs.ExitUsage {
		t.Fatalf("code=%d want %d", code, errs.ExitUsage)
	}
	if !strings.Contains(errw.String(), "-Z") {
		t.Fatalf("error must name the flag: %q", errw.String())
	}
}

// 长短行与二进制字节都必须原样通过 —— 用 bufio.Scanner 会在 64KB 处静默截断/报错。
func TestGrepLongLineAndBinaryBytesPassThroughUnchanged(t *testing.T) {
	long := bytes.Repeat([]byte("a"), 200_000)
	raw := append(append([]byte{}, long...), '\n')
	raw = append(raw, 0xff, 0xfe, 0x00, 'h', 'i', 't', '\n')

	var out bytes.Buffer
	code := Grep(context.Background(), ".", []string{"aaa"}, bytes.NewReader(raw), &out, io.Discard)
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	want := append(append([]byte{}, long...), '\n')
	if !bytes.Equal(out.Bytes(), want) {
		t.Fatalf("long line truncated or altered: got %d bytes want %d", out.Len(), len(want))
	}

	out.Reset()
	if code := Grep(context.Background(), ".", []string{"hit"}, bytes.NewReader(raw), &out, io.Discard); code != 0 {
		t.Fatalf("code=%d", code)
	}
	if !bytes.Equal(out.Bytes(), []byte{0xff, 0xfe, 0x00, 'h', 'i', 't', '\n'}) {
		t.Fatalf("binary bytes altered: %v", out.Bytes())
	}
}

// 没有结尾换行的最后一行也要能匹配并补上行结束符(bash 行为)。
func TestGrepMatchesLastLineWithoutTrailingNewline(t *testing.T) {
	var out bytes.Buffer
	if code := Grep(context.Background(), ".", []string{"last"}, strings.NewReader("first\nlast"), &out, io.Discard); code != 0 {
		t.Fatalf("code=%d", code)
	}
	if out.String() != "last\n" {
		t.Fatalf("got %q", out.String())
	}
}

// 长开关要整体报出(ls 那边踩过的同一个坑:报成 `--` 使用者定位不到)。
func TestGrepLongFlagReportedWhole(t *testing.T) {
	var errw bytes.Buffer
	code := Grep(context.Background(), ".", []string{"--nope", "x"}, strings.NewReader("x\n"), io.Discard, &errw)
	if code != errs.ExitUsage {
		t.Fatalf("code=%d want %d", code, errs.ExitUsage)
	}
	if !strings.Contains(errw.String(), "--nope") {
		t.Fatalf("error must name the whole flag: %q", errw.String())
	}
}

// `-l` 从标准输入读时,bash 报的是 "(standard input)" —— 无归属的输出在这里没有意义。
func TestGrepDashLFromStdinNamesStandardInput(t *testing.T) {
	var out bytes.Buffer
	if code := Grep(context.Background(), ".", []string{"-l", "x"}, strings.NewReader("x\n"), &out, io.Discard); code != 0 {
		t.Fatalf("code=%d", code)
	}
	if out.String() != "(standard input)\n" {
		t.Fatalf("got %q", out.String())
	}
}

// `-` 作为文件列表里的一员时,多文件输出同样要给它一行归属。
func TestGrepDashMeansStdinAmongFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x\n"), 0o644)

	var out bytes.Buffer
	code := Grep(context.Background(), dir, []string{"x", "a.txt", "-"}, strings.NewReader("x\n"), &out, io.Discard)
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	if out.String() != "a.txt:x\n(standard input):x\n" {
		t.Fatalf("got %q", out.String())
	}
}
