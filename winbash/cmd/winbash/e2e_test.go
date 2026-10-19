package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// 这个文件里测的是**跨进程**的事实:它真的构建并运行 winbash.exe。
//
// 为什么必须这样:项目的一条主判据是"不等待"(spec §9),而它取决于
// 「client 每次都新建、daemon 常驻」这件事 —— 包内调用看不见它。
//
// 注:本包的 TestMain 在 main_test.go(摘 WINBASH_SESSION 等),不要在这里再写一个。

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
	// 端口文件必须真的被删掉,而不只是"连不上"。
	// 这两件事不一样:文件还在但进程没了,--status 一样失败(所以上面那条抓不到),
	// 而残留的文件会让后来的 client 去连一个已经不存在的地址。
	// 复审指出这里原先只有注释、没有断言。
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(endpoint); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("--stop 之后端口文件 %s 仍然存在", endpoint)
		}
		time.Sleep(50 * time.Millisecond)
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

// TestManageCommandsDoNotQueueBehindLongCommand 钉住「daemon 忙时管理命令仍然立刻返回」。
//
// 这是并发化带来的**新**保证。改写自 TestManageCommandsGiveUpWhileDaemonBusy ——
// 那条测的是老行为:daemon 一条连接一条连接地串行处理,所以 --status/--stop 只能排在
// 长命令后面,最后靠 client 侧 manageTimeout(5s)放弃。现在 op 分派搬到会话队列之前
// (见 daemon.handleConn),两者都不再排队。
//
// 为什么这比"5s 后放弃"强:放弃只是不让调用方挂死,它仍然拿不到任何信息;现在是直接
// 拿到结果。client 侧的 5s 上限**仍然保留**,作为最后保险(daemon 卡在 Close 上这类
// 极端情况),那份覆盖在 internal/client 的单测里。
//
// 为什么必须是端到端:它需要「同一个 daemon 上先长命令、后管理命令」这个序列 ——
// 单元测试证不到"两条连接之间 daemon 是什么样的"。
func TestManageCommandsDoNotQueueBehindLongCommand(t *testing.T) {
	exe := buildWinbash(t)
	endpoint := benchEndpoint(t)
	// 这里必须用 stopAndWait 而不是裸 --stop:长命令还在跑,daemon 会走
	// stopGrace(5s)宽限 + 关会话,这期间 exe 仍被占用,而 t.TempDir() 要删它。
	t.Cleanup(func() { stopAndWait(t, exe, endpoint) })

	if out, code := runWinbash(t, exe, endpoint, "-c", "echo warm"); code != 0 {
		t.Fatalf("预热失败 exit=%d out=%q", code, out)
	}

	// 占住默认会话:先落标记(证明它真的开始执行了),再 sleep。
	// 标记路径必须带引号 —— t.TempDir() 落在 C:\Users\<用户名>\ 下,本机用户名含空格。
	marker := filepath.ToSlash(filepath.Join(t.TempDir(), "busy.marker"))
	busy := exec.Command(exe, "--endpoint", endpoint, "--timeout", "60s", "-c", "touch '"+marker+"'; sleep 30")
	busy.Stdout, busy.Stderr = io.Discard, io.Discard
	if err := busy.Start(); err != nil {
		t.Fatalf("启动长命令失败: %v", err)
	}
	t.Cleanup(func() { _ = busy.Process.Kill() })
	waitForFile(t, marker, 10*time.Second)

	// --status:必须立刻返回,而且拿到的是真状态(不是"可能正忙"那种放弃)。
	start := time.Now()
	out, code := runWinbash(t, exe, endpoint, "--status")
	elapsed := time.Since(start)
	if code != 0 {
		t.Fatalf("--status 在 daemon 忙时应当立刻成功,got exit=%d out=%q", code, out)
	}
	// 2s 是宽松上限:正常几毫秒。真正要抓的是"又排队了"(那会是 5s 或更久)。
	if elapsed > 2*time.Second {
		t.Fatalf("--status 花了 %s,像是又排队到长命令后面了", elapsed.Round(time.Millisecond))
	}
	if !strings.Contains(out, "pid=") {
		t.Errorf("--status 没给出真状态: %q", out)
	}

	// --stop:同样立刻返回。它会真的把 daemon 停掉,所以放在最后测。
	start = time.Now()
	out, code = runWinbash(t, exe, endpoint, "--stop")
	elapsed = time.Since(start)
	if code != 0 {
		t.Fatalf("--stop 在 daemon 忙时应当立刻成功,got exit=%d out=%q", code, out)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("--stop 花了 %s,像是又排队到长命令后面了", elapsed.Round(time.Millisecond))
	}

	// 停掉之后必须真的"看不见"了 —— 否则使用者会以为没停成功。
	// (发一条新命令会拉起新 daemon,所以这里用 --status:它从不拉起 daemon。)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, c := runWinbash(t, exe, endpoint, "--status"); c != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("--stop 之后 --status 仍然成功:daemon 没有真的退出")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// stopAndWait 停掉 daemon,并等它的进程**真的消失**。
//
// 为什么不能只发 --stop:--stop 只是「让 daemon 开始关停」—— 它在后台给正在跑的
// 命令 stopGrace(5s)宽限、再关会话(最多 1s)。这期间那个进程仍攥着 winbash.exe,
// 而 buildWinbash 把 exe 建在 t.TempDir() 里,清理要删掉它 —— Windows 不允许删
// 正在运行的 exe,于是测试以「Access is denied」的形式失败,而代码其实全对。
func stopAndWait(t *testing.T, exe, endpoint string) {
	t.Helper()
	runWinbash(t, exe, endpoint, "--stop")
	deadline := time.Now().Add(20 * time.Second)
	for countServeDaemons(t, endpoint) != 0 {
		if time.Now().After(deadline) {
			t.Errorf("--stop 之后 20s 仍有该端点的 daemon 进程未退出")
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestSessionsRunInParallel 钉住这条分支的存在理由:**不同会话名之间真的并行**。
//
// 并发化之前,daemon 全生命周期只有一个 Session,请求严格串行 —— 两条 sleep 1
// 要跑 2s(实测 2.074s)。分会话之后两条不同名的 sleep 1 应当接近 1s。
//
// 判据取「并行墙钟 < 1.8s」:串行是 ~2s,并行是 ~1.05s(含 client 15ms 启动开销)。
// 不从 1.0s 卡起,因为进程启动 + IPC 往返本来就有开销,卡太紧会假红。
func TestSessionsRunInParallel(t *testing.T) {
	exe := buildWinbash(t)
	endpoint := benchEndpoint(t)
	t.Cleanup(func() { runWinbash(t, exe, endpoint, "--stop") })

	if out, code := runWinbash(t, exe, endpoint, "-c", "echo warm"); code != 0 {
		t.Fatalf("预热失败 exit=%d out=%q", code, out)
	}

	const n = 3
	var wg sync.WaitGroup
	outs := make([]string, n)
	codes := make([]int, n)
	wg.Add(n)
	start := time.Now()
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			// 每条用**不同**的会话名 —— 这是并行的前提。
			sess := fmt.Sprintf("par-%d", i)
			outs[i], codes[i] = runWinbashCode(exe, endpoint, "--session", sess, "-c", "sleep 1")
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)

	for i, c := range codes {
		if c != 0 {
			t.Errorf("会话 par-%d 失败 exit=%d out=%q", i, c, outs[i])
		}
	}
	// 串行会是 ~3s,并行 ~1.1s。
	if elapsed > 1800*time.Millisecond {
		t.Fatalf("%d 条不同会话的 sleep 1 一共花了 %s —— 没有并行(串行约 3s)",
			n, elapsed.Round(time.Millisecond))
	}
}

// TestSameSessionKeepsStateAndStaysSerial 钉住「同一会话内仍然串行且状态持久」。
//
// 这是并行的**代价边界**:状态只在会话内连续。两条一起断言,因为它们是同一件事的
// 两面 —— 若同会话不串行,X=1 与 echo $X 就可能乱序,状态也就保不住。
func TestSameSessionKeepsStateAndStaysSerial(t *testing.T) {
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

	// 1. 同一会话内状态持久。
	runOrFail("--session", "s1", "-c", "KEPT=one")
	if got := runOrFail("--session", "s1", "-c", "echo ${KEPT:-<empty>}"); got != "one" {
		t.Errorf("同一会话内状态没保住,got %q", got)
	}

	// 2. 跨会话隔离 —— 这是并行的代价,必须是显式的、可预期的。
	if got := runOrFail("--session", "s2", "-c", "echo ${KEPT:-<empty>}"); got != "<empty>" {
		t.Errorf("会话 s1 的变量泄漏到了 s2,got %q", got)
	}

	// 3. 默认会话与具名会话之间也隔离(不写 --session 时与引入该特性前等价)。
	if got := runOrFail("-c", "echo ${KEPT:-<empty>}"); got != "<empty>" {
		t.Errorf("会话 s1 的变量泄漏到了默认会话,got %q", got)
	}

	// 4. 同一会话内**并发**投递也必须串行、且 FIFO。
	//
	//    关键在赋值的位置:必须放在 sleep **之后**。早先写 `ORDER=first; sleep 0.3`,
	//    变量在 t≈0 就落进会话,而读命令 t≈100ms 才发 —— 即使实现完全不串行,读命令
	//    也照样能看到 first,这条断言就成了空转(实测复审抓到了这一点)。
	//    改成先睡再赋值:只有"读命令真的等了写命令"才可能读到 first。
	var wg sync.WaitGroup
	wg.Add(2)
	var writeOut, readOut string
	var writeCode, readCode int
	go func() {
		defer wg.Done()
		writeOut, writeCode = runWinbashCode(exe, endpoint,
			"--session", "s3", "-c", "sleep 0.5; ORDER=first")
	}()
	// 等写命令真的开始跑(它已经进了会话队列),再发读命令 —— 于是读命令排在它后面。
	time.Sleep(150 * time.Millisecond)
	readStart := time.Now()
	go func() {
		defer wg.Done()
		readOut, readCode = runWinbashCode(exe, endpoint,
			"--session", "s3", "-c", "echo ${ORDER:-<unset>}")
	}()
	wg.Wait()
	readElapsed := time.Since(readStart)

	if writeCode != 0 || readCode != 0 {
		t.Fatalf("同会话并发投递失败: write exit=%d out=%q / read exit=%d out=%q",
			writeCode, writeOut, readCode, readOut)
	}
	if got := strings.TrimRight(readOut, "\r\n"); got != "first" {
		t.Errorf("同会话内的两条请求乱序了(后一条读到 %q,期望 first):"+
			"队列没有保证 FIFO,状态会静默损坏", got)
	}
	// 再加一条时间判据:读命令若真的排在写命令(0.5s)后面,它自己的耗时必然
	// 覆盖剩下的等待。只靠"读到 first"仍可能被一个恰好慢的实现蒙对,
	// 而"读命令必须等"是这条测试真正要钉的语义。
	if readElapsed < 300*time.Millisecond {
		t.Errorf("读命令只花了 %s 就返回 —— 它没有等前面的写命令(sleep 0.5s),同会话没串行",
			readElapsed.Round(time.Millisecond))
	}
}

// TestSessionLimitIsRejected 钉住会话池上限:**拒绝**,不回收最久未用的。
//
// 回收等于静默销毁状态,而状态正是这条会话存在的理由 —— 宁可让使用者看到明确的
// 拒绝,也不要让他在另一条命令里发现变量没了。
//
// 上限是 16(daemon.maxSessions)。这条测试建 17 个不同名的会话,最后一个必须失败。
func TestSessionLimitIsRejected(t *testing.T) {
	if testing.Short() {
		t.Skip("要建 16 个会话,short 模式跳过")
	}
	exe := buildWinbash(t)
	endpoint := benchEndpoint(t)
	t.Cleanup(func() { runWinbash(t, exe, endpoint, "--stop") })

	if out, code := runWinbash(t, exe, endpoint, "-c", "echo warm"); code != 0 {
		t.Fatalf("预热失败 exit=%d out=%q", code, out)
	}

	// 先在第一个会话里落一个状态。这一条是"不回收 LRU"的关键:
	// 若实现改成回收最久未用的那条,lim-01 会被销毁,而**重建一个同名会话再跑
	// echo alive 一样是 exit 0** —— 只看退出码是抓不到的(复审抓到的假绿)。
	// 所以先写一个变量,拒绝第 17 个之后把它读回来。
	if out, code := runWinbash(t, exe, endpoint, "--session", "lim-01", "-c", "MARK=kept"); code != 0 {
		t.Fatalf("往 lim-01 写状态失败 exit=%d out=%q", code, out)
	}

	// 每个会话跑一条 ① 层命令:不碰 ② 层,所以不会各拉一个 pwsh(引擎是按需的)。
	for i := 2; i <= 16; i++ {
		sess := fmt.Sprintf("lim-%02d", i)
		if out, code := runWinbash(t, exe, endpoint, "--session", sess, "-c", "true"); code != 0 {
			t.Fatalf("第 %d 个会话就失败了(上限应当允许 16 个):exit=%d out=%q", i, code, out)
		}
	}

	// 第 17 个必须被拒绝,而且错误要点明上限与处置。
	// 断言 exit **恰好是 1**(不是"非零"):契约是 1,回归成 2(用法错误)也该被抓到。
	out, code := runWinbash(t, exe, endpoint, "--session", "lim-17", "-c", "true")
	if code != 1 {
		t.Fatalf("第 17 个会话应当以 exit 1 拒绝,got %d out=%q", code, out)
	}
	if !strings.Contains(out, "上限") {
		t.Errorf("错误没点明会话上限: %q", out)
	}

	// 被拒绝之后,已有会话必须**照常可用**,而且它的**状态还在**
	// —— 这一条才真正钉住"不回收最久未用"。
	if out, code := runWinbash(t, exe, endpoint, "--session", "lim-01", "-c", "echo ${MARK:-<lost>}"); code != 0 {
		t.Fatalf("拒绝第 17 个会话之后 lim-01 不可用了:exit=%d out=%q", code, out)
	} else if got := strings.TrimRight(out, "\r\n"); got != "kept" {
		t.Errorf("lim-01 的状态丢了(got %q):说明上限触发时回收了已有会话,"+
			"而那是静默销毁状态", got)
	}
}

// waitForFile 等一个文件出现(最多 timeout)。用它确认"长命令真的开始执行了",
// 而不是靠 sleep 猜 —— 猜早了会让整条测试变成空转。
func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s 在 %s 内没有出现:长命令没跑起来,后面的断言无效", path, timeout)
}

// countServeDaemons 数「命令行里带这个端点、且带 --serve」的 winbash 进程数。
//
// 两个细节都不能省:
//   - 必须按**端点**过滤:别的测试、使用者自己的 daemon 都在跑,不过滤就数不准
//     (与 benchEndpoint 是同一条理由,隔离要在统计这一侧也成立);
//   - 必须用 WMI 而不是 tasklist:要按命令行过滤,而 tasklist 看不到命令行。
func countServeDaemons(t *testing.T, endpoint string) int {
	t.Helper()
	ps := fmt.Sprintf(
		`@(Get-CimInstance Win32_Process -Filter "Name='winbash.exe'" | `+
			`Where-Object { $_.CommandLine -and $_.CommandLine -like '*--serve*' -and $_.CommandLine -like '*%s*' }).Count`,
		endpoint)
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", ps)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("枚举 --serve 进程失败: %v\n%s", err, out.String())
	}
	n, err := strconv.Atoi(strings.TrimSpace(out.String()))
	if err != nil {
		t.Fatalf("进程计数解析不了: %q", out.String())
	}
	return n
}

// runWinbashCode 与 runWinbash 同效,但不用 t.Fatalf ——
// 并发测试要在 goroutine 里调用,而 t.Fatalf 只能从测试自己的 goroutine 调。
func runWinbashCode(exe, endpoint string, args ...string) (string, int) {
	full := append([]string{"--endpoint", endpoint}, args...)
	cmd := exec.Command(exe, full...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		code = -1
	}
	return out.String(), code
}

// TestConcurrentFirstStartSpawnsOneDaemon 钉住「并发首次启动只起一个 daemon」。
//
// 修之前(实测 8 个 client 同时首次调用):8 个 --serve 进程同时活着,端口文件只指向
// 最后一个写入的,其余 7 个是孤儿 —— --status 看不到、--stop 停不掉,各自还抱着一个
// Session,要等 30 分钟空闲才自己退。根因不是"后来者绑定失败退出":daemon 用
// 127.0.0.1:0 取随机端口,并发起的每一个都能绑定成功,必须靠跨进程锁去重。
func TestConcurrentFirstStartSpawnsOneDaemon(t *testing.T) {
	exe := buildWinbash(t)
	endpoint := benchEndpoint(t)
	t.Cleanup(func() { runWinbash(t, exe, endpoint, "--stop") })

	const n = 8
	var wg sync.WaitGroup
	outs := make([]string, n)
	codes := make([]int, n)
	wg.Add(n)
	start := time.Now()
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			outs[i], codes[i] = runWinbashCode(exe, endpoint, "-c", "echo ok")
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)

	// 1. 每一条都必须成功。注意这一条**单独不足以**说明问题:修之前它们也都成功,
	//    只是各连各的 daemon。它守的是"锁别把人挡在门外"。
	for i, c := range codes {
		if c != 0 {
			t.Errorf("第 %d 个并发 client 失败 exit=%d out=%q", i, c, outs[i])
		}
	}

	// 2. 只准留一个 daemon —— 这就是本测试的存在理由。修之前这里是 8。
	if got := countServeDaemons(t, endpoint); got != 1 {
		t.Fatalf("并发 %d 个首次启动后存活 daemon = %d,应当只有 1 个"+
			"(多出来的就是孤儿:--status 看不到、--stop 停不掉)", n, got)
	}

	// 3. 持锁只该覆盖「拉起 daemon」,不该把并发请求串成一条长队。
	//    8 个 client 每个约 15ms 启动开销,加上一次 daemon 冷启,给足余量取 10s ——
	//    这里要抓的是"锁一直没放"这种退化,不是卡毫秒。
	if elapsed > 10*time.Second {
		t.Errorf("并发 %d 个首次调用花了 %v,像是启动锁没有及时释放", n, elapsed.Round(time.Millisecond))
	}

	// 4. 停掉之后不该有残留。
	if out, code := runWinbash(t, exe, endpoint, "--stop"); code != 0 {
		t.Fatalf("--stop 失败 exit=%d out=%q", code, out)
	}
	deadline := time.Now().Add(5 * time.Second)
	for countServeDaemons(t, endpoint) != 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if got := countServeDaemons(t, endpoint); got != 0 {
		t.Fatalf("--stop 之后仍有 %d 个 daemon 存活", got)
	}
}

