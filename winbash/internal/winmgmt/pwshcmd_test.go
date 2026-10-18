package winmgmt

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"winbash/internal/errs"
	"winbash/internal/pwsh"
)

// 透明覆盖 pwsh 的测试。
//
// 背景:pwshx 那个项目(常驻 pwsh + 轻客户端,把 350ms 的启动成本变成一次)的残余价值
// 被收进 winbash 的 ② 层,见 pwshcmd.go。这些测试钉住三件事:
//
//  1. **接管哪几种形式**:-Command / -c / -File / 位置参数(含 -NoProfile 这类无害开关)。
//     位置参数按**文件路径**解释,与原生 pwsh 一致 —— 否则 `pwsh script.ps1` 会被
//     当成一段脚本文本来跑,行为与原生不同,那就不是"透明"覆盖了。
//  2. **不接管的形式必须报错**,不静默回落(Global Constraint:不认识的 flag 必须报错退出)。
//  3. **输出形状**:默认 TSV、--json 总是数组。值保证继续成立(数字就是数字、无 ANSI)。
//
// 还有两条不属于"形状"但必须钉住的,它们是同一个问题的两面:常驻引擎继承的是
// **它启动那一刻**的状态,而 ① 层的变化发生在那之后。
//
//   - 环境:bash 侧的 export 必须能被 pwsh 看到,否则 `export X=1; pwsh -c '$env:X'`
//     静默给出空值。
//   - cwd:会话的 `cd` 必须被 pwsh 看到,否则 `cd /tmp; pwsh -c 'New-Item x'`
//     静默把文件写进 daemon 的 cwd。

// runPwshCmd 是这些测试的统一入口:把 argv 交给 Pwsh,收齐它写的两个流。
func runPwshCmd(t *testing.T, conn *pwsh.Conn, jsonOut bool, args ...string) (stdout, stderr string, exit int) {
	t.Helper()
	return runPwshCmdCtx(t, context.Background(), conn, jsonOut, args...)
}

// runPwshCmdCtx 是带 ctx 的版本:会话的 cwd 和环境都靠 ctx 传(见 WithDir/WithEnv),
// 所以钉这两件事的测试必须能自己给 ctx。
func runPwshCmdCtx(t *testing.T, ctx context.Context, conn *pwsh.Conn, jsonOut bool, args ...string) (stdout, stderr string, exit int) {
	t.Helper()
	var out, errw strings.Builder
	exit = Pwsh(ctx, conn, args, jsonOut, &out, &errw)
	return out.String(), errw.String(), exit
}

// restoreEngineCwd 在测试结束时把引擎带回**测试进程**的 cwd。
//
// 引擎是整个包共享的一个常驻进程,`Set-Location` 会留在里面:不收拾的话,
// 后面不传 dir 的测试就跑在一个已被 t.TempDir 删掉的目录下 —— 成功与否
// 取决于测试执行顺序,那是最难查的一类失败。
func restoreEngineCwd(t *testing.T, conn *pwsh.Conn) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// 必须走 -c:位置参数在 Pwsh 里是**脚本文件路径**,裸 `$null` 会变成一个
		// 叫 `$null` 的文件名去 ReadFile(实测报「读不到脚本文件 …\$null」)。
		if _, errw, exit := runPwshCmdCtx(t, WithDir(context.Background(), wd), conn, false, "-c", "$null"); exit != errs.ExitOK {
			t.Errorf("收尾时没能把引擎 cwd 带回到 %q: %q", wd, errw)
		}
	})
}

// sameDir 判断两个路径是否指向同一个目录。
//
// 不能直接比字符串:`t.TempDir()` 可能给 8.3 短名(`C:\Users\TESTUS~1\…`),
// 而引擎经 Get-Item 解析后给长名(`C:\Users\Test User\…`) —— 同一个目录、两个字符串。
// os.SameFile 比的是文件身份,与命名法无关,这才是这条断言真正该问的问题。
func sameDir(t *testing.T, a, b string) bool {
	t.Helper()
	fa, err := os.Stat(a)
	if err != nil {
		t.Fatalf("stat %q: %v", a, err)
	}
	fb, err := os.Stat(b)
	if err != nil {
		t.Fatalf("stat %q: %v", b, err)
	}
	return os.SameFile(fa, fb)
}

