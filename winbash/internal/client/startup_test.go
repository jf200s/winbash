package client

// 本文件钉的是**启动锁与 ensureDaemonExcept 的自愈循环**:
//   - acquireStartLock:抢锁 / 放锁 / alreadyUp / 陈旧锁接管;
//   - ensureDaemonExcept:残留端口文件指向死地址时,一次都不许把它交回来,
//     无论是「等锁期间别人交上来」还是「自己 spawn 之后等覆盖写」。
//
// 这些路径在生产里的失败形态是「连不上之后再连还是连不上」「8 个 client 起出
// 8 个 daemon」—— 没有崩溃、没有日志,只有原地打转和资源泄漏,回归了极难发现。
//
// 构造手法沿用 oneshot_test.go 的假 daemon 与端口文件。能不真 spawn 就不 spawn
// (测试进程的 os.Executable() 是 winbash.test,不认识 --serve);唯一真跑
// spawnDaemon 的是 TestEnsureDaemonExceptSpawnsAndWaitsForFreshAddr,那个子进程
// 会在 flag 解析处立即退出,无副作用。

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"winbash/internal/ipc"
)

// liveDaemon 起一个正常应答的假 daemon,给「新 daemon 已就绪」的构造用。
func liveDaemon(t *testing.T) *fakeDaemon {
	t.Helper()
	return startFakeDaemon(t, func(conn net.Conn, seq int) {
		defer conn.Close()
		readRequest(t, conn)
		reply(t, conn, ipc.Response{OK: true})
	})
}

