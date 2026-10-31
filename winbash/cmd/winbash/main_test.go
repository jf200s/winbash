package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"winbash/internal/daemon"
	"winbash/internal/shell"
)

// TestMain 先摘掉 WINBASH_SESSION 与 WINBASH_SESSION_NAME。
//
// 本包测的是 **CLI 自己的行为**,而它经常在 winbash 会话里被跑 —— README「开发」节
// 给的命令就是 `go test ./...`,而那时执行它的 shell 正是 winbash。③ 层会把
// EnvNested=1 派给所有子进程,测试进程继承下来后,client 把每次调用都当成"嵌套",
// `--status`/`--stop` 刻意拒绝,于是 3 条 daemon 管理用例必然红 ——
// 而且失败信息看着像产品 bug(用户就这么撞上过)。
//
// 用 TestMain 而不是每个用例 t.Setenv:这个包**每一个**用例都该在非嵌套前提下跑,
// 一处声明比 N 处复制更难漏。测试对自身环境的要求要自己说,不能靠"你恰好在 Git Bash 里跑"。
func TestMain(m *testing.M) {
	_ = os.Unsetenv(shell.EnvNested)
	// WINBASH_SESSION_NAME 是 --session 的默认值来源:不摘掉的话,预热命令会落进一个
	// **具名**会话,白占一个名额,TestSessionLimitIsRejected 就会在第 16 个而不是第 17
	// 个报上限 —— 而那条错误信息会把人引向"上限算错了",不是"环境没清干净"。
	_ = os.Unsetenv("WINBASH_SESSION_NAME")
	os.Exit(m.Run())
}

// run 现在接收 args 与 stdout/stderr,所以能直接在测试里驱动 —— 这填掉了审计报告
// 点出的 cmd/winbash 0% 覆盖。原先它读**全局** flag.CommandLine 且只写 os.Stdout/
// os.Stderr:既不能重复调用(第二次注册同名 flag 会 panic),也没法断言输出。
//
// 不把 main() 本身纳入覆盖:那要构建真 exe 再 os/exec 跑它,而子进程的覆盖率
// 不会回传给 go test(除非 go build -cover + GOCOVERDIR 那一套),收益不抵复杂度。
// main 现在只剩一行 os.Exit(run(...))。
func runCLI(t *testing.T, args ...string) (code int, out, errOut string) {
	t.Helper()
	var o, e bytes.Buffer
	code = run(args, &o, &e)
	return code, o.String(), e.String()
}

func tmpEndpoint(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "test.port")
}

// startDaemon 在测试进程里直接起一个真 daemon。
//
// 为什么必须这样:run 走的 client 在端口文件不存在时会 spawnDaemon(),而后者用
// os.Executable() —— 在测试进程里那是 winbash.test,它不认识 --serve,于是端口文件
// 永远不出现,每条用例都要干等到 startupBudget(20s)才失败(实测就是这么挂的)。
// 自己把 daemon 起起来,run 就走"连上已有 daemon"这条正常路径。
//
// 代价:spawnDaemon 那条"daemon 不在就把它拉起来"的路径仍然没有回归网。要覆盖它
// 得构建真 exe,那是 daemon 包之外的事,已记在 progress 账本里。
func startDaemon(t *testing.T, ep string) {
	t.Helper()
	go func() { _ = daemon.Serve(ep, time.Minute, daemon.DefaultReadTimeout) }()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(ep); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("daemon 没在 10s 内写出端口文件")
}

