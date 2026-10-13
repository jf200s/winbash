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

func TestLsDefaultIsOneNamePerLine(t *testing.T) {
	// 默认输出必须与 bash 一致：一行一个名字，无装饰
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("x"), 0o644)

	var out bytes.Buffer
	if code := Ls(context.Background(), dir, nil, &out, io.Discard); code != 0 {
		t.Fatalf("exit code %d", code)
	}
	got := strings.Fields(out.String())
	if len(got) != 2 || got[0] != "a.txt" {
		t.Fatalf("got %q", out.String())
	}
}

func TestLsHandlesChineseAndSpaceInPath(t *testing.T) {
	// Review Focus 第 1 条：这台机器的用户名就含空格，中文路径是日常
	dir := filepath.Join(t.TempDir(), "命令行 目录")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "中文 文件.txt"), []byte("x"), 0o644)

	var out bytes.Buffer
	if code := Ls(context.Background(), dir, nil, &out, io.Discard); code != 0 {
		t.Fatalf("exit code %d", code)
	}
	if !strings.Contains(out.String(), "中文 文件.txt") {
		t.Fatalf("got %q", out.String())
	}
}

func TestLsLongFormatGivesRawByteSizeNotCommaString(t *testing.T) {
	// 值保证：不能出现 "86,488 K" 这类带千分位的字符串
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "f"), bytes.Repeat([]byte("x"), 1234), 0o644)

	var out bytes.Buffer
	Ls(context.Background(), dir, []string{"-l"}, &out, io.Discard)
	if !strings.Contains(out.String(), "1234") {
		t.Fatalf("want raw byte count: %q", out.String())
	}
	if strings.Contains(out.String(), ",") {
		t.Fatalf("must not contain thousands separators: %q", out.String())
	}
}

func TestLsRejectsUnknownFlag(t *testing.T) {
	// Global Constraint：不认识的 flag 必须报错，不得静默忽略
	var errw bytes.Buffer
	code := Ls(context.Background(), t.TempDir(), []string{"--definitely-not-a-flag"}, io.Discard, &errw)
	if code == 0 {
		t.Fatal("expected non-zero exit for unknown flag")
	}
	if errw.Len() == 0 {
		t.Fatal("expected an error message")
	}
}

func TestResolvePathAcceptsBothPOSIXAndWindowsForms(t *testing.T) {
	if p, err := ResolvePath(`C:\work`, "/c/Users/x"); err != nil || p != `C:\Users\x` {
		t.Fatalf("posix form: %q %v", p, err)
	}
	if p, err := ResolvePath(`C:\work`, `D:\other`); err != nil || p != `D:\other` {
		t.Fatalf("windows form: %q %v", p, err)
	}
}

func TestResolvePathJoinsRelativeAgainstCwd(t *testing.T) {
	got, err := ResolvePath(`C:\work`, filepath.Join("sub", "f.txt"))
	if err != nil || got != `C:\work\sub\f.txt` {
		t.Fatalf("got %q %v", got, err)
	}
}

func TestLsMissingPathIsAnErrorNotSilence(t *testing.T) {
	// 找不到就报错并给建议，不得静默返回 0 —— 静默是「翻车」的主要形态
	var errw bytes.Buffer
	code := Ls(context.Background(), t.TempDir(), []string{"no-such-entry"}, io.Discard, &errw)
	if code == 0 || errw.Len() == 0 {
		t.Fatalf("code=%d err=%q", code, errw.String())
	}
}
