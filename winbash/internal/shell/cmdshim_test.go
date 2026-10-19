package shell

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"winbash/internal/errs"
)

// 这些用例钉的是「翻译器的边界」,不是「npm 能不能跑」——后者属于端到端,
// 在真机上由 smoke 与手工验收覆盖。这里全部用临时目录造垫片,所以与装了什么
// 工具无关,换台机器也能跑。
//
// 判据统一是:能翻译的形状必须给出**确定的** prog+args;不能翻译的必须 ok=false
// (调用方据此保持拒绝),而不是给出一个"差不多"的猜测。

// writeFile 在 dir 下写一个文件,顺带建父目录。
func writeShim(t *testing.T, dir, rel, body string) string {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// mkNode 造一个假的 node.exe —— 只要求存在,翻译器不读它的内容。
func mkNode(t *testing.T, dir string) string {
	t.Helper()
	return writeShim(t, dir, "node.exe", "MZ")
}

// ① npm 9/10 与 pnpm 的垫片形态。它有两个特点最容易把解析器绕晕:
//   - 程序名与环境变量都藏在一层 `SET` 背后(`%NODE_EXE%` / `%NPM_CLI_JS%`)
//   - 最后一个变量有**两个候选**,第二个依赖 `FOR /F` 的运行时值
//
// 翻译器必须取到第一个候选(npm-cli.js),并把第二个丢掉 —— 而不是因为第二个
// 展开不出来就整条放弃。
func TestResolveShimHandlesNpmModernShape(t *testing.T) {
	dir := t.TempDir()
	mkNode(t, dir)
	cli := writeShim(t, dir, "node_modules/npm/bin/npm-cli.js", "// npm")

	shim := writeShim(t, dir, "npm.cmd", `:: Created by npm, please don't edit manually.
@ECHO OFF
SETLOCAL
SET "NODE_EXE=%~dp0\node.exe"
IF NOT EXIST "%NODE_EXE%" (
  SET "NODE_EXE=node"
)
SET "NPM_CLI_JS=%~dp0\node_modules\npm\bin\npm-cli.js"
FOR /F "delims=" %%F IN ('CALL "%NODE_EXE%" "%NPM_PREFIX_JS%"') DO (
  SET "NPM_PREFIX_NPM_CLI_JS=%%F\node_modules\npm\bin\npm-cli.js"
)
IF EXIST "%NPM_PREFIX_NPM_CLI_JS%" (
  SET "NPM_CLI_JS=%NPM_PREFIX_NPM_CLI_JS%"
)
"%NODE_EXE%" "%NPM_CLI_JS%" %*
`)

	call, ok := resolveNodeShim(shim, dir)
	if !ok {
		t.Fatal("npm 垫片应当能翻译")
	}
	if !strings.EqualFold(call.Prog, filepath.Join(dir, "node.exe")) {
		t.Errorf("prog = %q, want 同目录 node.exe", call.Prog)
	}
	if len(call.Args) != 1 || !strings.EqualFold(call.Args[0], cli) {
		t.Errorf("args = %#v, want [%s]", call.Args, cli)
	}
}

// ② npm 7/8 的简版形态:一行完事,程序与脚本都是内联的 %dp0%。
func TestResolveShimHandlesNpmSimpleShape(t *testing.T) {
	dir := t.TempDir()
	mkNode(t, dir)
	cli := writeShim(t, dir, "node_modules/pkg/bin/x.js", "// x")

	shim := writeShim(t, dir, "pkg.cmd", `@ECHO off
GOTO start
:find_dp0
SET dp0=%~dp0
EXIT /b
:start
SETLOCAL
CALL :find_dp0
"%dp0%\node.exe"  "%dp0%\node_modules\pkg\bin\x.js" %*
`)

	call, ok := resolveNodeShim(shim, dir)
	if !ok {
		t.Fatal("简单形态应当能翻译")
	}
	if !strings.EqualFold(call.Prog, filepath.Join(dir, "node.exe")) {
		t.Errorf("prog = %q", call.Prog)
	}
	if len(call.Args) != 1 || !strings.EqualFold(call.Args[0], cli) {
		t.Errorf("args = %#v", call.Args)
	}
}

// ③ 直接 exec 一个原生 exe 的垫片(claude.cmd 就是这种):
// 程序是 .exe、没有脚本参数。它同样该被翻译,而不是逼使用者去写 cmd /c。
func TestResolveShimHandlesDirectExeShape(t *testing.T) {
	dir := t.TempDir()
	target := writeShim(t, dir, "node_modules/@scope/pkg/bin/tool.exe", "MZ")

	shim := writeShim(t, dir, "tool.cmd", `@ECHO off
GOTO start
:find_dp0
SET dp0=%~dp0
EXIT /b
:start
SETLOCAL
CALL :find_dp0
"%dp0%\node_modules\@scope\pkg\bin\tool.exe"   %*
`)

	call, ok := resolveNodeShim(shim, dir)
	if !ok {
		t.Fatal("直接 exec exe 的垫片应当能翻译")
	}
	if !strings.EqualFold(call.Prog, target) {
		t.Errorf("prog = %q, want %q", call.Prog, target)
	}
	if len(call.Args) != 0 {
		t.Errorf("这种形状不该有前置参数,got %#v", call.Args)
	}
}

// npm 垫片里的 ELSE 分支是 `SET "_prog=node"` —— 裸名字。同目录没有 node.exe 时
// 应当回落到 PATH 上的 node。
func TestResolveShimFallsBackToNodeOnPath(t *testing.T) {
	dir := t.TempDir() // 垫片目录**没有** node.exe
	pathDir := t.TempDir()
	node := mkNode(t, pathDir)
	cli := writeShim(t, dir, "node_modules/pkg/bin/x.js", "// x")

	shim := writeShim(t, dir, "pkg.cmd", `@ECHO off
SET "_prog=node"
SET "_cli=%~dp0\node_modules\pkg\bin\x.js"
"%_prog%" "%_cli%" %*
`)

	call, ok := resolveNodeShim(shim, pathDir)
	if !ok {
		t.Fatal("裸 node 应当通过 searchPath 解析")
	}
	if !strings.EqualFold(call.Prog, node) {
		t.Errorf("prog = %q, want %q", call.Prog, node)
	}
	if len(call.Args) != 1 || !strings.EqualFold(call.Args[0], cli) {
		t.Errorf("args = %#v", call.Args)
	}
}

// ── 拒绝路径:认不出来就 ok=false,绝不能猜 ───────────────────────────────

func TestResolveShimRejectsUnknownShapes(t *testing.T) {
	dir := t.TempDir()
	mkNode(t, dir)
	writeShim(t, dir, "node_modules/pkg/bin/x.js", "// x")

	cases := []struct {
		name string
		body string
	}{
		{
			// 不是「以 %* 结尾」的形态:它是交互式批处理,不是垫片。
			name: "没有 %* 结尾",
			body: "@echo off\n\"%~dp0\\node.exe\" \"%~dp0\\x.js\"\n",
		},
		{
			// 引号外的片段里有管道 —— 这正是不能交给 cmd 的那类东西。
			name: "引号外有管道",
			body: "\"%~dp0\\node.exe\" \"%~dp0\\x.js\" %* | findstr x\n",
		},
		{
			// 未知变量:值只有运行时才知道,不该替它猜。
			name: "未知变量",
			body: "\"%~dp0\\node.exe\" \"%SOMETHING_ELSE%\" %*\n",
		},
		{
			// 目录穿越:翻译出来的路径必须仍在垫片目录之内。
			name: "目录穿越",
			body: "\"%~dp0\\node.exe\" \"%~dp0\\..\\..\\evil.js\" %*\n",
		},
		{
			// 脚本不存在 —— 说明这条垫片依赖别的东西"碰巧在",不是我们的形状。
			name: "脚本不存在",
			body: "\"%~dp0\\node.exe\" \"%~dp0\\missing.js\" %*\n",
		},
		{
			// 引号没闭合。
			name: "引号不闭合",
			body: "\"%~dp0\\node.exe\" \"%~dp0\\x.js %*\n",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			shim := writeShim(t, dir, "bad.cmd", c.body)
			if call, ok := resolveNodeShim(shim, dir); ok {
				t.Fatalf("这种形状必须拒绝,却翻译成了 %+v", call)
			}
		})
	}
}

