// `Serve` 与 `watchdog` 的入口边界。
//
// 这两处都是"参数不对时怎么办"的分支,平时不执行,但错了会静默降级:
//   - Serve 写不出端口文件却继续跑 → client 找不到 daemon,而错误只是"连不上";
//   - watchdog 在 idle<=0 时还去判空闲 → daemon 启动就自退(`--idle 0` 的语义是"不空闲退出")。

package daemon

import (
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"winbash/internal/client"
)

// ★ 写不出端口文件时 Serve 必须**立刻报错返回**,不能继续跑。
//
// 实测(`winbash --serve --endpoint Z:\no\such\dir\x.port`):
//
//	rc=1
//	stderr="winbash: serve: daemon: 写端口文件失败: open Z:\no\such\dir\x.port:
//	  The system cannot find the path specified."
//
// 为什么必须响亮失败:端口文件是 client 找到 daemon 的唯一途径。写不出来还继续跑,
// 就是一个谁都连不上、只能等空闲退出的孤儿进程 —— 而使用者看到的只是
// "connect 失败",根本不知道原因在这个目录上。
func TestServeFailsWhenEndpointUnwritable(t *testing.T) {
	// 一个不存在的目录 → os.WriteFile 必然失败。
	ep := filepath.Join(t.TempDir(), "no-such-subdir", "deep", "winbash.port")

	err := Serve(ep, time.Minute, DefaultReadTimeout)
	if err == nil {
		t.Fatal("端口文件写不出来,Serve 却返回了 nil(会留下一个连不上的孤儿进程)")
	}
	errText := err.Error()
	if !strings.Contains(errText, "端口文件") {
		t.Fatalf("错误信息没点明是端口文件的问题: %v", err)
	}
	// 要带上那个路径 —— 否则使用者不知道该修哪个目录。
	if !strings.Contains(errText, "no-such-subdir") {
		t.Fatalf("错误信息没带上失败的路径: %v", err)
	}
}

// ★ `idle <= 0` 时 watchdog 必须**立刻**返回(不启用空闲退出)。
//
// `--idle 0` 的语义是"不要因为空闲而退"。若 watchdog 照样跑起来,判据
// `time.Since(lastUsed) >= d.idle` 在 idle=0 时**恒为真**,daemon 会在第一个
// tick 就自退 —— 一条长命令跑到一半 daemon 就没了。
//
// **这条测试返工过一次**:第一版只断言"watchdog 会返回"(5s 窗口),结果变异验证
// 抓不住 —— 因为变异后它仍然会返回,只是**晚 1 秒**(ticker 触发后 shutdown 关掉
// done,循环下一轮命中 <-d.done 就退出)。那种"晚一点返回"正是 bug 本身,而宽松的
// 断言把它当成了正确行为,是典型的假绿。
//
// 现在断言的是"**立刻**返回":正确实现是纯 return(微秒级),起了 ticker 的实现
// 最快也要一个 tick(≥1s)。两者差三个数量级,100ms 的上限不会误伤。
func TestWatchdogNoOpsWhenIdleDisabled(t *testing.T) {
	for _, idle := range []time.Duration{0, -time.Second} {
		d := newTestDaemon()
		d.idle = idle
		// started/lastUsed 留零值也无妨:这个分支应当在读它们之前就返回。

		returned := make(chan struct{})
		go func() {
			d.watchdog()
			close(returned)
		}()

		start := time.Now()
		select {
		case <-returned:
			if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
				t.Fatalf("idle=%v 时 watchdog 用了 %v 才返回 —— "+
					"它起了 ticker,会在第一个 tick 就把 daemon 判成空闲并退出",
					idle, elapsed)
			}
			// 顺带确认它没有把 daemon 关停:done 必须还开着。
			select {
			case <-d.done:
				t.Fatalf("idle=%v 时 watchdog 把 daemon 关停了 —— "+
					"`--idle 0` 的语义是不因空闲退出", idle)
			default:
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("idle=%v 时 watchdog 没有返回 —— "+
				"它会在第一个 tick 就把 daemon 判成空闲并退出", idle)
		}
	}
}

