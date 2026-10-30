package client

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"winbash/internal/ipc"
)

// 这个文件补的是 oneShot / exchange 里的**接线**:谓词写对不等于接对了,
// 而 client_test.go 里此前的用例全是纯函数,一个都碰不到那四行。
//
// 做法是一个**假 daemon**:本地 listener + 手写端口文件(两行:地址、token)。
// 不用真 daemon,因为要构造的是「daemon 回了什么」——401、超长帧、源端读失败
// ——这些在真 daemon 上要么构造不出来,要么要靠竞态。
//
// 刻意**不**为测试改造生产代码(项目既有价值观,见 daemon/state_test.go 开头):
// oneShot 仍然自己 dial,我们只是让端口文件指向一个我们控制的 listener。

// fakeDaemon 是一个只按脚本回话的本地"daemon"。
type fakeDaemon struct {
	endpoint string
	addr     string
	ln       net.Listener
	conns    atomic.Int32
}

// startFakeDaemon 起一个 listener 并写好端口文件;每条连接交给 handler。
//
// handler 拿到的 seq 从 1 开始计数,用来断言"到底重试了没有"。
func startFakeDaemon(t *testing.T, handler func(conn net.Conn, seq int)) *fakeDaemon {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起假 daemon 失败: %v", err)
	}
	fd := &fakeDaemon{ln: ln, addr: ln.Addr().String()}
	fd.endpoint = filepath.Join(t.TempDir(), "fake.port")
	writePortFile(t, fd.endpoint, fd.addr, "fake-token")
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			seq := int(fd.conns.Add(1))
			go handler(c, seq)
		}
	}()
	return fd
}

// writePortFile 按 client 期望的格式写端口文件:第一行地址,第二行 token。
func writePortFile(t *testing.T, path, addr, token string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(addr+"\n"+token+"\n"), 0o600); err != nil {
		t.Fatalf("写端口文件: %v", err)
	}
}

// readRequest 读掉一个请求帧(不校验内容)。
func readRequest(t *testing.T, conn net.Conn) ipc.Request {
	t.Helper()
	raw, err := ipc.ReadMessage(conn)
	if err != nil {
		t.Errorf("假 daemon 读请求失败: %v", err)
		return ipc.Request{}
	}
	req, err := ipc.DecodeRequest(raw)
	if err != nil {
		t.Errorf("假 daemon 解请求失败: %v", err)
	}
	return req
}

// reply 回一个响应帧。
func reply(t *testing.T, conn net.Conn, resp ipc.Response) {
	t.Helper()
	raw, err := ipc.EncodeResponse(resp)
	if err != nil {
		t.Errorf("编码响应失败: %v", err)
		return
	}
	if err := ipc.WriteMessage(conn, raw); err != nil {
		t.Errorf("写响应失败: %v", err)
	}
}

// withStdin 把 os.Stdin 换成一个可从测试控制的 reader。
//
// 必须换:exchange 里对流式 stdin 直接读 os.Stdin,而在 go test 下它可能是终端,
// io.Copy 会阻塞到测试超时。cleanup 会恢复原值(包内测试都是串行的)。
func withStdin(t *testing.T, data string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("建管道: %v", err)
	}
	go func() {
		_, _ = io.WriteString(w, data)
		_ = w.Close()
	}()
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = old
		_ = r.Close()
	})
}

// ① 鉴权失败 + 流式 stdin → **不重发**。
//
// 判据是连接数:修前(或极性写反)会重发,于是假 daemon 收到第二条连接,
// 而第二支 io.Copy 会与第一支瓜分同一个 os.Stdin。
func TestOneShotDoesNotRetryAuthWhenStdinIsStreaming(t *testing.T) {
	withStdin(t, "payload-that-only-one-connection-may-read")

	fd := startFakeDaemon(t, func(conn net.Conn, seq int) {
		defer conn.Close()
		readRequest(t, conn)
		reply(t, conn, ipc.Response{Auth: true, Exit: 1})
		// 回完先排干入站再关:此刻流式 stdin 的字节还堆在接收缓冲里没被读,
		// Windows 对「带着未读数据的 close」回 RST 而不是 FIN —— client 的
		// 401 响应会被 RST 打掉,读到的是「连接被强制关闭」而不是响应。排干后
		// 才是优雅的 FIN;client 半关了写端,EOF 很快到,2s 是兜底。
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _ = io.Copy(io.Discard, conn)
	})

	resp, err := oneShot(fd.endpoint, ipc.Request{
		Script:    "cat",
		StdinMode: ipc.StdinStream,
		StdinLen:  ipc.StdinUntilEOF,
	}, false, 5*time.Second, 0)
	if err != nil {
		t.Fatalf("oneShot 返回错误: %v", err)
	}
	if got := fd.conns.Load(); got != 1 {
		t.Fatalf("假 daemon 收到 %d 条连接,期望 1 条(流式 stdin 已被消费,不能重发)", got)
	}
	joined := strings.Join(resp.Errors, "\n")
	if !strings.Contains(joined, "不能自动重发") {
		t.Fatalf("应当说明为什么不能自动重发, got %q", joined)
	}
	if !strings.Contains(joined, "未执行") {
		t.Errorf("还应当说明命令本身未执行(否则调用方不敢重跑), got %q", joined)
	}
}

