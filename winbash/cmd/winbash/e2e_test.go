package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 这个文件里测的是**跨进程**的事实:它真的构建并运行 winbash.exe。
//
// 为什么必须这样:项目的一条主判据是"不等待"(spec §9),而它取决于
// 「client 每次都新建、daemon 常驻」这件事 —— 包内调用看不见它。

// buildWinbash 构建一个测试专用的可执行文件。
func buildWinbash(t *testing.T) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "winbash.exe")
	cmd := exec.Command("go", "build", "-o", exe, ".")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("构建 winbash 失败: %v\n%s", err, stderr.String())
	}
	return exe
}

// benchEndpoint 给每次测试一个**独立**的端口文件。
//
// 这一条不能省:默认端口文件是按用户名共享的,测试若用它,就会停掉、或者抢用
// 使用者正在跑的那个 daemon。
func benchEndpoint(t *testing.T) string {
	return filepath.Join(t.TempDir(), "bench.port")
}

func runWinbash(t *testing.T, exe, endpoint string, args ...string) (string, int) {
	t.Helper()
	full := append([]string{"--endpoint", endpoint}, args...)
	cmd := exec.Command(exe, full...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("运行 %v: %v", full, err)
	}
	return out.String(), code
}

func mustStatus(t *testing.T, exe, endpoint string) string {
	t.Helper()
	out, code := runWinbash(t, exe, endpoint, "--status")
	if code != 0 {
		t.Fatalf("--status 失败 exit=%d out=%q", code, out)
	}
	return out
}

// pidFromStatus 从 `--status` 的输出里取 pid。
func pidFromStatus(t *testing.T, out string) int {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "pid="); ok {
			pid, err := strconv.Atoi(v)
			if err != nil {
				t.Fatalf("status 里的 pid 解析不了: %q", line)
			}
			return pid
		}
	}
	t.Fatalf("status 输出里没有 pid= 行: %q", out)
	return 0
}

// "不等待"的**机制**:多个 client 进程必须复用同一个 daemon。
//
// 断言钉在 pid 上而不是时间上:时间随机器负载浮动,而"pid 不变"是确定的
// —— 时间数字见下面那条要显式开启的报告。
func TestDaemonIsReusedAcrossClientProcesses(t *testing.T) {
	exe := buildWinbash(t)
	endpoint := benchEndpoint(t)
	t.Cleanup(func() { runWinbash(t, exe, endpoint, "--stop") })

	// 第一次调用会把 daemon 拉起来。
	if out, code := runWinbash(t, exe, endpoint, "-c", "echo ok"); code != 0 {
		t.Fatalf("第一次调用失败 exit=%d out=%q", code, out)
	}
	first := pidFromStatus(t, mustStatus(t, exe, endpoint))

	for i := 2; i <= 4; i++ {
		if out, code := runWinbash(t, exe, endpoint, "-c", "echo ok"); code != 0 {
			t.Fatalf("第 %d 次调用失败 exit=%d out=%q", i, code, out)
		}
		if pid := pidFromStatus(t, mustStatus(t, exe, endpoint)); pid != first {
			t.Fatalf("第 %d 次调用之后 daemon 换了(pid %d → %d):client 每次都新起了一个",
				i, first, pid)
		}
	}

	// `--stop` 之后必须真的退掉,而且不留端口文件。
	if out, code := runWinbash(t, exe, endpoint, "--stop"); code != 0 {
		t.Fatalf("--stop 失败 exit=%d out=%q", code, out)
	}
	if _, code := runWinbash(t, exe, endpoint, "--status"); code == 0 {
		t.Fatal("stop 之后 status 仍然成功:daemon 没有真的退出")
	}
}