// TestStaleStartLockIsTakenOver 钉住陈旧锁的接管。
//
// 持锁者可能被强杀(taskkill、宿主回收、用户 Ctrl+C 掉了宿主),锁文件会留下。
// 若不做接管,这个端点就**永久**起不来 daemon —— 一次强杀 = 该端点报废,
// 而这种失败看起来像"daemon 在 20s 内没有起来",极难归因。
func TestStaleStartLockIsTakenOver(t *testing.T) {
	exe := buildWinbash(t)
	endpoint := benchEndpoint(t)
	t.Cleanup(func() { runWinbash(t, exe, endpoint, "--stop") })

	// 造一把"持锁者已经死了"的残留锁:文件在,但 mtime 老到远超 staleLockBudget。
	lock := endpoint + ".lock"
	if err := os.WriteFile(lock, []byte("999999\n0\n"), 0o600); err != nil {
		t.Fatalf("造陈旧锁失败: %v", err)
	}
	old := time.Now().Add(-5 * time.Minute)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatalf("改锁文件 mtime 失败: %v", err)
	}

	if out, code := runWinbash(t, exe, endpoint, "-c", "echo ok"); code != 0 {
		t.Fatalf("陈旧锁把启动挡住了: exit=%d out=%q", code, out)
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Errorf("正常放锁之后锁文件仍然存在(stat err=%v)", err)
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

// sameDirLine 判断输出里的某一行是否指向 dir 这个目录。
//
// 只比文件身份、不比字符串:t.TempDir() 可能给 8.3 短名,而 PowerShell 经
// Get-Item 解析后给长名 —— 同一个目录、两个字符串,EqualFold 会假红(这个坑上一轮
// 踩过)。stat 失败一律当作"不是这一行":同一个 stdout 里还混着被测脚本自己的输出。
func sameDirLine(t *testing.T, dir, line string) bool {
	t.Helper()
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat %q: %v", dir, err)
	}
	fi, err := os.Stat(line)
	if err != nil {
		return false
	}
	return os.SameFile(fi, di)
}

