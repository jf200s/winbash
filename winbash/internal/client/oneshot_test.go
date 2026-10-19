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