// 计时报告:spec §9 的判据是「端到端 ~17 ms,相对 pwsh 的 351 ms 约 20×」
// 与「一次任务上百条命令的总耗时从几十秒降到几秒」。
//
// 默认跳过:数字取决于机器负载,把它做成常规门槛只会得到假失败。
// 开启方式:WINBASH_BENCH=1 go test ./cmd/winbash/ -run Latency -v
//
// 即使开了,断言也只有**量级**级别的一条 —— 用来抓"每条命令都新起一个 daemon"
// 这种退化,而不是卡毫秒。
func TestEndToEndLatencyReport(t *testing.T) {
	if os.Getenv("WINBASH_BENCH") == "" {
		t.Skip("计时报告需要 WINBASH_BENCH=1(数字依赖机器负载)")
	}

	exe := buildWinbash(t)
	endpoint := benchEndpoint(t)
	t.Cleanup(func() { runWinbash(t, exe, endpoint, "--stop") })

	scripts := []string{
		`echo ok`, `echo ok`, `echo ok`, `echo ok`, `echo ok`,
		`echo ok`, `echo ok`, `echo ok`, `echo ok`, `echo ok`,
		`ls`, `ls`,
		`cat go.mod | grep module`,
		`git --version`,
		`svc status W32Time`,
	}

	// ---- 冷启:daemon 不在时的第一次调用(含 daemon 与 pwsh 引擎的启动)----
	var cold []time.Duration
	for i := 0; i < 3; i++ {
		runWinbash(t, exe, endpoint, "--stop")
		time.Sleep(300 * time.Millisecond)
		start := time.Now()
		runWinbash(t, exe, endpoint, "-c", "echo ok")
		cold = append(cold, time.Since(start))
	}
	t.Logf("冷启(daemon 不在)          n=%d  p50=%s", len(cold), p50(cold))

	// ---- 稳态:daemon 已常驻 ----
	runWinbash(t, exe, endpoint, "-c", "echo ok") // 预热

	base := measure(t, exe, endpoint, 60, func() []string { return []string{"-c", "echo ok"} })
	t.Logf("① 层 echo ok(稳态)         n=%d  p50=%s  min=%s", len(base), p50(base), min(base))

	layer2 := measure(t, exe, endpoint, 30, func() []string { return []string{"-c", "svc status W32Time"} })
	t.Logf("② 层 svc status            n=%d  p50=%s", len(layer2), p50(layer2))

	layer3 := measure(t, exe, endpoint, 30, func() []string { return []string{"-c", "git --version"} })
	t.Logf("③ 层 git --version         n=%d  p50=%s", len(layer3), p50(layer3))

	// ---- pwsh 的对照:AI 现在的做法就是逐次调用它 ----
	pwsh := findPwsh(t)
	if pwsh == "" {
		t.Log("找不到 pwsh,跳过对照")
	} else {
		pwshOne := measurePwsh(t, pwsh, 20)
		t.Logf("pwsh -NoProfile 单条       n=%d  p50=%s", len(pwshOne), p50(pwshOne))
	}

	// ---- 批次:200 条混合命令 ----
	const n = 200
	start := time.Now()
	for i := 0; i < n; i++ {
		runWinbash(t, exe, endpoint, "-c", scripts[i%len(scripts)])
	}
	winTotal := time.Since(start)
	t.Logf("批次 %d 条混合(winbash)   total=%s  平均=%s/条", n, winTotal.Round(time.Millisecond), (winTotal / n).Round(time.Millisecond))

	if pwsh != "" {
		const m = 20
		start := time.Now()
		for i := 0; i < m; i++ {
			c := exec.Command(pwsh, "-NoProfile", "-Command", "echo ok")
			c.Stdout, c.Stderr = io.Discard, io.Discard
			c.Run()
		}
		per := time.Since(start) / m
		t.Logf("pwsh 逐次调用              %d 条外推 %s(单条 %s)", n,
			time.Duration(int64(per)*n).Round(time.Second), per.Round(time.Millisecond))
		t.Logf("比值(批次总耗时)           约 %.1f×", float64(per*n)/float64(winTotal))
	}

	// ---- 量级健全性(宽松到不会因负载假失败)----
	if p50(base) >= p50(cold) {
		t.Errorf("稳态 p50(%s)没有比冷启 p50(%s)快 —— daemon 可能没有被复用", p50(base), p50(cold))
	}
	if winTotal > 60*time.Second {
		t.Errorf("%d 条混合命令花了 %s,远超预期量级", n, winTotal.Round(time.Second))
	}
}

// measure 逐次跑同一个命令,返回每次的耗时。
func measure(t *testing.T, exe, endpoint string, n int, args func() []string) []time.Duration {
	t.Helper()
	ds := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		start := time.Now()
		runWinbash(t, exe, endpoint, args()...)
		ds = append(ds, time.Since(start))
	}
	return ds
}