// TestPwshFollowsSessionCwdAcrossProcesses 钉住「② 层的 pwsh 跟随**会话**的 cwd」。
//
// 这条事实在 internal/winmgmt/pwshcmd_test.go(TestPwshFollowsSessionDir)已经钉过,
// 但那个坑长在 client / daemon / 常驻引擎的边界上:会话的 cwd 是 client 发过来的,
// 引擎是 daemon 里的一个常驻进程 —— 包内测试证不到这一路上它有没有被带上。
// 所以这里走真进程,并把两件事放进**同一条** -c 脚本:
//
//	cd <dir>; pwsh -c '(Get-Location).Path'; pwsh marker.ps1
//
// 必须同一条脚本才构成「先 cd、再 pwsh」这个序列;拆成两次调用的话 cd 与 pwsh 之间
// 还要跨一次进程,测的就不是同一个东西了。
//
// 修前会红,而且两处一起红:
//   - 目录那行会是 **daemon 启动时的 cwd**(引擎继承了它,会话的 cd 没同步过去);
//   - `pwsh marker.ps1` 会报「读不到脚本文件」—— 相对路径按 daemon 的 cwd 解析了,
//     而文件明明就在 <dir> 里。
func TestPwshFollowsSessionCwdAcrossProcesses(t *testing.T) {
	if findPwsh(t) == "" {
		t.Skip("本机没有 pwsh,② 层跑不了")
	}
	exe := buildWinbash(t)
	endpoint := benchEndpoint(t)
	// 这里用 stopAndWait 而不是裸 --stop:daemon 关停时要杀掉常驻的 pwsh,这期间它
	// 仍攥着 winbash.exe,而 buildWinbash 把 exe 建在 t.TempDir() 里 —— Windows
	// 不允许删正在运行的 exe,清理会以「Access is denied」失败(代码其实全对)。
	t.Cleanup(func() { stopAndWait(t, exe, endpoint) })

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "marker.ps1"),
		[]byte("Write-Output \"from-session-cwd\"\r\n"), 0o644); err != nil {
		t.Fatalf("写 marker.ps1: %v", err)
	}

	out, code := runWinbash(t, exe, endpoint, "-c",
		"cd "+shellPathArg(dir)+"; pwsh -c '(Get-Location).Path'; pwsh marker.ps1")
	if code != 0 {
		t.Fatalf("rc=%d out=%q(cd %s 之后 pwsh 就失败了)", code, out, dir)
	}

	var sawDir, sawMarker bool
	for _, ln := range strings.Split(strings.TrimRight(out, "\r\n"), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		if ln == "from-session-cwd" {
			sawMarker = true
			continue
		}
		if sameDirLine(t, dir, ln) {
			sawDir = true
		}
	}
	if !sawDir {
		t.Errorf("没有一行指向会话的 cwd %q:引擎的 cwd 是 daemon 启动时继承的那个,"+
			"会话的 cd 没同步过去(修前的症状)。out=%q", dir, out)
	}
	if !sawMarker {
		t.Errorf("输出里没有 from-session-cwd:相对路径的脚本没按会话的 cwd 解析。"+
			"out=%q", out)
	}
}