// acquireStartLock 的三个返回值各有各的契约,拆开各钉一个。
func TestAcquireStartLock(t *testing.T) {
	t.Run("抢到锁,release 删掉锁文件", func(t *testing.T) {
		ep := filepath.Join(t.TempDir(), "wb.port")
		release, alreadyUp, err := acquireStartLock(ep)
		if err != nil {
			t.Fatalf("空地上抢锁不该失败: %v", err)
		}
		if release == nil || alreadyUp {
			t.Fatalf("应当抢到锁: release==nil:%v alreadyUp=%v", release == nil, alreadyUp)
		}
		if _, serr := os.Stat(startLockPath(ep)); serr != nil {
			t.Fatalf("持锁期间锁文件应当存在: %v", serr)
		}
		release()
		if _, serr := os.Stat(startLockPath(ep)); !os.IsNotExist(serr) {
			t.Fatalf("release 应当删掉自己建的锁文件, stat err = %v", serr)
		}
	})

	t.Run("别人持锁且端口文件已出现 → alreadyUp,别人的锁原样保留", func(t *testing.T) {
		dir := t.TempDir()
		ep := filepath.Join(dir, "wb.port")
		lockPath := startLockPath(ep)
		// 「别人」的锁:mtime 是新的,不会被当陈旧锁接管。
		if err := os.WriteFile(lockPath, []byte("999\n0\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		writePortFile(t, ep, "127.0.0.1:62615", "tok")

		release, alreadyUp, err := acquireStartLock(ep)
		if err != nil {
			t.Fatalf("alreadyUp 不算失败: %v", err)
		}
		if !alreadyUp || release != nil {
			t.Fatalf("应当报 alreadyUp 且无锁可放: alreadyUp=%v release!=nil:%v", alreadyUp, release != nil)
		}
		if _, serr := os.Stat(lockPath); serr != nil {
			t.Fatalf("别人的锁不该被我们动: %v", serr)
		}
	})

	t.Run("陈旧锁(超过 staleLockBudget)被接管", func(t *testing.T) {
		ep := filepath.Join(t.TempDir(), "wb.port")
		lockPath := startLockPath(ep)
		if err := os.WriteFile(lockPath, []byte("被强杀的进程\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		// 把 mtime 拨到陈旧线之前,不用真的等 30s。
		old := time.Now().Add(-staleLockBudget - time.Minute)
		if err := os.Chtimes(lockPath, old, old); err != nil {
			t.Fatal(err)
		}

		release, alreadyUp, err := acquireStartLock(ep)
		if err != nil {
			t.Fatalf("陈旧锁应当被立即接管,而不是傻等到超时: %v", err)
		}
		if release == nil || alreadyUp {
			t.Fatalf("接管后应当持有自己的锁: release==nil:%v alreadyUp=%v", release == nil, alreadyUp)
		}
		release()
		if _, serr := os.Stat(lockPath); !os.IsNotExist(serr) {
			t.Fatalf("接管者的 release 应当删掉锁文件: %v", serr)
		}
	})

	t.Run("锁被接管后,原持锁者的 release 不删接管者的锁", func(t *testing.T) {
		ep := filepath.Join(t.TempDir(), "wb.port")
		lockPath := startLockPath(ep)
		release1, alreadyUp, err := acquireStartLock(ep)
		if err != nil {
			t.Fatalf("第一次抢锁不该失败: %v", err)
		}
		if release1 == nil || alreadyUp {
			t.Fatalf("第一次应当抢到锁: release==nil:%v alreadyUp=%v", release1 == nil, alreadyUp)
		}
		// 把锁的 mtime 拨过陈旧线:模拟原持锁者被暂停超过 staleLockBudget,
		// 不用真的等 30s(上面「陈旧锁被接管」子测试同款手法)。
		old := time.Now().Add(-staleLockBudget - time.Minute)
		if err := os.Chtimes(lockPath, old, old); err != nil {
			t.Fatal(err)
		}
		release2, alreadyUp, err := acquireStartLock(ep)
		if err != nil {
			t.Fatalf("陈旧锁应当被第二次 acquire 接管: %v", err)
		}
		if release2 == nil || alreadyUp {
			t.Fatalf("第二次应当接管成功: release==nil:%v alreadyUp=%v", release2 == nil, alreadyUp)
		}
		// 此刻锁文件已是接管者重建的那把。同一测试进程 pid 相同,release1 靠
		// 时间戳行认出"这不是我写下的锁" —— 绝不能删,否则第三个进程就能并发 spawn。
		release1()
		if _, serr := os.Stat(lockPath); serr != nil {
			t.Fatalf("被接管后,原持锁者的 release 不该删锁文件: %v", serr)
		}
		// 接管者自己的 release 语义不变:照删。
		release2()
		if _, serr := os.Stat(lockPath); !os.IsNotExist(serr) {
			t.Fatalf("接管者的 release 应当删掉自己的锁: %v", serr)
		}
	})
}

// alreadyUp 也不许信死地址:等锁期间持锁者交上来的端口文件若是 deadAddr 本尊,
// 必须退回去继续等,直到出现**新**地址才返回。
//
// 这正是「删不掉的残留文件」缺陷的另一半:文件**可读**(所以 acquireStartLock
// 报 alreadyUp),但内容就是刚才连不上的那个地址 —— 信任它就是原样回到故障里。
// 锁文件由「别人」一直持着,所以这条只能走 alreadyUp 分支,不会碰 spawn。
func TestEnsureDaemonExceptDoesNotTrustDeadAddrFromLockHolder(t *testing.T) {
	dir := t.TempDir()
	ep := filepath.Join(dir, "wb.port")
	lockPath := startLockPath(ep)
	dead := deadAddr(t)

	writePortFile(t, ep, dead, "stale-token")
	if err := os.WriteFile(lockPath, []byte("999\n0\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	live := liveDaemon(t)
	// 「持锁者」100ms 后做它起好 daemon 后的第一件事:覆盖写端口文件。
	go func() {
		time.Sleep(100 * time.Millisecond)
		if werr := os.WriteFile(ep, []byte(live.addr+"\nfresh-token\n"), 0o600); werr != nil {
			t.Errorf("模拟持锁者写端口文件: %v", werr)
		}
	}()

	info, err := ensureDaemonExcept(ep, dead, 0)
	if err != nil {
		t.Fatalf("持锁者起好新 daemon 后应当拿到它: %v", err)
	}
	if info.addr != live.addr {
		t.Fatalf("返回的 addr = %q,期望新 daemon 的 %q(死地址 %q 一次都不该被交回来)",
			info.addr, live.addr, dead)
	}
	if info.token != "fresh-token" {
		t.Fatalf("返回的 token = %q,期望 fresh-token", info.token)
	}
	// alreadyUp 路径没有锁可放:别人的锁必须原样保留。
	if _, serr := os.Stat(lockPath); serr != nil {
		t.Fatalf("alreadyUp 时不该动别人的锁: %v", serr)
	}
}

// 抢到锁、重读仍不信(还是死地址)→ 真的去 spawn,然后等的是「端口文件里出现
// 非 deadAddr 的地址」,等到了就放锁返回。
//
// 若这里的判据退化成「文件存在就信」,它会立刻把死地址交回来;若退化成「永远
// 不等」,它会在 20s 后报错 —— 两种回归都逃不出这三条断言。spawnDaemon 在测试里
// 会真的执行:测试二进制不认识 --serve,flag 解析失败立即退出,无副作用;我们
// 扮演新 daemon 的第一步动作(覆盖写端口文件)。
func TestEnsureDaemonExceptSpawnsAndWaitsForFreshAddr(t *testing.T) {
	dir := t.TempDir()
	ep := filepath.Join(dir, "wb.port")
	dead := deadAddr(t)
	writePortFile(t, ep, dead, "stale-token")

	live := liveDaemon(t)
	go func() {
		time.Sleep(300 * time.Millisecond)
		if werr := os.WriteFile(ep, []byte(live.addr+"\nfresh-token\n"), 0o600); werr != nil {
			t.Errorf("模拟新 daemon 覆盖写端口文件: %v", werr)
		}
	}()

	info, err := ensureDaemonExcept(ep, dead, 0)
	if err != nil {
		t.Fatalf("新地址出现后应当返回它: %v", err)
	}
	if info.addr != live.addr || info.token != "fresh-token" {
		t.Fatalf("addr=%q token=%q,期望 %q / fresh-token", info.addr, info.token, live.addr)
	}
	// 这条路径的锁是我们自己抢的:返回前必须已经放掉。
	if _, serr := os.Stat(startLockPath(ep)); !os.IsNotExist(serr) {
		t.Fatalf("自己抢的锁应当在返回前放掉, stat err = %v", serr)
	}
}