// ② 鉴权失败 + 无 stdin → **重发**(这条与上面配对,防止"干脆永不重发"也被当成修复)。
func TestOneShotRetriesAuthWithoutStdin(t *testing.T) {
	fd := startFakeDaemon(t, func(conn net.Conn, seq int) {
		defer conn.Close()
		readRequest(t, conn)
		if seq == 1 {
			reply(t, conn, ipc.Response{Auth: true, Exit: 1})
			return
		}
		reply(t, conn, ipc.Response{OK: true, Exit: 0, Out: []byte("second")})
	})

	resp, err := oneShot(fd.endpoint, ipc.Request{Script: "echo hi"}, false, 5*time.Second, 0)
	if err != nil {
		t.Fatalf("oneShot 返回错误: %v", err)
	}
	if got := fd.conns.Load(); got != 2 {
		t.Fatalf("假 daemon 收到 %d 条连接,期望 2 条(无 stdin 时应当重发一次)", got)
	}
	if string(resp.Out) != "second" {
		t.Fatalf("应当拿到第二次的响应, got %q", resp.Out)
	}
}

// ③ 确定性失败(超长帧)不重试,而且**不能**为此删掉端口文件。
//
// 这是接线最容易写错的一处:谓词 isPermanentFailure 有单测,但它在 if 里怎么
// 组合没人管。判据选「端口文件还在」而不是「连接数」—— 重试路径的第一件事是
// forgetStaleEndpoint,它会把端口文件删掉(或由新 daemon 覆盖写)。文件还在
// 就证明根本没走那条路。
func TestOneShotDoesNotRetryOversizeFrame(t *testing.T) {
	fd := startFakeDaemon(t, func(conn net.Conn, seq int) {
		defer conn.Close()
		// 什么都不读:这条用例里 client 在**写出请求之前**就因超长放弃,
		// 读会拿到 EOF —— 那不是本用例要断言的东西。
		time.Sleep(50 * time.Millisecond)
	})

	// 让 Encode 后的 JSON 超过单帧上限(64 MiB)。多给 1 KiB 是给 JSON 的包装留余量。
	huge := strings.Repeat("x", (64<<20)+1024)

	_, err := oneShot(fd.endpoint, ipc.Request{Script: huge}, false, 5*time.Second, 0)
	if err == nil {
		t.Fatal("超长帧必须报错")
	}
	if !strings.Contains(err.Error(), "重试不会成功") {
		t.Fatalf("错误信息应当说明重试无意义, got %q", err.Error())
	}
	if _, statErr := os.Stat(fd.endpoint); statErr != nil {
		t.Fatalf("端口文件被删了 —— 说明永久失败仍走了「删端口 + spawn」那条重试路径: %v", statErr)
	}
}

