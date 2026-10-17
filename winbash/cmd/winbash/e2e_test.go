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

// TestManageCommandsGiveUpWhileDaemonBusy 钉住 --status / --stop 的 5s 上限。
//
// 为什么必须是端到端:daemon 是**一条连接串行处理**的(Serve 里 Accept → handle 同步),
// 所以一条长命令跑着时,管理命令连上去只能排队。上限若被改回"无限等",使用者看到的是
// 挂死 —— 上个版本就是这么来的,而 README 当时把 5s 写成「有代码、未实测」。
// 单元测试证不到这件事:它需要「同一个 daemon 上先长命令、后管理命令」这个序列。
//
// 口径:上限本身是 manageTimeout(5s,client 侧),断言取 4s–20s ——
// 下限防"根本没排队就失败了"(那会让这条测试变成空转),上限防"上限被改掉了还在等"。
func TestManageCommandsGiveUpWhileDaemonBusy(t *testing.T) {
	const manageCap = 5 * time.Second
	exe := buildWinbash(t)
	endpoint := benchEndpoint(t)
	t.Cleanup(func() { runWinbash(t, exe, endpoint, "--stop") })

	// 先让 daemon 起来 —— 后面要占住的是**这个** daemon。
	if out, code := runWinbash(t, exe, endpoint, "-c", "echo warm"); code != 0 {
		t.Fatalf("预热失败 exit=%d out=%q", code, out)
	}

	// 占住它:命令先落一个标记(证明它真的开始执行了),再 sleep 到 12s ——
	// 长过下面两次测量(各 5s),又不至于把测试拖太久。
	//
	// 注意这里让 sleep **自然结束**,不靠杀掉 busy 的 client:daemon 跑命令时并不监听
	// 连接断开(实测杀 client 之后它仍占着,一直到 sleep 自己结束),所以"断开即恢复"
	// 是个不存在的假设。
	// 标记路径必须带引号 —— t.TempDir() 落在 C:\Users\<用户名>\ 下,本机用户名含空格。
	marker := filepath.ToSlash(filepath.Join(t.TempDir(), "busy.marker"))
	busy := exec.Command(exe, "--endpoint", endpoint, "--timeout", "60s", "-c", "touch '"+marker+"'; sleep 12")
	busy.Stdout, busy.Stderr = io.Discard, io.Discard
	if err := busy.Start(); err != nil {
		t.Fatalf("启动长命令失败: %v", err)
	}
	t.Cleanup(func() { _ = busy.Process.Kill() })
	waitForFile(t, marker, 10*time.Second)

	for _, op := range []string{"--status", "--stop"} {
		start := time.Now()
		out, code := runWinbash(t, exe, endpoint, op)
		elapsed := time.Since(start)

		if code != 1 {
			t.Fatalf("%s 在 daemon 忙时应当以 1 退出,got %d out=%q", op, code, out)
		}
		if elapsed < manageCap-time.Second || elapsed > 20*time.Second {
			t.Fatalf("%s 花了 %s,应在上限 %s 附近(4s–20s):上限没生效,或者根本没排队",
				op, elapsed.Round(time.Millisecond), manageCap)
		}
		if !strings.Contains(out, "没有响应") {
			t.Errorf("%s 的错误没点明「没有响应」: %q", op, out)
		}
	}

	// 上限只该让调用方"早点放弃",不该把 daemon 弄坏 —— 尤其 --stop 那次超时
	// 不该顺手把它停掉(请求还排在队列里,daemon 根本没读到)。
	// sleep 结束后 daemon 应当自己回到 Accept:这里等它,而不是假设立即恢复。
	deadline := time.Now().Add(20 * time.Second)
	for {
		out, code := runWinbash(t, exe, endpoint, "--timeout", "5s", "-c", "echo alive")
		if code == 0 && strings.TrimSpace(out) == "alive" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("长命令结束后 daemon 没恢复可用:exit=%d out=%q", code, out)
		}
		time.Sleep(200 * time.Millisecond)
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