// TestEndpointSurvivesStopAndRestart 钉住「--stop 之后这个 endpoint 还能用」。
//
// 修之前的现场:自定义 --endpoint 的端口文件被收紧了 ACL,daemon --stop 时删不掉
// 自己建的那个文件(ACL 里没有 DELETE)。--stop 仍报 rc=0,文件却留了下来、指向一个
// 已经不存在的地址 —— 此后每次调用都是「连接被拒」,重试也不会好:这个 endpoint
// 就此**永久不可用**。
//
// 所以判据必须落在「文件真的不在了」上,不能只看 --stop 的退出码:把删除错误吞掉的
// 实现同样报 rc=0,而修前那个 bug 正是这样被漏掉的。紧跟着的第二次调用钉的是另一半:
// 即使文件因故残留,下一个调用也要能自愈(重建 daemon)。
//
// 注意:这里**故意不复现**「ACL 让人删不掉文件」。端口文件建在 t.TempDir() 下
// (%TEMP% 里),父目录的 FILE_DELETE_CHILD 会顶掉文件自身 ACL 的缺失 —— 构造不出来。
// ACL 与「残留文件自愈」两块已由 internal/daemon/state_test.go 与
// internal/client/client_test.go 的单元测试钉住,这条只负责跨进程能看见的那一半。
func TestEndpointSurvivesStopAndRestart(t *testing.T) {
	if findPwsh(t) == "" {
		t.Skip("本机没有 pwsh,② 层跑不了")
	}
	exe := buildWinbash(t)
	endpoint := benchEndpoint(t)
	t.Cleanup(func() { runWinbash(t, exe, endpoint, "--stop") })

	if out, code := runWinbash(t, exe, endpoint, "-c", "echo boot"); code != 0 {
		t.Fatalf("首次调用失败 rc=%d out=%q", code, out)
	}
	if out, code := runWinbash(t, exe, endpoint, "--stop"); code != 0 {
		t.Fatalf("--stop 失败 rc=%d out=%q", code, out)
	}

	// --stop 在回 OK **之前**就关监听、删端口文件(见 daemon.stopAccepting),所以
	// 这里不需要轮询:拿到 rc=0 时该做的清理已经做完了。延迟出现才算失败的话,
	// 这条断言反而抓不到"删不掉"那个缺陷。
	if _, err := os.Stat(endpoint); !os.IsNotExist(err) {
		t.Fatalf("--stop 之后端口文件 %s 仍然存在(stat err=%v)。这个文件是 daemon 自己"+
			"建的,它必须能删掉 —— 删不掉就那个 endpoint 从此永久不可用,"+
			"之后每次调用只会得到一句「连接被拒」", endpoint, err)
	}

	out, code := runWinbash(t, exe, endpoint, "-c", "echo recovery")
	if code != 0 {
		t.Fatalf("--stop 之后同一个 endpoint 再也起不来了 rc=%d out=%q", code, out)
	}
	if !strings.Contains(out, "recovery") {
		t.Errorf("恢复后的调用输出里没有 recovery: %q", out)
	}
}