// 两个候选指向**不同**的脚本:说明它到运行时才决定跑哪个。
// 替它做主就会跑错东西,所以必须拒绝。
func TestResolveShimRejectsAmbiguousScriptCandidates(t *testing.T) {
	dir := t.TempDir()
	mkNode(t, dir)
	a := writeShim(t, dir, "a.js", "// a")
	b := writeShim(t, dir, "b.js", "// b")

	shim := writeShim(t, dir, "amb.cmd", `@ECHO off
IF EXIST "%~dp0\a.js" (
  SET "_cli=%~dp0\a.js"
) ELSE (
  SET "_cli=%~dp0\b.js"
)
"%~dp0\node.exe" "%_cli%" %*
`)
	call, ok := resolveNodeShim(shim, dir)
	if ok {
		t.Fatalf("两个不同候选必须拒绝,got %+v (a=%s b=%s)", call, a, b)
	}
}

// 两个候选指向**同一个**文件(IF EXIST 的两支恰好一样)时不该被当成歧义。
func TestResolveShimAcceptsDuplicateCandidates(t *testing.T) {
	dir := t.TempDir()
	mkNode(t, dir)
	cli := writeShim(t, dir, "x.js", "// x")

	shim := writeShim(t, dir, "dup.cmd", `@ECHO off
IF EXIST "%~dp0\x.js" (
  SET "_cli=%~dp0\x.js"
) ELSE (
  SET "_cli=%~dp0\x.js"
)
"%~dp0\node.exe" "%_cli%" %*
`)
	call, ok := resolveNodeShim(shim, dir)
	if !ok {
		t.Fatal("同一路径的两个候选不算歧义")
	}
	if len(call.Args) != 1 || !strings.EqualFold(call.Args[0], cli) {
		t.Errorf("args = %#v", call.Args)
	}
}

