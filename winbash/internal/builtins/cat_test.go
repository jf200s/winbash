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

func TestCatWritesFileVerbatim(t *testing.T) {
	dir := t.TempDir()
	content := "line1\n中文\n"
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte(content), 0o644)

	var out bytes.Buffer
	if code := Cat(context.Background(), dir, []string{"f.txt"}, &out, io.Discard); code != 0 {
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
	Cat(context.Background(), dir, []string{"b.bin"}, &out, io.Discard)
	if !bytes.Equal(out.Bytes(), raw) {
		t.Fatalf("bytes altered: %v", out.Bytes())
	}
}

func TestCatMultipleFilesConcatenatesInOrder(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a"), []byte("A"), 0o644)
	os.WriteFile(filepath.Join(dir, "b"), []byte("B"), 0o644)

	var out bytes.Buffer
	Cat(context.Background(), dir, []string{"a", "b"}, &out, io.Discard)
	if out.String() != "AB" {
		t.Fatalf("got %q", out.String())
	}
}

func TestCatMissingFileExitsNonZeroWithThreeLineError(t *testing.T) {
	var errw bytes.Buffer
	code := Cat(context.Background(), t.TempDir(), []string{"nope.txt"}, io.Discard, &errw)
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
	code := Cat(context.Background(), dir, []string{"nope", "ok"}, &out, io.Discard)
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
	if code := Cat(context.Background(), `C:\`, []string{posix}, &out, io.Discard); code != 0 {
		t.Fatalf("posix path form failed: code=%d", code)
	}
	if out.String() != "hi" {
		t.Fatalf("got %q", out.String())
	}
}
