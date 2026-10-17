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

// run 的主路径:真拉起一个 daemon(端点指向临时目录),跑一条命令。
func TestEchoRunsThroughARealDaemon(t *testing.T) {	ep := tmpEndpoint(t)
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