// 兜底开关默认关 —— 这条很重要:打开它会把参数交给 cmd 重新解析,
// 与「不静默做错事」冲突,所以必须是使用者显式选择的。
func TestCmdShellFallbackIsOffByDefault(t *testing.T) {
	env := map[string]string{}
	get := func(k string) string { return env[k] }
	if cmdShellFallbackEnabled(get) {
		t.Fatal("未设变量时兜底必须是关的")
	}
	for _, v := range []string{"1", "true", "YES", "on"} {
		env[envCmdShellFallback] = v
		if !cmdShellFallbackEnabled(get) {
			t.Errorf("%s=%q 应当打开兜底", envCmdShellFallback, v)
		}
	}
	for _, v := range []string{"0", "false", "", "  ", "nope"} {
		env[envCmdShellFallback] = v
		if cmdShellFallbackEnabled(get) {
			t.Errorf("%s=%q 不该打开兜底", envCmdShellFallback, v)
		}
	}
}

// 端到端:自造一条 npm 形状的垫片,真的跑起来,验证**使用者参数原样到达**。
//
// 为什么必须端到端:解析器的单元测试只能证明"翻译结果对",证不到"exec 拼出来的
// 命令行也对" —— 而后者才是 `cmd /c` 与直调的分水岭(`&` `%` `^` 会不会被二次解释)。
// 这里刻意不依赖装了什么 npm,只用 PATH 上的 node,所以换台机器也能跑。
func TestShimEndToEndPassesArgumentsVerbatim(t *testing.T) {
	if _, ok := whichOnPath("node.exe", os.Getenv("PATH")); !ok {
		t.Skip("PATH 上没有 node.exe,跳过")
	}
	dir := t.TempDir()
	// 脚本把收到的参数用 | 连起来打印,便于逐字比对。
	writeShim(t, dir, "node_modules/pkg/bin/argv.js",
		"console.log(process.argv.slice(2).join('|'));\n")
	shim := writeShim(t, dir, "pkg.cmd", `@ECHO off
SETLOCAL
IF EXIST "%~dp0\node.exe" (
  SET "_prog=%~dp0\node.exe"
) ELSE (
  SET "_prog=node"
)
SET "_cli=%~dp0\node_modules\pkg\bin\argv.js"
"%_prog%" "%_cli%" %*
`)

	s := NewSession()
	// 这四个参数是 cmd /c 那条路会搞坏的典型:空格、&、%、^。
	script := "'" + filepath.ToSlash(shim) + "' 'a b' 'c&d' 'e%F%g' 'h^i'"
	out, exit, errs := s.Run(context.Background(), script, false)
	if exit != 0 {
		t.Fatalf("exit=%d errs=%v out=%q", exit, errs, out)
	}
	want := "a b|c&d|e%F%g|h^i\n"
	if out != want {
		t.Fatalf("参数没有原样到达:\n got %q\nwant %q", out, want)
	}
}

// 认不出来的批处理仍然响亮拒绝,且退出码是 126(不可执行),不是 127(找不到)。
func TestUnrecognizedBatchIsStillRejected(t *testing.T) {
	dir := t.TempDir()
	bat := writeShim(t, dir, "plain.bat", "@echo off\necho ran %*\n")

	s := NewSession()
	out, exit, errLines := s.Run(context.Background(), "'"+filepath.ToSlash(bat)+"' x", false)
	if exit != errs.ExitNoExec {
		t.Fatalf("exit=%d want %d; out=%q errs=%v", exit, errs.ExitNoExec, out, errLines)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("被拒绝的批处理不该产生输出, got %q", out)
	}
	joined := strings.Join(errLines, "\n")
	if !strings.Contains(joined, "WINBASH_CMD_SHELL") {
		t.Errorf("错误信息应当给出兜底开关, got %v", errLines)
	}
}