// TestPwshScriptWithParamBlockAcrossProcesses 钉住「以 param(...) 开头的脚本能跑」。
//
// 机理在 internal/winmgmt/pwshcmd_test.go 里写清了:PowerShell 要求 `param(...)` 是
// scriptblock 的**第一条**语句,而老实现把环境同步拼成脚本文本的**前导**,于是报
//
//	The term 'param' is not recognized as a name of a cmdlet...
//
// 它为什么还需要一条跨进程的覆盖:那段前导要真的**非空**才出现,而环境是
// client → daemon → 会话 → 常驻引擎一路传下去的 —— 包内测试得自己造 WithEnv,
// 真实调用里则取决于 ① 层的 export 有没有被同步过去。所以这里先 export 一个探针
// 变量:让"前导非空"这件事由**这条测试自己**保证,而不是碰巧依赖 daemon 继承了什么。
func TestPwshScriptWithParamBlockAcrossProcesses(t *testing.T) {
	if findPwsh(t) == "" {
		t.Skip("本机没有 pwsh,② 层跑不了")
	}
	exe := buildWinbash(t)
	endpoint := benchEndpoint(t)
	t.Cleanup(func() { stopAndWait(t, exe, endpoint) })

	dir := t.TempDir()
	// 行尾用 \r\n:这是 Windows 原生工具写出来的形状,不至于让这条测试因为
	// 「另一个平台的换行」而红。
	body := "param($Name = \"default\")\r\nWrite-Output \"hello-$Name\"\r\n"
	if err := os.WriteFile(filepath.Join(dir, "param.ps1"), []byte(body), 0o644); err != nil {
		t.Fatalf("写 param.ps1: %v", err)
	}

	// 相对文件名是刻意的:顺带复用「按会话 cwd 解析脚本路径」,让两个缺陷在同一处汇合。
	out, code := runWinbash(t, exe, endpoint, "-c",
		"export WINBASH_PARAM_PROBE=1; cd "+shellPathArg(dir)+"; pwsh param.ps1")
	if code != 0 {
		t.Fatalf("rc=%d out=%q(带 param 块的脚本跑不了:环境又回到脚本前导了?)", code, out)
	}
	for _, ln := range strings.Split(strings.TrimRight(out, "\r\n"), "\n") {
		if strings.TrimSpace(ln) == "hello-default" {
			return
		}
	}
	t.Fatalf("输出里没有一行恰为 hello-default: %q(param 的默认值没生效)", out)
}