// lines 把输出切成非空行,免得每个断言都要跟结尾换行较劲。
func lines(s string) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func TestPwshCommandFormIsTakenOver(t *testing.T) {
	conn := startTestPwsh(t)
	out, errw, exit := runPwshCmd(t, conn, false, "-c", "Write-Output hello")
	if exit != errs.ExitOK {
		t.Fatalf("exit=%d stderr=%q", exit, errw)
	}
	if got := lines(out); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("out=%q(想要一行 hello)", out)
	}
}

func TestPwshAcceptsHarmlessStartupSwitches(t *testing.T) {
	conn := startTestPwsh(t)
	// `pwsh -NoProfile -Command '...'` 是 AI 最顺手的写法,必须照样工作 ——
	// 引擎本来就无 profile、非交互,这些开关对它是恒真的,接受而不是报错。
	for _, args := range [][]string{
		{"-NoProfile", "-Command", "Write-Output a"},
		{"-NoLogo", "-c", "Write-Output b"},
		{"-NonInteractive", "-NoProfile", "-c", "Write-Output c"},
	} {
		out, errw, exit := runPwshCmd(t, conn, false, args...)
		if exit != errs.ExitOK {
			t.Fatalf("args=%v exit=%d stderr=%q", args, exit, errw)
		}
		if got := lines(out); len(got) != 1 {
			t.Fatalf("args=%v out=%q", args, out)
		}
	}
}

