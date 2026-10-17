package builtins

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"winbash/internal/errs"
)

// Which 的查找语义:这些用例原来在 shell 包的 pathsearch_test.go 里,
// 随着 Which 搬到这里(它是 ① 层 `which` 命令的核心,与 shell 的会话状态无关)。

func TestWhichFindsCommandPresentOnPath(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "probe-tool.exe")
	os.WriteFile(exe, []byte("MZ"), 0o755)
	if got, ok := Which("probe-tool", dir); !ok || got != exe {
		t.Fatalf("got %q ok=%v", got, ok)
	}
}

func TestWhichReportsMissForAbsentCommand(t *testing.T) {
	if _, ok := Which("definitely-not-installed-xyz", t.TempDir()); ok {
		t.Fatal("expected a miss")
	}
}

// 搜索路径的顺序必须被尊重:同名工具取先出现的那个(PATH 顺序即优先级)。
// ③ 层能避开 Windows 的 sort/find,靠的正是这一条 —— usr\bin 前置就有意义了。
func TestWhichRespectsPathOrder(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(first, "probe-tool.exe"), []byte("MZ"), 0o755)
	os.WriteFile(filepath.Join(second, "probe-tool.exe"), []byte("MZ"), 0o755)

	sep := string(filepath.ListSeparator)
	got, ok := Which("probe-tool", first+sep+second)
	if !ok {
		t.Fatal("expected a hit")
	}
	if got != filepath.Join(first, "probe-tool.exe") {
		t.Fatalf("PATH order ignored: got %q", got)
	}
}

// 只有 PATHEXT 里的扩展名算命令。PATH 目录里同名的普通文件不该被选中 ——
// 那会让 ③ 层"找到一个跑不起来的东西"。
func TestWhichIgnoresNonExecutableExtensions(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "probe-tool"), []byte("data"), 0o644) // 无扩展名
	if _, ok := Which("probe-tool", dir); ok {
		t.Fatal("a plain file without a PATHEXT extension must not be treated as a command")
	}
}

func TestWhichCmdOutputsNameForBuiltins(t *testing.T) {
	for _, name := range []string{"ls", "cat", "grep", "which"} {
		var out, errw bytes.Buffer
		code := WhichCmd(context.Background(), "", []string{name}, &out, &errw)
		if code != errs.ExitOK {
			t.Errorf("%s: exit=%d err=%q", name, code, errw.String())
			continue
		}
		// 与 bash 的 `command -v` 对内建的做法一致:给名字,不是路径。
		if strings.TrimSpace(out.String()) != name {
			t.Errorf("%s: got %q", name, strings.TrimSpace(out.String()))
		}
	}
}

// ② 层的名字也要认得 —— `command -v svc` 不该说"没有"。
func TestWhichCmdOutputsNameForWinmgmtCommands(t *testing.T) {
	var out, errw bytes.Buffer
	code := WhichCmd(context.Background(), "", []string{"svc"}, &out, &errw)
	if code != errs.ExitOK {
		t.Fatalf("exit=%d err=%q", code, errw.String())
	}
	if strings.TrimSpace(out.String()) != "svc" {
		t.Fatalf("got %q", strings.TrimSpace(out.String()))
	}
}

// ③ 层给绝对路径;找不到要非零 + 说清楚,而不是安静地什么都不打印。
func TestWhichCmdReportsMissWithNonZero(t *testing.T) {
	var out, errw bytes.Buffer
	code := WhichCmd(context.Background(), "", []string{"definitely-not-installed-xyz"}, &out, &errw)
	if code == errs.ExitOK {
		t.Fatal("expected non-zero exit")
	}
	if out.String() != "" {
		t.Fatalf("nothing should reach stdout, got %q", out.String())
	}
	if !strings.Contains(errw.String(), "找不到") {
		t.Fatalf("expected an explicit message, got %q", errw.String())
	}
}

// 自带路径的写法要按**会话 cwd** 解析 —— Which 里 os.Stat 的基准是**进程** cwd,
// 而 `which` 的 cwd 参数是会话目录,两者通常不同。
//
// 修之前 `cd /tmp && which ./x.sh` 会说"找不到",而同一会话里 `./x.sh` 却能跑:
// 我们的错误信息恰好还让人拿 `which` 自查,那条指路就会是坏的。
func TestWhichCmdResolvesPathAgainstSessionCwd(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "x.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errw bytes.Buffer
	code := WhichCmd(context.Background(), dir, []string{"./x.sh"}, &out, &errw)
	if code != errs.ExitOK {
		t.Fatalf("exit=%d stderr=%q", code, errw.String())
	}
	if !strings.Contains(out.String(), "x.sh") {
		t.Fatalf("应在 stdout 给出解析后的路径,got %q", out.String())
	}
}

func TestWhichCmdWithoutArgsIsUsageError(t *testing.T) {	var out, errw bytes.Buffer
	if code := WhichCmd(context.Background(), "", nil, &out, &errw); code != errs.ExitUsage {
		t.Fatalf("exit=%d want %d", code, errs.ExitUsage)
	}
	if !strings.Contains(errw.String(), "用法") {
		t.Fatalf("expected a usage line, got %q", errw.String())
	}
}

// 搜索路径通过 ctx 进来(会话属性),不在 ctx 里时 ③ 层查不到任何东西 —— 这是等价的,
// 只是说明 white-box:这个测试钉住"ctx 是唯一入口"。
func TestWhichCmdUsesSearchPathFromContext(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "probe-tool.exe"), []byte("MZ"), 0o755)

	var out, errw bytes.Buffer
	ctx := WithSearchPath(context.Background(), dir)

	// 进程 PATH 里没有 probe-tool,所以只有 ctx 里的搜索路径能救它。
	if code := WhichCmd(ctx, "", []string{"probe-tool"}, &out, &errw); code != errs.ExitOK {
		t.Fatalf("exit=%d err=%q", code, errw.String())
	}
	if got := strings.TrimSpace(out.String()); got != filepath.Join(dir, "probe-tool.exe") {
		t.Fatalf("got %q", got)
	}
}