// shellPathArg 把一个 Windows 路径变成可以安全写进 `-c` 脚本的实参。
//
// 两个选择都不是随手写的:
//   - **正斜杠**:t.TempDir() 在 Windows 上给反斜杠形式,而这段脚本由 ① 层按 bash
//     风格解析,`\t`、`\n` 之类会被当成转义序列(本机路径里真的出现过 `\t`)。
//     filepath.ToSlash 之后没有这个歧义,而 POSIX 风格的路径 winbash 与 pwsh 都认。
//   - **单引号**:本机用户名带空格("Test User"),t.TempDir() 落在 C:\Users\Test User\… 下。
//     bash 风格的单引号里不做任何展开,空格、`$`、反引号都原样传进去。
func shellPathArg(path string) string {
	return "'" + filepath.ToSlash(path) + "'"
}

// TestDaemonKilledMidFlightDoesNotReplayTheScript 钉住 2026-10-19 收窄的重试判据。
//
// 请求帧**已经完整送出**之后连接断掉,client 绝不能重放脚本:
//   - 那会让同一条脚本跑两遍(与「超时绝不重放」要消灭的是同一个失败形态);
//   - 而且第二遍打在新拉起的 daemon 上,旧 daemon 那份可能还在跑 —— daemon 没有
//     Job Object,它派生的子进程不会跟着退出,于是两个副本并发执行。
//
// 构造:后台发一条「先写标记、再 sleep」的命令,等标记出现(证明它真的开始执行了),
// 然后强杀 daemon。判据是标记文件的行数:
//   - 修后:1 行 —— client 不重放,直接报「命令可能已经执行」;
//   - 修前:2 行 —— 重拉 daemon 又跑了一遍(这条用例就是为它写的,变异验证见提交信息)。
//
// 为什么必须端到端:重放发生在 client 与 daemon 的**连接生命周期**上,包内测试
// 看不见「杀掉之后谁又拉起了一个 daemon」。
func TestDaemonKilledMidFlightDoesNotReplayTheScript(t *testing.T) {
	exe := buildWinbash(t)
	endpoint := benchEndpoint(t)
	t.Cleanup(func() { stopAndWait(t, exe, endpoint) })

	if out, code := runWinbash(t, exe, endpoint, "-c", "echo warm"); code != 0 {
		t.Fatalf("预热失败 exit=%d out=%q", code, out)
	}
	pid := pidFromStatus(t, mustStatus(t, exe, endpoint))

	marker := filepath.Join(t.TempDir(), "replay.marker")
	// sleep 3 而不是更长:修前那条路会重跑脚本、等它跑完才返回,3s 足够在变异
	// 验证里看出差别,又不至于让正常路径白等。
	script := "echo RUN >> " + shellPathArg(marker) + "; sleep 3"
	cmd := exec.Command(exe, "--endpoint", endpoint, "--timeout", "30s", "-c", script)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动被测命令失败: %v", err)
	}
	// 兜底:万一 client 卡住,别让整个包挂到 go test 的默认超时。
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	waitForFile(t, marker, 15*time.Second)

	// 强杀 daemon —— 模拟被 taskkill / 崩溃 / 机器资源回收。
	taskkillPID(t, pid)

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("杀掉 daemon 之后 client 20s 还没返回")
	}

	// 给「重放」留出落地时间:修前那条路会在新 daemon 上重新执行、立刻写出第二行。
	time.Sleep(1500 * time.Millisecond)

	if n := countNonEmptyLines(marker); n != 1 {
		t.Fatalf("标记文件里有 %d 行(期望 1 行)—— 脚本被重放了;输出: %q", n, out.String())
	}

	msg := out.String()
	// 文案要能让人/AI 做决定:是「放心重跑」还是「先去确认副作用」。
	if !strings.Contains(msg, "可能已经执行") {
		t.Errorf("错误信息应当说明命令可能已经执行,实际: %q", msg)
	}
	if strings.Contains(msg, "未执行") {
		t.Errorf("读响应失败却被说成「命令未执行」—— 那会诱导调用方放心重跑: %q", msg)
	}
	if code := cmd.ProcessState.ExitCode(); code != 1 {
		t.Errorf("exit=%d,期望 1", code)
	}
}