// ④ 流式 stdin 的**源端**读错误必须一路走到 resp.Errors。
//
// 这条链是 exchange → append(resp.Errors) → RunTimeout 原样返回 → main.go 打 stderr。
// 中间断任何一环,命令都会拿着残缺输入照常 exit 0 —— 静默错值,最难事后发现。
//
// 怎么造源端错误:用一个已关闭的读端,Read 立刻返回 "file already closed"
// (不是 EOF,所以会被 recordingReader 记下来)。
func TestOneShotSurfacesStdinSourceError(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("建管道: %v", err)
	}
	_ = w.Close()
	_ = r.Close() // 读端一关,Read 就报错而不是 EOF

	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = old })

	fd := startFakeDaemon(t, func(conn net.Conn, seq int) {
		defer conn.Close()
		readRequest(t, conn)
		// 稍等一下:给 pump 时间去撞上那个读错误。它一旦完成就会把结果
		// 放进 buffered channel,而 pump.err() 是非阻塞取——取早了就是空。
		time.Sleep(300 * time.Millisecond)
		reply(t, conn, ipc.Response{OK: true, Exit: 0, Out: []byte("ran")})
	})

	resp, err := oneShot(fd.endpoint, ipc.Request{
		Script:    "cat",
		StdinMode: ipc.StdinStream,
		StdinLen:  ipc.StdinUntilEOF,
	}, false, 10*time.Second, 0)
	if err != nil {
		t.Fatalf("oneShot 返回错误: %v", err)
	}
	// 命令本身成功了,所以退出码不该被改写 —— 但输入不完整这件事必须说出来。
	if resp.Exit != 0 {
		t.Errorf("client 不该改写 daemon 给的退出码, got %d", resp.Exit)
	}
	joined := strings.Join(resp.Errors, "\n")
	if !strings.Contains(joined, "不完整输入") {
		t.Fatalf("stdin 源端读失败必须出现在 resp.Errors 里, got %q", joined)
	}
}

// ⑤ --stop/--status 的鉴权重试不能把 daemon 拉起来。
//
// 构造:第一次连接回 401,同时把端口文件改指向一个**死地址**。若 mustExist 被
// 正确传下去,dial 会直接报「没在运行」并把端口文件留在原地;若传成 false,
// dial 会先 forgetStaleEndpoint 删掉端口文件、再走 spawn —— 断言抓的是后者。
func TestAuthRetryRespectsMustExist(t *testing.T) {
	// 先声明再赋值:handler 闭包里要用 fd.endpoint,而 `fd := …` 的右侧作用域里
	// fd 还没声明。
	var fd *fakeDaemon
	fd = startFakeDaemon(t, func(conn net.Conn, seq int) {
		defer conn.Close()
		readRequest(t, conn)
		// 把端口文件指向一个必定连不上的地址,模拟「读完 token 后旧 daemon 退出」。
		writePortFile(t, fd.endpoint, "127.0.0.1:1", "stale-token")
		reply(t, conn, ipc.Response{Auth: true, Exit: 1})
	})

	_, err := oneShot(fd.endpoint, ipc.Request{Script: "echo hi"}, true, 5*time.Second, 0)
	if err == nil {
		t.Fatal("应当返回错误")
	}
	if !strings.Contains(err.Error(), "没在运行") {
		t.Fatalf("mustExist=true 时应当报「没在运行」而不是去 spawn, got %q", err.Error())
	}
	if _, statErr := os.Stat(fd.endpoint); statErr != nil {
		t.Fatalf("端口文件被删了 —— 说明 mustExist 没传到 dial,重试走了会 spawn 的那条路: %v", statErr)
	}
}

// ⑥ dial 必须返回「实际连上的是哪个 daemon」。
//
// 这是 W1 的修复点。旧实现只返回 conn:旧地址连不上时,它内部会换一个 daemon
// (ensureDaemonExcept 可能读到别人刚写好的新端口文件),而调用方手里还是旧 token
// —— 拿旧 token 连新 daemon,必然 401。带流式 stdin 时那条 401 救不回来
// (stdin 已被消费),本来能成功的调用就变成失败。
//
// 构造手法完全不碰 spawn:
//   - 传给 dial 的 info 指向一个**死地址**(端口被占过又放掉,连不上);
//   - 端口文件里写的是**另一个**活着的假 daemon 的地址与 token;
//   - dial 连死地址失败 → forgetStaleEndpoint 因为"文件第一行不是死地址"而不删
//     → ensureDaemonExcept 读到活地址且它不是 deadAddr → 信任并直接返回
//     —— 不会 spawn,所以这条用例毫秒级完成,不会干等 startupBudget(20s)。
func TestDialReturnsTheEndpointItActuallyConnectedTo(t *testing.T) {
	live := startFakeDaemon(t, func(conn net.Conn, seq int) {
		defer conn.Close()
		req := readRequest(t, conn)
		// 把收到的 token 回显出来,断言用的就是它。
		reply(t, conn, ipc.Response{OK: true, Out: []byte(req.Token)})
	})

	// 造一个"死地址":先占住一个端口再放掉,之后连它必定被拒。
	dead := deadAddr(t)

	endpoint := filepath.Join(t.TempDir(), "w1.port")
	// 端口文件指向活 daemon —— 注意它与传给 dial 的死的 info 不是同一个。
	writePortFile(t, endpoint, live.addr, "live-token")

	conn, used, err := dial(endpoint, endpointInfo{addr: dead, token: "stale-token"}, false, 0)
	if err != nil {
		t.Fatalf("dial 应当回落到端口文件里的活地址, got %v", err)
	}
	defer conn.Close()

	if used.addr != live.addr {
		t.Fatalf("返回的 addr = %q,期望活 daemon 的 %q", used.addr, live.addr)
	}
	if used.token != "live-token" {
		t.Fatalf("返回的 token = %q,期望 %q —— 旧实现只返回 conn,调用方会继续用死地址那个 token",
			used.token, "live-token")
	}

	// 端到端确认:用它回填的 token 能通过鉴权(假 daemon 只是把它回显出来)。
	req := ipc.Request{Script: "echo hi", Token: used.token}
	resp, xerr := exchange(conn, req, 5*time.Second)
	if xerr != nil {
		t.Fatalf("用回填的 token 发请求失败: %v", xerr)
	}
	if string(resp.Out) != "live-token" {
		t.Fatalf("假 daemon 收到的 token = %q,期望 live-token", resp.Out)
	}
}

