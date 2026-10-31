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

// ★ `which -a` 列**所有**匹配位置,而不是把 -a 当成一个名字。
//
// 修之前:WhichCmd 把 argv 当名字逐个搜,于是 `which -a ls` 会
//  1. 打出 ls 的位置(正确),
//  2. 再对名字 "-a" 报一句「找不到: -a」并返回 exit 1。
//
// 那份「既有输出又报错」的组合最容易被误读成"某个名字没找到",而 AI 常用
// `which -a python` 判断装了几个版本 —— 拿到输出却被退出码告知失败。
func TestWhichCmdAllListsEveryMatch(t *testing.T) {
	// 两个目录各放一个同名可执行文件,按 order 拼进搜索路径。
	first, second := t.TempDir(), t.TempDir()
	for _, d := range []string{first, second} {
		if err := os.WriteFile(filepath.Join(d, "probe-all.exe"), []byte("MZ"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ctx := WithSearchPath(context.Background(), first+string(filepath.ListSeparator)+second)

	var out, errw bytes.Buffer
	code := WhichCmd(ctx, "", []string{"-a", "probe-all"}, &out, &errw)
	if code != errs.ExitOK {
		t.Fatalf("which -a 应当 exit 0,got %d stderr=%q", code, errw.String())
	}
	lines := strings.Fields(strings.TrimSpace(out.String()))
	if len(lines) != 2 {
		t.Fatalf("两个目录各有一个,应当输出 2 行,got %d: %q", len(lines), out.String())
	}
	// 顺序必须按 PATH 顺序(先 first,后 second)。
	if !strings.Contains(strings.ToLower(lines[0]), strings.ToLower(filepath.Base(first))) {
		t.Errorf("第一行应当来自 %s,got %q", first, lines[0])
	}
	// stderr 必须干净 —— 这正是修之前坏掉的地方。
	if strings.Contains(errw.String(), "-a") {
		t.Fatalf("stderr 不该提到 -a(它是个开关,不是名字): %q", errw.String())
	}

	// 不带 -a 时只给第一处(原有行为不能变)。
	out.Reset()
	errw.Reset()
	if code := WhichCmd(ctx, "", []string{"probe-all"}, &out, &errw); code != errs.ExitOK {
		t.Fatalf("which probe-all: exit=%d", code)
	}
	if n := len(strings.Fields(strings.TrimSpace(out.String()))); n != 1 {
		t.Fatalf("不带 -a 时只该给一行,got %d: %q", n, out.String())
	}

	// 不认识的开关照旧响亮拒绝(不静默当名字 —— 项目底线)。
	out.Reset()
	errw.Reset()
	if code := WhichCmd(ctx, "", []string{"-Z", "probe-all"}, &out, &errw); code != errs.ExitUsage {
		t.Fatalf("未知开关应当 exit %d,got %d", errs.ExitUsage, code)
	}
}

func TestWhichCmdWithoutArgsIsUsageError(t *testing.T) {
	var out, errw bytes.Buffer
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

// --layers 是诊断开关:逐层报告命中/未命中,而不是回答"命令落到哪"。
//
// 它必须**早于** ①② 层的提前返回处理 —— 修之前放在后面,`which --layers ls`
// 退化成普通 `which ls`(只打一行 "ls"),实测踩过。这里钉住四件事:
// 三层各有一行、① 层命中时不吞掉后两层、全部未命中时给出结论且 exit 1、
// 以及与 --layers 同名的未知开关仍然响亮拒绝。
func TestWhichLayersReportsEveryLayer(t *testing.T) {
	dir := t.TempDir()
	hit := filepath.Join(dir, "probe-layers.exe")
	if err := os.WriteFile(hit, []byte("MZ"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := WithSearchPath(context.Background(), dir)

	t.Run("①层命中也要报出③层的结果", func(t *testing.T) {
		var out, errw bytes.Buffer
		if code := WhichCmd(ctx, "", []string{"--layers", "ls"}, &out, &errw); code != errs.ExitOK {
			t.Fatalf("exit=%d want 0, stderr=%q", code, errw.String())
		}
		s := out.String()
		// ① 层命中只是第一行,不能像默认路径那样 continue 掉后两层。
		if strings.Count(s, "\n") < 3 {
			t.Fatalf("--layers 必须三层各一行, got %q", s)
		}
		for _, want := range []string{"① 内建", "② 内建", "③ PATH"} {
			if !strings.Contains(s, want) {
				t.Errorf("缺少 %q 这一行: %q", want, s)
			}
		}
	})

	t.Run("③层命中给绝对路径", func(t *testing.T) {
		var out, errw bytes.Buffer
		if code := WhichCmd(ctx, "", []string{"--layers", "probe-layers"}, &out, &errw); code != errs.ExitOK {
			t.Fatalf("exit=%d want 0", code)
		}
		if !strings.Contains(out.String(), hit) {
			t.Fatalf("③ 层应当报出 %s, got %q", hit, out.String())
		}
	})

	t.Run("全部未命中给结论且非零", func(t *testing.T) {
		var out, errw bytes.Buffer
		code := WhichCmd(ctx, "", []string{"--layers", "probe-definitely-absent"}, &out, &errw)
		if code != errs.ExitError {
			t.Fatalf("exit=%d want %d", code, errs.ExitError)
		}
		if !strings.Contains(out.String(), "所有层都没有") {
			t.Fatalf("应当给出结论行, got %q", out.String())
		}
		// 诊断模式下不再额外打一条「找不到: x」的错误行 —— 结论行已经够了,
		// 两条一起出现会让人以为有两个问题。
		if strings.Contains(errw.String(), "找不到") {
			t.Fatalf("stderr 不该重复报「找不到」: %q", errw.String())
		}
	})

	t.Run("多名字逐个诊断", func(t *testing.T) {
		var out, errw bytes.Buffer
		WhichCmd(ctx, "", []string{"--layers", "probe-layers", "probe-absent"}, &out, &errw)
		if n := strings.Count(out.String(), "结论"); n != 1 {
			t.Fatalf("只有一个名字缺失,应当只有一条结论行, got %d: %q", n, out.String())
		}
	})

	t.Run("与未知开关一样响亮拒绝", func(t *testing.T) {
		var out, errw bytes.Buffer
		if code := WhichCmd(ctx, "", []string{"--layer", "ls"}, &out, &errw); code != errs.ExitUsage {
			t.Fatalf("--layer(少个 s)应当 exit %d, got %d", errs.ExitUsage, code)
		}
	})
}

// ★ `--` 之后全是名字(照 POSIX 惯例,与 ls/cat/grep 的 -- 对齐)。
//
// 修之前 case "--" 只有注释没有状态:-- 本身被消费掉,但后面的 -a 仍走 flag 分支
// (被当成开关)、-x 仍走 default 被拒 —— `which -- -a` 最终报「需要至少一个名字」
// (exit 2)。修之后 -- 之后的参数全部当名字;名字找不到时的行为照旧
// (报「找不到: 名字」、exit 1),不因 -- 改变。
func TestWhichCmdDoubleDashMakesEverythingAfterItAName(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "probe-dd.exe"), []byte("MZ"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := WithSearchPath(context.Background(), dir)

	// -- 之后的 -a 按名字搜:目录里没有叫 -a 的可执行文件,应报「找不到: -a」
	// 且 exit 1 —— 关键是不能再把 -a 当开关消费掉(否则是「需要至少一个名字」exit 2)。
	var out, errw bytes.Buffer
	code := WhichCmd(ctx, "", []string{"--", "-a"}, &out, &errw)
	if code != errs.ExitError {
		t.Fatalf("-- 之后的 -a 应当按名字搜, got code=%d stderr=%q", code, errw.String())
	}
	if !strings.Contains(errw.String(), "找不到: -a") {
		t.Fatalf("应当按名字 -a 报找不到, got %q", errw.String())
	}

	// 名字在 -- 之后照常找得到。
	out.Reset()
	errw.Reset()
	if code := WhichCmd(ctx, "", []string{"--", "probe-dd"}, &out, &errw); code != errs.ExitOK {
		t.Fatalf("exit=%d stderr=%q", code, errw.String())
	}
	if !strings.Contains(out.String(), "probe-dd.exe") {
		t.Fatalf("-- 之后的名字应当命中, got %q", out.String())
	}

	// 实际场景:有一个名字就叫 -a 的命令。开关在前、名字在 -- 之后 ——
	// `which -a -- -a` 的第一个 -a 是开关(列所有位置),-- 之后的 -a 是名字。
	exeA := filepath.Join(dir, "-a.exe")
	if err := os.WriteFile(exeA, []byte("MZ"), 0o755); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errw.Reset()
	if code := WhichCmd(ctx, "", []string{"-a", "--", "-a"}, &out, &errw); code != errs.ExitOK {
		t.Fatalf("exit=%d stderr=%q", code, errw.String())
	}
	if !strings.Contains(out.String(), exeA) {
		t.Fatalf("应当找到名为 -a 的可执行文件, got %q", out.String())
	}
}