func measurePwsh(t *testing.T, pwsh string, n int) []time.Duration {
	t.Helper()
	ds := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		start := time.Now()
		c := exec.Command(pwsh, "-NoProfile", "-Command", "echo ok")
		c.Stdout, c.Stderr = io.Discard, io.Discard
		c.Run()
		ds = append(ds, time.Since(start))
	}
	return ds
}

// findPwsh 找 pwsh(本机实测在 %USERPROFILE%\Applications\PowerShell-7\pwsh.exe)。
func findPwsh(t *testing.T) string {
	t.Helper()
	if p, err := exec.LookPath("pwsh"); err == nil {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, "Applications", "PowerShell-7", "pwsh.exe")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// TestFreshSessionIsIsolated 钉住 --fresh / --no-state 的语义:**双向隔离**。
//
// 这条测试的形态是刻意的 —— 同一个 daemon 上「先 A 后 B」的序列。单元测试只能证明
// 「一次 Run 的行为对」,证不到「两次调用之间会话是干净的」;而这里的 bug 恰恰长在
// 「两次调用之间」,所以必须走真进程、真 daemon。
func TestFreshSessionIsIsolated(t *testing.T) {
	exe := buildWinbash(t)
	endpoint := benchEndpoint(t)
	t.Cleanup(func() { runWinbash(t, exe, endpoint, "--stop") })

	runOrFail := func(args ...string) string {
		t.Helper()
		out, code := runWinbash(t, exe, endpoint, args...)
		if code != 0 {
			t.Fatalf("%v 失败 exit=%d out=%q", args, code, out)
		}
		return strings.TrimRight(out, "\r\n")
	}

	// 0. 先确认默认行为没变:会话**持久**(这是 winbash 相对 bash -c 的特性)。
	runOrFail("-c", "PERSIST=kept")
	if got := runOrFail("-c", "echo $PERSIST"); got != "kept" {
		t.Fatalf("默认会话应当持久,got %q", got)
	}

	// 1. 方向一:之前的命令影响不到 fresh 请求。
	if got := runOrFail("--fresh", "-c", "echo ${PERSIST:-<empty>}"); got != "<empty>" {
		t.Errorf("--fresh 应当看不到之前设的变量,got %q", got)
	}

	// 2. 方向二:fresh 请求设的东西也不留给后面的命令。
	runOrFail("--fresh", "-c", "LEAKED=yes")
	if got := runOrFail("-c", "echo ${LEAKED:-<empty>}"); got != "<empty>" {
		t.Errorf("--fresh 设的变量泄漏到了下一个命令,got %q", got)
	}

	// 3. set -e 也隔离:它在 fresh 里该生效(后面的 echo 不执行),但影响不到下一个命令。
	out, code := runWinbash(t, exe, endpoint, "--fresh", "-c", "set -e; false; echo unreachable")
	if code == 0 {
		t.Errorf("fresh 里的 set -e 没生效:exit=0 out=%q", out)
	}
	if got := runOrFail("-c", "false; echo still-running"); got != "still-running" {
		t.Errorf("set -e 泄漏到了下一个命令,got %q(它应当照常往下跑)", got)
	}

	// 4. --no-state 是同一个开关的别名。
	if got := runOrFail("--no-state", "-c", "echo ${PERSIST:-<empty>}"); got != "<empty>" {
		t.Errorf("--no-state 应当与 --fresh 同效,got %q", got)
	}

	// 5. fresh 请求之后长驻会话仍然可用 —— 别把 daemon 弄成一次性。
	if got := runOrFail("-c", "echo alive"); got != "alive" {
		t.Errorf("fresh 之后长驻会话不可用了,got %q", got)
	}
	if got := runOrFail("-c", "echo $PERSIST"); got != "kept" {
		t.Errorf("fresh 之后长驻会话的状态丢了,got %q", got)
	}
}

func p50(ds []time.Duration) time.Duration {
	s := append([]time.Duration{}, ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[len(s)/2].Round(time.Millisecond)
}

func min(ds []time.Duration) time.Duration {
	s := append([]time.Duration{}, ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[0].Round(time.Millisecond)
}