// deadAddr 返回一个「刚刚还开着、现在已经关掉」的本地地址。
//
// 用它而不是 "127.0.0.1:1":后者在部分 Windows 环境上是 SYN 超时(要等满
// dialTimeout),而刚关闭的端口会立刻回 RST,用例能毫秒级结束。
func deadAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占位 listener 建不起来: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// ⑦ 发送阶段的失败(请求帧没能完整写出)必须走重试路径:forgetStaleEndpoint →
// ensureDaemonExcept 拿一个新 daemon → 用**新 daemon 的 token** 重发。
//
// 这是 ③(超长帧就地报错)的对照面:两类失败底层长得一样(都是连接断了),
// 分叉只在 oneShot 那一个 if 上 —— permanent 就地报错,瞬时发送失败重试一次。
// 只钉谓词不钉接线的话,把这两类掉个头测试照样全绿。
//
// 怎么确定性造出「发送阶段失败」:请求体 60 MiB(单帧上限 64 MiB 之内),对端
// 在 accept 后立刻 SetLinger(0)+Close 发 RST。两个实测事实撑起这里的时序:
//   - Windows 回环会把**任意大小**的写整个吃进内核缓冲(8 MiB、60 MiB 都实测
//     如此),对端不读也不会让写阻塞 —— 所以「靠缓冲区塞满让写失败」在本机不
//     成立,RST 只会打死读端;
//   - 60 MiB 的 JSON 编码要几十毫秒,而 RST 在握手完成后毫秒级就到 —— client
//     第一次 write 之前,socket 必然已被标记 reset,写端直接报错。
//
// 于是失败铁定落在 WriteMessage 里:「一个字节都没进 daemon 的读端」,读响应
// 根本没开始,重发是安全的。睡 20ms 再动手是让 dial 稳稳返回,别让 RST 抢在
// 握手收尾前到。
func TestOneShotRetriesSendFailureOnNewDaemon(t *testing.T) {
	// 第二个 daemon:重试的目标。它回显收到的 token,证明重发用的是**新**凭据
	// (旧实现的 dial 只返回 conn 不返回 info,这里就会拿旧 token 连新 daemon)。
	fd2 := startFakeDaemon(t, func(conn net.Conn, seq int) {
		defer conn.Close()
		req := readRequest(t, conn)
		reply(t, conn, ipc.Response{OK: true, Out: []byte(req.Token)})
	})

	var fd1 *fakeDaemon
	fd1 = startFakeDaemon(t, func(conn net.Conn, seq int) {
		defer conn.Close()
		// 不读任何东西;先把端口文件改指向新 daemon(模拟「新 daemon 接管」),
		// 再发 RST。顺序不能反:重试路径要先能从端口文件里拿到一个**不是死地址**
		// 的新地址,否则它只能去 spawn(等 20s)。
		time.Sleep(20 * time.Millisecond)
		writePortFile(t, fd1.endpoint, fd2.addr, "fresh-token")
		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.SetLinger(0)
		}
	})

	resp, err := oneShot(fd1.endpoint, ipc.Request{Script: strings.Repeat("x", 60<<20)},
		false, 10*time.Second, 0)
	if err != nil {
		t.Fatalf("发送阶段的瞬时失败应当被重试救回来, got %v", err)
	}
	if got := fd1.conns.Load(); got != 1 {
		t.Errorf("第一个 daemon 收到 %d 条连接,期望 1(重试不该回头找旧 daemon)", got)
	}
	if got := fd2.conns.Load(); got != 1 {
		t.Errorf("第二个 daemon 收到 %d 条连接,期望 1(重试应当发给新 daemon)", got)
	}
	if string(resp.Out) != "fresh-token" {
		t.Errorf("重发应当携带新 daemon 的 token, got %q", resp.Out)
	}
	// 端口文件此刻指向**新** daemon —— forgetStaleEndpoint 拿旧地址比对,不许误删它。
	b, rerr := os.ReadFile(fd1.endpoint)
	if rerr != nil {
		t.Fatalf("新 daemon 的端口文件不该被重试路径删掉: %v", rerr)
	}
	if !strings.Contains(string(b), fd2.addr) {
		t.Errorf("端口文件被改动了: %q", string(b))
	}
}