// ★ `idle > 0` 时 watchdog 必须**继续跑**,直到 done 关闭。
//
// 上面那条如果实现成"watchdog 一律立刻返回",空闲退出就彻底失效了 ——
// 而那条测试抓不到(它只验证 idle<=0 时返回)。这里补另一个方向。
func TestWatchdogKeepsRunningUntilDone(t *testing.T) {
	d := newTestDaemon()
	d.idle = time.Hour // 一小时不会到,所以它只会因 done 而退出
	d.lastUsed = time.Now()

	returned := make(chan struct{})
	go func() {
		d.watchdog()
		close(returned)
	}()

	// 给它一点时间进入 select 循环,然后确认它**没有**提前返回。
	select {
	case <-returned:
		t.Fatal("idle>0 时 watchdog 不该立刻返回(否则空闲退出失效)")
	case <-time.After(300 * time.Millisecond):
	}

	// 关闭 done 之后它必须退出,否则每起一个 daemon 就漏一个 goroutine。
	close(d.done)
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("done 关闭后 watchdog 没有退出(goroutine 泄漏)")
	}
}

// ★ `readTimeout <= 0` 时 Serve 必须退回默认值,而不是"不设读预算"。
//
// 传 0 的意思是"没指定",不是"关掉保护"。若照着 0 走,handleConn 里那段
// `if readTimeout > 0` 会被整个跳过,于是"连上不说活的连接"能白占一个 goroutine
// 与一个 fd 直到对端消失 —— 正是 DefaultReadTimeout 要挡的攻击面。
//
// **这条测试返工过一次**(记下来,免得再犯):
// 第一版是"用 0 起服务,然后跑一条命令看它成功"。那条断言**没有鉴别力** ——
// 兜底漏掉时 daemon 照样能服务,只是不设读预算,所以它永远是绿的。
//
// 现在改成用**静默连接**去区分两条实现:
//   - 兜底生效(-1 → 5s):连接 1s 内活着(没被立刻断),约 5s 后被 daemon 关掉;
//   - 兜底漏掉但把 -1 直接给 SetReadDeadline:deadline 早已过期 → 1s 内立刻被断;
//   - 兜底漏掉且跳过设置:连接永不关闭 → 8s 窗口内读不到 EOF。
//
// 三个方向都被覆盖。窗口给得很宽(1s / 3~8s),不靠抢时序。
func TestServeDefaultsReadTimeoutWhenNonPositive(t *testing.T) {
	ep := uniqueEndpoint(t)
	// 传负数 —— 比 0 更能说明"非正值一律兜底",而不是恰好 0 被特判。
	go func() { _ = Serve(ep, time.Minute, -1) }()
	waitForEndpoint(t, ep, 10*time.Second)
	t.Cleanup(func() { _ = client.Stop(ep) })

	addr, _ := readEndpointFile(t, ep)
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// 1) 1 秒内必须**还活着**:若把 -1 当成 deadline 直接用,它早就过期了。
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("静默连接居然读到了数据")
	} else if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
		t.Fatalf("静默连接在 1s 内就被断开了(%v)—— "+
			"说明 -1 被当成读 deadline 直接用了,而不是兜底成 %v", err, DefaultReadTimeout)
	}

	// 2) 8 秒内必须被 daemon 关掉(兜底值 5s)。读不到 EOF 就意味着压根没设读预算。
	_ = conn.SetReadDeadline(time.Now().Add(8 * time.Second))
	start := time.Now()
	_, err = conn.Read(make([]byte, 1))
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("静默连接读到了数据,不该有")
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatalf("静默连接 8s 内没被关闭 —— readTimeout<=0 没有兜底成 %v,"+
			"「连上不说活」的连接会白占一个 goroutine 与一个 fd", DefaultReadTimeout)
	}
	// 兜底值是 5s:给 (3s, 8s) 的宽松窗口,既能排除"立刻断",也能排除"更长的值"。
	if elapsed < 3*time.Second {
		t.Fatalf("静默连接只撑了 %v 就被关,不像 %v 的兜底", elapsed, DefaultReadTimeout)
	}
}
