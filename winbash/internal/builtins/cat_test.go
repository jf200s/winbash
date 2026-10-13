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

func emptyIn() io.Reader { return strings.NewReader("") }

func TestCatWritesFileVerbatim(t *testing.T) {
	dir := t.TempDir()
	content := "line1\n中文\n"
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte(content), 0o644)

	var out bytes.Buffer
	if code := Cat(context.Background(), dir, []string{"f.txt"}, emptyIn(), &out, io.Discard); code != 0 {
		t.Fatal(code)
	}
	if out.String() != content {
		t.Fatalf("got %q want %q", out.String(), content)
	}
}

func TestCatPassesBinaryBytesUnchanged(t *testing.T) {
	// Review Focus 第 4 条：不得假定 UTF-8
	dir := t.TempDir()
	raw := []byte{0x00, 0xff, 0xfe, 0x01, 0x80}
	os.WriteFile(filepath.Join(dir, "b.bin"), raw, 0o644)

	var out bytes.Buffer
	Cat(context.Background(), dir, []string{"b.bin"}, emptyIn(), &out, io.Discard)
	if !bytes.Equal(out.Bytes(), raw) {
		t.Fatalf("bytes altered: %v", out.Bytes())
	}
}

func TestCatMultipleFilesConcatenatesInOrder(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a"), []byte("A"), 0o644)
	os.WriteFile(filepath.Join(dir, "b"), []byte("B"), 0o644)

	var out bytes.Buffer
	Cat(context.Background(), dir, []string{"a", "b"}, emptyIn(), &out, io.Discard)
	if out.String() != "AB" {
		t.Fatalf("got %q", out.String())
	}
}

func TestCatMissingFileExitsNonZeroWithThreeLineError(t *testing.T) {
	var errw bytes.Buffer
	code := Cat(context.Background(), t.TempDir(), []string{"nope.txt"}, emptyIn(), io.Discard, &errw)
	if code != errs.ExitError {
		t.Fatalf("code=%d want %d", code, errs.ExitError)
	}
	if n := strings.Count(strings.TrimRight(errw.String(), "\n"), "\n"); n != 2 {
		t.Fatalf("want 3 lines: %q", errw.String())
	}
}

func TestCatContinuesPastUnreadableFileLikeBash(t *testing.T) {
	// bash 的 cat 遇到打不开的文件不会放弃后面的文件，只把退出码置非零
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "ok"), []byte("OK"), 0o644)

	var out bytes.Buffer
	code := Cat(context.Background(), dir, []string{"nope", "ok"}, emptyIn(), &out, io.Discard)
	if code == 0 {
		t.Fatal("expected non-zero exit")
	}
	if out.String() != "OK" {
		t.Fatalf("must keep going after a bad file, got %q", out.String())
	}
}

func TestCatHandlesPOSIXPathForm(t *testing.T) {
	// ResolvePath 的两种写法在 cat 上同样要成立
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "f"), []byte("hi"), 0o644)

	var out bytes.Buffer
	posix := "/" + strings.ToLower(string(dir[0])) + strings.ReplaceAll(dir[2:], `\`, "/") + "/f"
	if code := Cat(context.Background(), `C:\`, []string{posix}, emptyIn(), &out, io.Discard); code != 0 {
		t.Fatalf("posix path form failed: code=%d", code)
	}
	if out.String() != "hi" {
		t.Fatalf("got %q", out.String())
	}
}

// `-` 表示标准输入 —— 与 grep 对齐(bash 里两者对这个符号的处理是一样的)。
func TestCatDashReadsStdin(t *testing.T) {
	var out bytes.Buffer
	code := Cat(context.Background(), ".", []string{"-"}, strings.NewReader("from stdin\n"), &out, io.Discard)
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	if out.String() != "from stdin\n" {
		t.Fatalf("got %q", out.String())
	}
}

// 无参数 = 读标准输入(bash 行为)。这条同时是"不挂住"的保证:
// stdin 是空的时候必须立刻结束,而不是等人喂数据。
func TestCatWithNoArgsReadsStdin(t *testing.T) {
	var out bytes.Buffer
	if code := Cat(context.Background(), ".", nil, strings.NewReader("piped\n"), &out, io.Discard); code != 0 {
		t.Fatalf("code=%d", code)
	}
	if out.String() != "piped\n" {
		t.Fatalf("got %q", out.String())
	}

	// 空 stdin:立即结束、无输出、退出 0。
	out.Reset()
	if code := Cat(context.Background(), ".", nil, emptyIn(), &out, io.Discard); code != 0 {
		t.Fatalf("empty stdin should still succeed, code=%d", code)
	}
	if out.Len() != 0 {
		t.Fatalf("empty stdin produced output: %q", out.String())
	}
}

// `-` 可以出现在文件列表中间,按出现顺序读(bash 行为)。
func TestCatDashInTheMiddleOfFileList(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a"), []byte("A"), 0o644)
	os.WriteFile(filepath.Join(dir, "b"), []byte("B"), 0o644)

	var out bytes.Buffer
	code := Cat(context.Background(), dir, []string{"a", "-", "b"}, strings.NewReader("MID"), &out, io.Discard)
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	if out.String() != "AMIDB" {
		t.Fatalf("got %q", out.String())
	}
}

// 二进制字节从 stdin 进来也必须原样通过。
func TestCatPassesBinaryStdinUnchanged(t *testing.T) {
	raw := []byte{0x00, 0xff, 0xfe, 0x01, 0x80}
	var out bytes.Buffer
	if code := Cat(context.Background(), ".", []string{"-"}, bytes.NewReader(raw), &out, io.Discard); code != 0 {
		t.Fatalf("code=%d", code)
	}
	if !bytes.Equal(out.Bytes(), raw) {
		t.Fatalf("stdin bytes altered: %v", out.Bytes())
	}
}

// stdin 为 nil 时不得 panic。daemon 里 stdin 曾经就是 nil,而 `grep` 直接把进程打崩了
// (实测:`winbash -c 'grep xyz'` → daemon 死掉、client 报连接被强制关闭)。
// 这一条钉住内建自己的防线:读不到输入就当空输入,而不是崩。
func TestCatAndGrepSurviveNilStdin(t *testing.T) {
	var out bytes.Buffer
	if code := Cat(context.Background(), ".", []string{"-"}, nil, &out, io.Discard); code != 0 {
		t.Fatalf("cat with nil stdin must not crash: code=%d", code)
	}
	if code := Grep(context.Background(), ".", []string{"xyz"}, nil, &out, io.Discard); code != 1 {
		t.Fatalf("grep with nil stdin must exit 1 (no match), got %d", code)
	}
}