// ⑧ 请求**已送达**(对端把请求帧完整读走了)之后连接断开 → 就地报错,绝不重放。
//
// 这条与 ⑦ 是同一枚硬币的两面:⑦ 的失败发生在写出阶段,重发安全;这里请求帧
// 已经完整交给了 daemon,它可能已经把脚本跑了 —— 重放就是同一条脚本跑两遍
// (mkdir / git commit / 写文件都被双重执行)。判据:假 daemon 只收到 1 条连接。
// 谓词单测(TestIsSendFailureDistinguishesSendFromRead)钉的是分类,这里钉的是
// 分类结果真的接到了「不重试」上。
func TestOneShotDoesNotReplayAfterRequestWasDelivered(t *testing.T) {
	fd := startFakeDaemon(t, func(conn net.Conn, seq int) {
		defer conn.Close()
		readRequest(t, conn) // 完整读走请求帧 —— 从这一刻起「命令可能已经执行」
		// 不回响应,直接断开:模拟 daemon 在执行中途消失。
	})

	_, err := oneShot(fd.endpoint, ipc.Request{Script: "mkdir important"}, false, 5*time.Second, 0)
	if err == nil {
		t.Fatal("对端没回响应,应当报错")
	}
	if !strings.Contains(err.Error(), "可能已经执行") {
		t.Fatalf("错误必须说明命令可能已执行(调用方要据此先查副作用再决定重跑), got %q", err.Error())
	}
	if got := fd.conns.Load(); got != 1 {
		t.Fatalf("假 daemon 收到 %d 条连接,期望 1 —— 第二条连接就意味着重放", got)
	}
	// 读失败路径连 ensureDaemonExcept 都不该去问,更不该碰端口文件。
	if _, serr := os.Stat(fd.endpoint); serr != nil {
		t.Fatalf("端口文件不该被读失败路径动: %v", serr)
	}
}

// ⑨ 鉴权重试时端口文件已经不可读 → 明确报「端口文件已不可读」,而不是哑着失败。
//
// 构成:daemon 回 401 的同时把端口文件删掉(极端形态:读完 token 后 daemon 退出,
// 文件也跟着没了)。retryAfterAuthFailure 的第一件事是重读端口文件,读不到就该把
// 这件事说出来 —— 修之前那条读错误会被一层层包成一句没头没尾的「读取失败」。
func TestAuthRetryReportsUnreadablePortFile(t *testing.T) {
	var fd *fakeDaemon
	fd = startFakeDaemon(t, func(conn net.Conn, seq int) {
		defer conn.Close()
		readRequest(t, conn)
		_ = os.Remove(fd.endpoint)
		reply(t, conn, ipc.Response{Auth: true, Exit: 1})
	})

	_, err := oneShot(fd.endpoint, ipc.Request{Script: "echo hi"}, false, 5*time.Second, 0)
	if err == nil {
		t.Fatal("端口文件没了,鉴权重试无从谈起,应当报错")
	}
	if !strings.Contains(err.Error(), "端口文件已不可读") {
		t.Fatalf("错误应当说明端口文件已不可读, got %q", err.Error())
	}
}
