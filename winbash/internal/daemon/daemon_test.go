package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"winbash/internal/client"
	"winbash/internal/shell"
)

// 计划里的 2 个用例照抄自 plan Task 14(调用方式按 client 的实际 API 落定:
// 用 client.Stop 而不是 client.Run(ep, "stop")—— 后者发的是**脚本**,
// daemon 会把 "stop" 当成一条命令去执行,根本不会停下)。

// TestMain 先摘掉 WINBASH_SESSION。
//
// 本包的 stop/status 用例走 client 包,而 client 见到 EnvNested 就判定"我在 winbash
// 会话里",于是刻意拒绝 --status/--stop(见 inSession)。从 winbash 里跑
// `go test ./...`(README「开发」节就是那么写的)时,③ 层派生给子进程的 EnvNested=1
// 会被继承下来 —— 这几条必然红,而且红得像产品 bug。
//
// 测试对自身环境的要求要自己声明,不能靠"你恰好在 Git Bash 里跑"。
func TestMain(m *testing.M) {
	_ = os.Unsetenv(shell.EnvNested)
	os.Exit(m.Run())
}

func uniqueEndpoint(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "winbash-test.port")
}

func waitForEndpoint(t *testing.T, ep string, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(ep); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("daemon 没在 %s 内写出端口文件 %s", budget, ep)
}

func TestStopActuallyStopsDaemon(t *testing.T) {
	ep := uniqueEndpoint(t)
	go Serve(ep, time.Minute, DefaultReadTimeout)
	waitForEndpoint(t, ep, 10*time.Second)

	if err := client.Stop(ep); err != nil {
		t.Fatal(err)
	}

	// 必须直接观察传输层本身,不能经 client.Run —— client 发现 daemon 没了会自动重拉,
	// 那会把「--stop 失效」这个 bug 完全掩盖掉(pwshx 上正是这样漏掉的)。
	// 约定:daemon 退出时删除自己的端口文件(见 Serve)。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(ep); os.IsNotExist(err) {
			return // 端口文件没了 = daemon 真的退出了
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("daemon still serving after stop; --stop did not exit the accept loop")
}

func TestDaemonExitsAfterIdleTimeout(t *testing.T) {
	ep := uniqueEndpoint(t)
	done := make(chan struct{})
	go func() {
		_ = Serve(ep, 300*time.Millisecond, DefaultReadTimeout)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("daemon did not exit after idle timeout")
	}
}

// ★ 一条跑得比 idle 还长的命令,结束后不能立刻被判成「已经空闲了整整一个 idle」。
//
// 这条钉的是 d.lastUsed 的刷新时机。修之前它只在 pickSession / statusText 里更新,
// 而 watchdog 判的是它 —— 于是一条跑了 5s 的命令、在 idle=2s 下,一结束就让
// since(d.lastUsed) 从 5s 起算 > 2s,watchdog 下一个 tick 就把 daemon 关了。
//
// 实测(修之前):idle=2s + `sleep 5`,命令结束后 1.2s 端口文件就没了;
// 而 idle=2s + `echo` 同条件仍存活 —— 说明问题只在「命令跑得比 idle 久」时才显形。
//
// 真实后果:任何超过 30 分钟(默认 idle)的 `go test` / 构建跑完之后,常驻 daemon
// 立刻消失,下一次调用要重新冷启 —— 正是本项目「常驻省时间」的反面。
//
// 判据用**端口文件**而不是 --status:后者自己会刷新 d.lastUsed,拿它当探针等于
// 边测边改被测物(第一版就是这么写成假绿的)。
func TestLongCommandDoesNotImmediatelyLookIdle(t *testing.T) {
	if testing.Short() {
		t.Skip("要真跑一条 5s 的命令")
	}
	ep := uniqueEndpoint(t)
	const idle = 2 * time.Second

	done := make(chan struct{})
	go func() {
		_ = Serve(ep, idle, DefaultReadTimeout)
		close(done)
	}()
	// 等端口文件出现(daemon 起来了)。复用现有辅助,不另写一套轮询。
	waitForEndpoint(t, ep, 10*time.Second)

	// 发一条比 idle 长的命令。走真 client(与线上同一条路径)。
	resp, err := client.Run(ep, "sleep 5; echo cmd-done", "", false)
	if err != nil {
		t.Fatalf("长命令失败: %v", err)
	}
	if resp.Exit != 0 {
		t.Fatalf("长命令 exit=%d out=%q", resp.Exit, resp.Out)
	}

	// 命令刚结束:daemon 必须还活着。
	// 1.2s < idle(2s),所以只要 d.lastUsed 在收工时被刷过就必然还活着;
	// 若没刷,它会被算成"已空闲 5s+"而在下一次 tick(≤1s)就退。
	time.Sleep(1200 * time.Millisecond)
	if _, err := os.Stat(ep); err != nil {
		t.Fatalf("长命令结束后 1.2s 端口文件就没了(daemon 已退出):" +
			"说明 d.lastUsed 没在收工时刷新 —— 任何比 idle 长的命令跑完都会白丢常驻")
	}

	// 反过来也要成立:真的空闲超过 idle 之后必须**正常**退出,
	// 否则这个修复就等于"永不退出",那也是坏的。
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("空闲超过 idle 之后 daemon 仍不退出:修复过头了")
	}
}

// status 要能说清 daemon 的现状(不拉起一个没在跑的 daemon)。
func TestStatusReportsAndDoesNotStartDaemon(t *testing.T) {
	ep := uniqueEndpoint(t)

	if _, err := client.Status(ep); err == nil {
		t.Fatal("Status must fail when no daemon is running, not silently start one")
	}
	if _, err := os.Stat(ep); err == nil {
		t.Fatal("Status started a daemon (port file appeared) — it must not")
	}

	go Serve(ep, time.Minute, DefaultReadTimeout)
	waitForEndpoint(t, ep, 10*time.Second)

	out, err := client.Status(ep)
	if err != nil {
		t.Fatal(err)
	}
	if out == "" {
		t.Fatal("status output is empty")
	}
	_ = client.Stop(ep)
}