func TestPwshFileFormRunsTheFile(t *testing.T) {
	conn := startTestPwsh(t)
	dir := t.TempDir()
	script := filepath.Join(dir, "probe.ps1")
	if err := os.WriteFile(script, []byte("Write-Output from-file\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// -File 与"光给一个路径"是同一件事的两种写法,原生 pwsh 两者都认。
	for _, args := range [][]string{
		{"-File", script},
		{script},
	} {
		out, errw, exit := runPwshCmd(t, conn, false, args...)
		if exit != errs.ExitOK {
			t.Fatalf("args=%v exit=%d stderr=%q", args, exit, errw)
		}
		if got := lines(out); len(got) != 1 || got[0] != "from-file" {
			t.Fatalf("args=%v out=%q", args, out)
		}
	}
}

func TestPwshMissingFilePointsAtCommand(t *testing.T) {
	conn := startTestPwsh(t)
	// 位置参数不存在时,最常见的误用是"把一段脚本当位置参数给了"。
	// 报错必须说清该怎么改,而不是丢一句找不到文件。
	_, errw, exit := runPwshCmd(t, conn, false, "Write-Output hi")
	if exit == errs.ExitOK {
		t.Fatal("不存在的文件不该成功")
	}
	if !strings.Contains(errw, "-Command") {
		t.Fatalf("报错没告诉用户改用 -Command: %q", errw)
	}
}

func TestPwshObjectOutputIsTSVWithSortedColumns(t *testing.T) {
	conn := startTestPwsh(t)
	// 列取**键的并集**并排序:
	//  - 并集,因为 PowerShell 不同对象可以带不同属性(格式不齐的数组很常见);
	//  - 排序,因为 Go 的 map 迭代顺序随机,不排序的话两次运行列序会变 ——
	//    「同一命令每次输出列必须一致」这条在 ② 层是不让步的(见 renderRecords 的注释)。
	out, errw, exit := runPwshCmd(t, conn, false, "-c", `[ordered]@{Name="x";CPU=7}`)
	if exit != errs.ExitOK {
		t.Fatalf("exit=%d stderr=%q", exit, errw)
	}
	if got := lines(out); len(got) != 1 || got[0] != "7\tx" {
		t.Fatalf("out=%q(想要 7\\tx:列按 CPU,Name 排序)", out)
	}

	out, _, _ = runPwshCmd(t, conn, false, "-c", `[ordered]@{Name="x";CPU=7}`, "--header")
	if got := lines(out); len(got) != 2 || got[0] != "CPU\tName" {
		t.Fatalf("带表头时首行应是列名: %q", out)
	}
}

func TestPwshScalarArrayIsOnePerLine(t *testing.T) {
	conn := startTestPwsh(t)
	out, errw, exit := runPwshCmd(t, conn, false, "-c", "1..3")
	if exit != errs.ExitOK {
		t.Fatalf("exit=%d stderr=%q", exit, errw)
	}
	got := lines(out)
	if len(got) != 3 || got[0] != "1" || got[2] != "3" {
		t.Fatalf("out=%q(想要三行 1/2/3)", out)
	}
}

func TestPwshSingleScalarIsBareNotAOneCellTable(t *testing.T) {
	conn := startTestPwsh(t)
	// 这是 spec 点名的值保证:`reg query` 给 "0xa"(十六进制字符串),这里必须是 10。
	out, errw, exit := runPwshCmd(t, conn, false, "-c",
		`(Get-ItemProperty "HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion").CurrentMajorVersionNumber`)
	if exit != errs.ExitOK {
		t.Fatalf("exit=%d stderr=%q", exit, errw)
	}
	if got := strings.TrimRight(out, "\n"); got != "10" {
		t.Fatalf("out=%q(想要裸的 10,不是 0xa、也不带列名)", got)
	}
}

func TestPwshJSONIsAlwaysAnArray(t *testing.T) {
	conn := startTestPwsh(t)
	cases := []struct{ script, want string }{
		// 单个对象包成数组:形状不能随数据条数变化(单元素退化是同一个坑的另一种形态)。
		{`[ordered]@{a=1}`, `[{"a":1}]`},
		{`1..2`, `[1,2]`},
		{`Write-Output 5`, `[5]`},
		{`$null`, `[]`},
	}
	for _, c := range cases {
		out, errw, exit := runPwshCmd(t, conn, true, "-c", c.script)
		if exit != errs.ExitOK {
			t.Fatalf("script=%s exit=%d stderr=%q", c.script, exit, errw)
		}
		if got := strings.TrimSpace(out); got != c.want {
			t.Fatalf("script=%s out=%q want=%q", c.script, got, c.want)
		}
	}
}

func TestPwshUnsupportedFormsExitUsageAndMentionNativePwsh(t *testing.T) {
	conn := startTestPwsh(t)
	// 不接管的形式必须**明确报错**。静默回落(悄悄跑原生 pwsh)看似友好,
	// 实际会让人以为"反正都一样",而两者在启动成本上差 20 倍 —— 那才是欺骗。
	for _, args := range [][]string{
		{},
		{"-EncodedCommand", "VwByAGkAdABlAC0ATwB1AHQAcAB1AHQAIABoAGkA"},
		{"-Version"},
	} {
		_, errw, exit := runPwshCmd(t, conn, false, args...)
		if exit != errs.ExitUsage {
			t.Fatalf("args=%v exit=%d(想要 %d)stderr=%q", args, exit, errs.ExitUsage, errw)
		}
		if !strings.Contains(errw, "原生") {
			t.Fatalf("args=%v 报错没指路到原生 pwsh: %q", args, errw)
		}
	}

	// 开关给了却没给值,是用法错误(不属于"不接管的形式"),钉退出码就够了。
	if _, _, exit := runPwshCmd(t, conn, false, "-Command"); exit != errs.ExitUsage {
		t.Fatalf("-Command 缺值 exit=%d(想要 %d)", exit, errs.ExitUsage)
	}
}

func TestPwshVariablesSurviveAcrossCalls(t *testing.T) {
	conn := startTestPwsh(t)
	if _, errw, exit := runPwshCmd(t, conn, false, "-c", `$winbash_probe = 'kept'`); exit != errs.ExitOK {
		t.Fatalf("第一次调用失败: %q", errw)
	}
	out, errw, exit := runPwshCmd(t, conn, false, "-c", "$winbash_probe")
	if exit != errs.ExitOK {
		t.Fatalf("第二次调用失败: %q", errw)
	}
	// 这是常驻会话买到的东西之一(裸 pwsh 每次调用都是新进程)。它也意味着
	// 变量会在不相干的调用之间泄漏 —— 见 README 的会话级状态一节。
	if got := strings.TrimRight(out, "\n"); got != "kept" {
		t.Fatalf("out=%q(想要 kept:变量没跨调用保持)", got)
	}
}

func TestPwshNonTerminatingErrorFails(t *testing.T) {
	conn := startTestPwsh(t)
	// PowerShell 的 non-terminating error 默认不改退出码。透传命令如果不查
	// $Error,上游会以为成功了 —— 这正是 spec §7 第 1 条要消灭的形态。
	_, errw, exit := runPwshCmd(t, conn, false, "-c", "Get-Service -Name winbash-no-such-service-xyz")
	if exit == errs.ExitOK {
		t.Fatalf("非终止错误被当成了成功: stderr=%q", errw)
	}
	if strings.TrimSpace(errw) == "" {
		t.Fatal("失败了却没有错误信息")
	}
}

// 会话的 cd 必须被 ② 层的 pwsh 看见。
//
// 这里钉的是一个修过的**静默**缺陷:引擎常驻,它的 cwd 是 daemon 启动时继承的
// 那个,而会话的 `cd` 发生在之后。不同步的话 `cd /tmp; pwsh -c 'New-Item x'`
// 会把文件写到 daemon 的 cwd —— 命令成功、输出干净,错的东西静静落了地。
func TestPwshFollowsSessionDir(t *testing.T) {
	conn := startTestPwsh(t)
	restoreEngineCwd(t, conn)
	dir := t.TempDir()

	out, errw, exit := runPwshCmdCtx(t, WithDir(context.Background(), dir), conn, false,
		"-c", "Get-Location | Select-Object -ExpandProperty Path")
	if exit != errs.ExitOK {
		t.Fatalf("exit=%d stderr=%q", exit, errw)
	}
	// 单标量是裸值、不套列名(见 renderAny),所以这里就应该是干净的一行路径。
	got := lines(out)
	if len(got) != 1 || !sameDir(t, strings.TrimSpace(got[0]), dir) {
		t.Fatalf("out=%q(想要 %q:引擎没跟随会话的 cwd)", out, dir)
	}
}

// 相对路径的脚本文件也必须按**会话**的 cwd 解析。
//
// os.ReadFile 解析相对路径用的是**进程** cwd(daemon 的),不是会话的 ——
// 不先拼绝对路径,`cd /tmp; pwsh probe.ps1` 会报「读不到脚本文件」,
// 而文件明明就在那儿。
func TestPwshRelativeScriptFileUsesSessionDir(t *testing.T) {
	conn := startTestPwsh(t)
	restoreEngineCwd(t, conn)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "probe.ps1"),
		[]byte("Write-Output \"from-session-dir\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, errw, exit := runPwshCmdCtx(t, WithDir(context.Background(), dir), conn, false, "probe.ps1")
	if exit != errs.ExitOK {
		t.Fatalf("exit=%d stderr=%q(相对路径没按会话 cwd 找)", exit, errw)
	}
	if got := lines(out); len(got) != 1 || got[0] != "from-session-dir" {
		t.Fatalf("out=%q(想要一行 from-session-dir)", out)
	}
}

// 目录名里的单引号必须过 psQuote 的转义。
//
// 直接拼进脚本的话 `Set-Location -LiteralPath 'C:\a'b'` 会在引号处断开:
// 轻则解析错误,重则路径被截成 `C:\a` 而**静默成功**。
func TestPwshSessionDirWithQuoteIsEscaped(t *testing.T) {
	conn := startTestPwsh(t)
	restoreEngineCwd(t, conn)
	dir := filepath.Join(t.TempDir(), "a'b")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	out, errw, exit := runPwshCmdCtx(t, WithDir(context.Background(), dir), conn, false,
		"-c", "Get-Location | Select-Object -ExpandProperty Path")
	if exit != errs.ExitOK {
		t.Fatalf("exit=%d stderr=%q", exit, errw)
	}
	got := lines(out)
	if len(got) != 1 || !sameDir(t, strings.TrimSpace(got[0]), dir) {
		t.Fatalf("out=%q(想要 %q:带单引号的目录名没被正确转义)", out, dir)
	}
}

func TestPwshInheritsBashExportedEnv(t *testing.T) {
	conn := startTestPwsh(t)
	ctx := WithEnv(context.Background(), []string{"WINBASH_ENV_PROBE=42"})
	var out, errw strings.Builder
	if exit := Pwsh(ctx, conn, []string{"-c", "$env:WINBASH_ENV_PROBE"}, false, &out, &errw); exit != errs.ExitOK {
		t.Fatalf("exit=%d stderr=%q", exit, errw.String())
	}
	if got := strings.TrimRight(out.String(), "\n"); got != "42" {
		t.Fatalf("out=%q(想要 42:常驻引擎看不到 bash 侧的 export)", got)
	}
}

// 带 param(...) 的脚本文件必须能跑。
//
// 这里钉的是一个修过的缺陷,不是"顺手多测一条":PowerShell 要求 `param(...)` 是
// scriptblock 的**第一条**语句。环境同步过去是拼在脚本文本**前面**的一段
// `[Environment]::SetEnvironmentVariable(...)`,于是 param 不再是第一条,pwsh 报
//
//	The term 'param' is not recognized as a name of a cmdlet...
//
// (实测;同一个文件用原生 `pwsh -File` 完全正常)。现在环境走协议字段(session.ps1
// 在脚本之外设),这条前缀也就不存在了。
//
// 必须带上 WithEnv:没有环境变量时老实现的 envPrelude 返回空串,
// 这条测试对那个缺陷就是假绿的。
func TestPwshScriptWithParamBlockRuns(t *testing.T) {
	conn := startTestPwsh(t)
	restoreEngineCwd(t, conn)
	dir := t.TempDir()
	// $Name 是 PowerShell 变量,原样写进文件(Go 不会对普通字符串字面量插值)。
	body := "param($Name = \"default\")\nWrite-Output \"hello-$Name\"\n"
	if err := os.WriteFile(filepath.Join(dir, "param.ps1"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	// 相对文件名:顺带复用上一轮做好的"按会话 cwd 解析脚本路径"。
	ctx := WithEnv(WithDir(context.Background(), dir), []string{"WINBASH_PARAM_PROBE=1"})
	out, errw, exit := runPwshCmdCtx(t, ctx, conn, false, "param.ps1")
	if exit != errs.ExitOK {
		t.Fatalf("exit=%d stderr=%q(带 param 块的脚本跑不了:环境又回到脚本前导了?)", exit, errw)
	}
	if got := lines(out); len(got) != 1 || got[0] != "hello-default" {
		t.Fatalf("out=%q(想要一行 hello-default:param 的默认值没生效)", out)
	}
}

// `#Requires` 同样必须是脚本的第一条语句,所以它也是同一处缺陷的另一个面。
func TestPwshScriptWithRequiresRuns(t *testing.T) {
	conn := startTestPwsh(t)
	restoreEngineCwd(t, conn)
	dir := t.TempDir()
	body := "#Requires -Version 5.1\nWrite-Output \"requires-ok\"\n"
	if err := os.WriteFile(filepath.Join(dir, "requires.ps1"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := WithEnv(WithDir(context.Background(), dir), []string{"WINBASH_REQUIRES_PROBE=1"})
	out, errw, exit := runPwshCmdCtx(t, ctx, conn, false, "requires.ps1")
	if exit != errs.ExitOK {
		t.Fatalf("exit=%d stderr=%q(#Requires 开头的脚本跑不了)", exit, errw)
	}
	if got := lines(out); len(got) != 1 || got[0] != "requires-ok" {
		t.Fatalf("out=%q(想要一行 requires-ok)", out)
	}
}