// taskkillPID 强杀一个 pid。用 taskkill 而不是 Process.Kill:后者只能杀我们自己
// 启动的子进程,而 daemon 是 client 拉起的,测试进程与它没有父子关系。
func taskkillPID(t *testing.T, pid int) {
	t.Helper()
	out, err := exec.Command("taskkill", "/F", "/PID", strconv.Itoa(pid)).CombinedOutput()
	if err != nil {
		t.Fatalf("强杀 daemon(pid=%d)失败: %v\n%s", pid, err, out)
	}
}

// countNonEmptyLines 数文件里非空行的数量(文件不存在算 0)。
func countNonEmptyLines(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n := 0
	for _, ln := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(ln) != "" {
			n++
		}
	}
	return n
}

// TestStreamStdinOnCommandThatIgnoresItStaysQuiet 钉住「不读 stdin 的命令不该有噪音」。
//
// 这是一条**修 bug 引入、又当场被复审抓出来**的路径:stdin 转发收尾时
// CloseWrite 会失败,而失败原因是 Windows 的 WSAECONNRESET —— daemon 写完响应
// 就关连接,它的接收缓冲里还有没人读的 stdin,close 于是发 RST。那是一条完全
// 正常的路径(命令根本没打算读 stdin),把它的错误报成「stdin 转发收尾失败」
// 等于在成功命令后面加一行假警告。
//
// 判据是 stderr 必须**完全干净**。灌 64KB 是为了让 daemon 的接收缓冲里确实
// 有没被读走的数据 —— 小输入可能恰好全被内核吞掉,那样这条用例的鉴别力就没了。
//
// **鉴别力的诚实说明**:这条用例对「把 CloseWrite 错误报出来」那个变异**不敏感**
// —— 实测本机这条路径上 CloseWrite 并不失败(2026-10-19,`head -c 65536 | …`
// 配 `echo done`,变异版也没打出警告)。所以复审那条担忧是源码层面的推断,不是
// 可复现的缺陷。保留这条用例的理由是它钉的是**结果**:不读 stdin 的正常命令
// stderr 必须干净 —— 将来谁在这一带加日志/警告,它会立刻红。
func TestStreamStdinOnCommandThatIgnoresItStaysQuiet(t *testing.T) {
	exe := buildWinbash(t)
	endpoint := benchEndpoint(t)
	t.Cleanup(func() { stopAndWait(t, exe, endpoint) })

	cmd := exec.Command(exe, "--endpoint", endpoint, "-c", "echo done")
	cmd.Stdin = strings.NewReader(strings.Repeat("x", 64<<10))
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("命令失败: %v\nstdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "done" {
		t.Fatalf("stdout = %q", got)
	}
	if stderr.Len() != 0 {
		t.Fatalf("不读 stdin 的正常命令不该有 stderr, got %q", stderr.String())
	}
}