// sanitize 是纯函数,顺手把边界钉住 —— 本机用户名就带空格("Test User"),
// 端口文件名靠它才合法。
func TestSanitizeKeepsOnlySafeCharacters(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Test User", "HELLO_F"},
		{"a-b_c9", "a-b_c9"},
		{"张三", "__"},    // 非 ASCII 每个 rune 换一个下划线
		{"", "default"}, // 全空时给个兜底,而不是空文件名
		{"  ", "__"},
	}
	for _, c := range cases {
		if got := sanitize(c.in); got != c.want {
			t.Errorf("sanitize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// 端口文件名要能安全落地:不带空格、不带路径分隔符。
func TestDefaultEndpointIsAUsableFileName(t *testing.T) {
	got := defaultEndpoint()
	base := filepath.Base(got)
	if !strings.HasPrefix(base, "winbash-") || !strings.HasSuffix(base, ".port") {
		t.Fatalf("命名不符: %q", base)
	}
	if strings.ContainsAny(base, ` \/:*?"<>|`) {
		t.Fatalf("文件名里有非法字符: %q", base)
	}
}

func TestNoScriptIsAUsageError(t *testing.T) {
	code, out, errOut := runCLI(t)
	if code != 2 {
		t.Fatalf("code=%d want 2", code)
	}
	if out != "" {
		t.Fatalf("不该有 stdout 输出: %q", out)
	}
	if !strings.Contains(errOut, "用法") {
		t.Fatalf("stderr 该给用法提示: %q", errOut)
	}
}

func TestUnknownFlagIsAUsageError(t *testing.T) {
	code, _, errOut := runCLI(t, "--definitely-not-a-flag")
	if code != 2 {
		t.Fatalf("code=%d want 2 (不认识的 flag 必须报错,不能静默忽略)", code)
	}
	if errOut == "" {
		t.Fatal("该有 stderr 输出")
	}
}

// --fresh 只对「执行脚本」这条路有意义。和 --serve/--stop/--status 一起给时
// ★ --serve / --stop / --status 三者互斥,任意两个同用都是用法错误。
//
// 修之前它们按 if 顺序取第一个,而 `if serve` 在最前 —— 于是实测:
//
//	--serve --stop    → **真的去 serve 并常驻**(超时 4s 都没返回)
//	--serve --status  → 同样挂住
//
// 这比"忽略一个"严重得多:使用者想停掉 daemon,结果把自己挂在一个前台 daemon 上。
// 两个开关都在手边、以为可以一起给,是最容易踩的一种。
func TestControlFlagsAreMutuallyExclusive(t *testing.T) {
	// 守卫:本用例曾经**不带 `--endpoint`** 跑过 `--stop`。不带就用 defaultEndpoint(),
	// 也就是调用者正在用的那个 daemon —— 而本包经常在 winbash 会话里被跑(README
	// 「开发」节)。那时这条 --stop 会把承载 `go test` 自己的 daemon 停掉:表现是
	// `go test ./...` 只输出前几个包就断流、会话里的变量 / set -e / trap 一起没,
	// 而本用例照样 PASS。所以这里快照默认端点,结束时要求它**原样不动**。
	//
	// 这条守卫只在「默认端点正被某个 daemon 使用」时才有鉴别力(CI 上通常没有),
	// 但它恰好覆盖出事的那台机器:开发机上跑测试时 daemon 是常驻的。
	defaultEP := defaultEndpoint()
	before, beforeErr := os.ReadFile(defaultEP)
	t.Cleanup(func() {
		after, afterErr := os.ReadFile(defaultEP)
		if (beforeErr == nil) != (afterErr == nil) || !bytes.Equal(before, after) {
			t.Errorf("本用例改动了默认端点 %s:管理开关必须带 --endpoint,否则会停掉调用者自己的 daemon", defaultEP)
		}
	})

	combos := [][]string{
		{"--serve", "--stop"},
		{"--serve", "--status"},
		{"--stop", "--status"},
		{"--serve", "--stop", "--status"},
	}
	for _, c := range combos {
		// 这里不会真的挂住:互斥校验在 `if serve` 之前。若它退化了,这个测试会卡住 ——
		// go test 的默认超时会把整包判红,不会静默通过。
		code, _, errOut := runCLI(t, c...)
		if code != 2 {
			t.Errorf("%v: code=%d want 2(互斥的管理动作不能静默取第一个)", c, code)
		}
		if !strings.Contains(errOut, "互斥") {
			t.Errorf("%v: stderr 应当说明互斥,got %q", c, errOut)
		}
	}
	// 单独给的时候不能被误伤。
	//
	// 每条都显式指向**不存在的临时端点**:--stop / --status 在那种端点上返回 1
	// (不是 2),正好够验证「单独给不是用法错误」这件事,又不会碰到任何真实 daemon。
	for _, ctl := range []string{"--stop", "--status"} {
		if code, _, errOut := runCLI(t, ctl, "--endpoint", tmpEndpoint(t)); code == 2 {
			t.Errorf("单独 %s 不该是用法错误,stderr=%q", ctl, errOut)
		}
	}
}

// 报用法错误,而不是默默忽略 —— 静默忽略会让人以为隔离生效了。
func TestFreshWithControlFlagsIsAUsageError(t *testing.T) {
	for _, ctl := range []string{"--serve", "--stop", "--status"} {
		code, _, errOut := runCLI(t, "--fresh", ctl)
		if code != 2 {
			t.Errorf("--fresh %s: code=%d want 2 (组合不合法不能静默放过)", ctl, code)
		}
		if !strings.Contains(errOut, "--fresh") {
			t.Errorf("--fresh %s: stderr 应当说明冲突,got %q", ctl, errOut)
		}
	}
}

// --session 的默认值来自 WINBASH_SESSION_NAME,而那个变量是给**执行脚本**那条路用的。
// 所以互斥校验的判据必须是「用户显式给了 flag」,不能是「值非空」——
// 按值判会让一个环境变量把 --status / --stop / --serve 全部打成 exit 2。
//
// 实测过的后果(修之前):设了 WINBASH_SESSION_NAME 之后连 daemon 都起不来
// (client 拉起的 `--serve` 子进程继承了同一个环境),而 --stop 正是版本不匹配时
// 唯一的处置 —— 于是使用者会卡在一个既用不了又停不掉的 daemon 上。
// 而 README 恰恰推荐并行子代理去设这个变量,所以这条不是理论风险。
//
// 两个方向都要钉住:环境变量不该触发互斥,显式 flag 仍必须触发。
func TestSessionEnvVarDoesNotTriggerUsageErrors(t *testing.T) {
	t.Setenv("WINBASH_SESSION_NAME", "build")

	// 方向一:环境变量在场时,管理开关与 --fresh 都不能被误判成"与 --session 同用"。
	ep := tmpEndpoint(t)
	startDaemon(t, ep)
	defer func() { _, _, _ = runCLI(t, "--stop", "--endpoint", ep) }()

	if code, _, errOut := runCLI(t, "--status", "--endpoint", ep); code != 0 {
		t.Errorf("环境变量在场时 --status 不该失败: code=%d stderr=%q", code, errOut)
	}
	// --fresh 与环境变量给的默认会话名不冲突:--fresh 已经把"不要持久会话"说清楚了。
	if code, out, errOut := runCLI(t, "--fresh", "-c", "echo fresh-ok", "--endpoint", ep); code != 0 {
		t.Errorf("环境变量在场时 --fresh 不该失败: code=%d out=%q stderr=%q", code, out, errOut)
	}
	// 普通执行当然也要能用。
	if code, out, _ := runCLI(t, "-c", "echo plain", "--endpoint", ep); code != 0 || !strings.Contains(out, "plain") {
		t.Errorf("环境变量在场时普通执行失败: code=%d out=%q", code, out)
	}

	// 方向二:显式写出来的组合**仍然**必须是用法错误 ——
	// 修 bug 不能把校验一起废掉,否则又回到"静默忽略,以为隔离生效了"。
	for _, ctl := range []string{"--serve", "--stop", "--status"} {
		if code, _, _ := runCLI(t, "--session", "x", ctl, "--endpoint", ep); code != 2 {
			t.Errorf("显式 --session %s: code=%d want 2", ctl, code)
		}
	}
	if code, _, _ := runCLI(t, "--fresh", "--session", "x", "-c", "echo x", "--endpoint", ep); code != 2 {
		t.Errorf("显式 --fresh --session: code=%d want 2", code)
	}
}

// run 的主路径:真拉起一个 daemon(端点指向临时目录),跑一条命令。
func TestEchoRunsThroughARealDaemon(t *testing.T) {
	ep := tmpEndpoint(t)
	t.Cleanup(func() { _, _, _ = runCLI(t, "--stop", "--endpoint", ep) })
	startDaemon(t, ep)

	code, out, errOut := runCLI(t, "-c", "echo hi", "--endpoint", ep)
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if strings.TrimSpace(out) != "hi" {
		t.Fatalf("out=%q", out)
	}
}

// 位置参数也要当脚本(README 里承诺 `winbash ls -l` 这种写法)。
// --endpoint 必须写在位置参数**之前**:flag 包在遇到第一个非 flag 参数时就停下,
// 否则它会落进脚本里、而这条用例会去连默认端点(污染使用者自己的 daemon)。
func TestPositionalArgsBecomeTheScript(t *testing.T) {
	ep := tmpEndpoint(t)
	t.Cleanup(func() { _, _, _ = runCLI(t, "--stop", "--endpoint", ep) })
	startDaemon(t, ep)

	code, out, errOut := runCLI(t, "--endpoint", ep, "echo", "hello", "world")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if strings.TrimSpace(out) != "hello world" {
		t.Fatalf("位置参数没有被拼成脚本: out=%q", out)
	}
}

// -c 后面的位置参数按真 bash 语义绑成 $0/$1…:真 bash 对
// `bash -c 'echo $0 $1' foo bar` 输出 `foo bar`(第一个位置参数当 $0,其余当 $1+),
// 与脚本文件路径的绑法一致。修之前这些参数被**静默扔掉** —— 违反「不静默做错事」。
//
// --endpoint 必须写在位置参数之前,理由同 TestPositionalArgsBecomeTheScript。
func TestDashCPositionalArgsBindToDollar0And1(t *testing.T) {
	ep := tmpEndpoint(t)
	t.Cleanup(func() { _, _, _ = runCLI(t, "--stop", "--endpoint", ep) })
	startDaemon(t, ep)

	// 基本绑定:$0 = 第一个位置参数,$1 = 第二个,与真 bash 逐字一致。
	code, out, errOut := runCLI(t, "--endpoint", ep, "-c", "echo $0 $1", "foo", "bar")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if got := strings.TrimSpace(out); got != "foo bar" {
		t.Fatalf("got %q want %q", got, "foo bar")
	}

	// 参数按**参数**传,不是拼进脚本文本:引号与空格要原样活到 $0/$1。
	// 若被 join 进脚本,`a b` 会裂成两个词、`$0` 就只剩 `a`。
	// (绑法与 bash 一致:第一个位置参数是 $0,`a b` 是 $0、`c d` 才是 $1。)
	code, out, errOut = runCLI(t, "--endpoint", ep, "-c", `echo "[$0] [$1]"`, "a b", "c d")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if got := strings.TrimSpace(out); got != "[a b] [c d]" {
		t.Fatalf("参数没有按原样绑定: got %q want %q", got, "[a b] [c d]")
	}
}

// 无位置参数的 -c 行为不变:$1/$2 照旧为空(修之前后都一样),不凭空多出参数。
// $0 的默认值(interp 的兜底)刻意不钉 —— 那是另一件事,本任务不引入新行为。
func TestDashCWithoutPositionalArgsUnchanged(t *testing.T) {
	ep := tmpEndpoint(t)
	t.Cleanup(func() { _, _, _ = runCLI(t, "--stop", "--endpoint", ep) })
	startDaemon(t, ep)

	code, out, errOut := runCLI(t, "-c", `echo "[$1] [$2]"`, "--endpoint", ep)
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if got := strings.TrimSpace(out); got != "[] []" {
		t.Fatalf("无位置参数时 $1/$2 应当为空: got %q", got)
	}
}

// 这条命令注入的 $1 不得泄漏给同一会话的下一条命令:runner.Params 是跨 Run 持久的
// 会话状态,注入后必须还原(见 shell.RunArgs)。同一条 daemon、同一个默认会话,
// 第二条命令的 $1 必须为空。
func TestDashCPositionalArgsDoNotLeakIntoNextCommand(t *testing.T) {
	ep := tmpEndpoint(t)
	t.Cleanup(func() { _, _, _ = runCLI(t, "--stop", "--endpoint", ep) })
	startDaemon(t, ep)

	if code, _, errOut := runCLI(t, "--endpoint", ep, "-c", "echo $1", "leaked"); code != 0 {
		t.Fatalf("预热失败: code=%d stderr=%q", code, errOut)
	}
	code, out, errOut := runCLI(t, "--endpoint", ep, "-c", `echo "[$1]"`)
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if got := strings.TrimSpace(out); got != "[]" {
		t.Fatalf("位置参数泄漏进了同会话的下一条命令: got %q want \"[]\"", got)
	}
}

// --status 看到 daemon,--stop 停掉它,之后 --status 必须失败(而不是又把它拉起来)。
func TestStatusThenStopThenStatusFails(t *testing.T) {
	ep := tmpEndpoint(t)
	t.Cleanup(func() { _, _, _ = runCLI(t, "--stop", "--endpoint", ep) })
	startDaemon(t, ep)

	if code, _, errOut := runCLI(t, "-c", "echo warmup", "--endpoint", ep); code != 0 {
		t.Fatalf("预热失败: code=%d stderr=%q", code, errOut)
	}

	code, out, errOut := runCLI(t, "--status", "--endpoint", ep)
	if code != 0 {
		t.Fatalf("--status 失败: code=%d stderr=%q", code, errOut)
	}
	if !strings.Contains(out, "pid=") {
		t.Fatalf("--status 该报出 pid: %q", out)
	}

	if code, _, errOut = runCLI(t, "--stop", "--endpoint", ep); code != 0 {
		t.Fatalf("--stop 失败: code=%d stderr=%q", code, errOut)
	}

	code, _, errOut = runCLI(t, "--status", "--endpoint", ep)
	if code != 1 {
		t.Fatalf("停掉之后 --status 该失败,而不是把 daemon 又拉起来: code=%d stderr=%q", code, errOut)
	}
}

// --serve 写不出端口文件时必须**快速失败**并给非零退出码(拿目录当端口文件路径)。
func TestServeFailsFastOnBadEndpoint(t *testing.T) {
	dir := t.TempDir() // 用一个**目录**当端口文件路径,写它一定失败

	code, _, errOut := runCLI(t, "--serve", "--endpoint", dir)
	if code != 1 {
		t.Fatalf("code=%d want 1", code)
	}
	if !strings.Contains(errOut, "serve") {
		t.Fatalf("stderr 该点名 serve: %q", errOut)
	}
}
