package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"winbash/internal/client"
)

// 计划里的 2 个用例照抄自 plan Task 14(调用方式按 client 的实际 API 落定:
// 用 client.Stop 而不是 client.Run(ep, "stop")—— 后者发的是**脚本**,
// daemon 会把 "stop" 当成一条命令去执行,根本不会停下)。

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
	go Serve(ep, time.Minute)
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
		_ = Serve(ep, 300*time.Millisecond)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("daemon did not exit after idle timeout")
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

	go Serve(ep, time.Minute)
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
