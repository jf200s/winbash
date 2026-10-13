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
// 还有一条不属于"形状"但必须钉住的:bash 侧的 export 必须能被 pwsh 看到。
// 常驻引擎继承的是**启动那一刻**的环境,而 ① 层的 export 发生在之后 ——
// 不显式同步的话 `export X=1; pwsh -c '$env:X'` 会静默给出空值。

// runPwshCmd 是这些测试的统一入口:把 argv 交给 Pwsh,收齐它写的两个流。
func runPwshCmd(t *testing.T, conn *pwsh.Conn, jsonOut bool, args ...string) (stdout, stderr string, exit int) {
	t.Helper()
	var out, errw strings.Builder
	exit = Pwsh(context.Background(), conn, args, jsonOut, &out, &errw)
	return out.String(), errw.String(), exit
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
