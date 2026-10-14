// Package daemon 是 winbash 的常驻服务端。
//
// 传输层用 TCP loopback 而不是命名管道：Go 标准库没有 CreateNamedPipe 封装，
// 而项目只允许一个直接依赖(mvdan/sh)。127.0.0.1 仅本机可达，对本机开发工具可接受。
//
// 但「仅本机可达」不等于「只有你连得上」—— 同机**其他用户**与沙箱进程同样连得上。
// 所以端口文件是两行(地址 + 随机 token),每个请求都要带对 token(见 ipc.Request.Token)。
package daemon

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"winbash/internal/ipc"
	"winbash/internal/shell"
)

const createNoWindow = 0x08000000 // CREATE_NO_WINDOW

// DefaultReadTimeout 是一条连接**读完请求**的预算,可由 Serve 覆盖。
//
// 为什么必须有:handle 是同步串行调用的,而 ipc.ReadMessage 会一直等对方把帧发完。
// 一个只连上、一个字节都不发的连接会把整个 daemon 卡在那儿:后续每条命令都排在它后面
// —— 实测要等到 client 自己的超时(默认 300s)才散场,而 daemon 既不能服务也不能自愈。
// 给它一个预算,到点就丢掉这条连接。
//
// 为什么是 5s 这么"紧":这是 loopback,合法请求在连上之后**立刻**就发出来了(微秒级)。
// 实测用 30s 时,一个静默连接仍能让 daemon 沉默半分钟 —— 机制对,但代价还是太高。
// 5s 对任何真实客户端都远远够用,同时把"被一个坏连接拖住"的上限压到可接受。
const DefaultReadTimeout = 5 * time.Second

// Serve 在 endpoint(端口文件路径)上启动服务并阻塞,直到被 --stop 或空闲超时结束。
//
// readTimeout 是单条连接读请求的预算(<=0 用 DefaultReadTimeout)。
//
// 退出时(两种情况都算)必须删除端口文件 —— 否则下次 client 会读到陈旧端口、
// 连上一个不存在的地址。
func Serve(endpoint string, idleTimeout, readTimeout time.Duration) error {
	if readTimeout <= 0 {
		readTimeout = DefaultReadTimeout
	}
	token, err := newToken()
	if err != nil {
		return fmt.Errorf("daemon: 生成 token: %w", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer ln.Close()

	// 端口文件两行:地址 + token。写在一起是因为它们必须同时生效 ——
	// 只发一个地址,就是修之前那个「任意本机进程都能借 daemon 身份执行命令」的洞。
	if err := os.WriteFile(endpoint, []byte(ln.Addr().String()+"\n"+token+"\n"), 0o600); err != nil {
		return fmt.Errorf("daemon: 写端口文件失败: %w", err)
	}
	defer removeIfOwned(endpoint, ln.Addr().String())
	restrictEndpointFile(endpoint)

	tcpLn := ln.(*net.TCPListener)
	sess := shell.NewSession()
	// 会话持有 ② 层的常驻 pwsh 与 ① 层的状态,退出时一定要收干净 ——
	// 否则每重启一次 daemon 就留下一个 pwsh 进程。
	defer func() { _ = sess.Close() }()

	for {
		if idleTimeout > 0 {
			_ = tcpLn.SetDeadline(time.Now().Add(idleTimeout))
		}

		conn, err := tcpLn.Accept()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				return nil // 空闲超时,自行退出
			}
			return err
		}

		quit, herr := handle(conn, sess, token, readTimeout)

		// 优雅关闭:先发 FIN(CloseWrite),把接收方向留到对端读完响应为止。
		// 直接 Close 会让还没读完的 client 收到 RST —— 实测 `winbash --stop`
		// 因此偶发地报 "An existing connection was forcibly closed by the remote host"。
		// client 通常立刻关闭,所以这里的等待是零成本;真等不到也有 250ms 上限。
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
			_ = conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
			_, _ = io.Copy(io.Discard, conn)
		}
		conn.Close()

		// 关键:--stop 直接在 Accept 处返回,绝不回到循环顶部。
		// pwshx 上踩过的 bug 正是"只设了标志、外层循环又重建监听"，导致 --stop 静默失效。
		if quit {
			// 先把端口文件删掉,再退出去。这不是多余的:紧接着来的 client 若读到
			// 还在的端口文件,就会连上一个**正在退出**的监听 —— 实测表现为
			// "An existing connection was forcibly closed by the remote host"。
			// 删掉它,后来的 client 就会去拉一个新 daemon(那才是它想要的)。
			removeIfOwned(endpoint, ln.Addr().String())
			return nil
		}
		if herr != nil {
			// 单个连接的坏帧不该打死整个 daemon。
			continue
		}
	}
}

// handle 处理一个连接，返回是否应当停止 daemon。
func handle(conn net.Conn, sess *shell.Session, token string, readTimeout time.Duration) (quit bool, err error) {
	// 先挂上读预算再去读请求:没有它,一个只连不说活的连接就能把 daemon 卡死,
	// 而 handle 是串行调用的,后面每条命令都排队等它(见 DefaultReadTimeout)。
	if readTimeout > 0 {
		_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
	}
	body, err := ipc.ReadMessage(conn)
	if err != nil {
		return false, err
	}
	// 请求已经读完,撤掉读预算:执行期间我们不从这条连接读东西,留着一个过期的
	// deadline 只会在以后改成"边跑边读"时咬人。
	_ = conn.SetReadDeadline(time.Time{})

	req, err := ipc.DecodeRequest(body)
	if err != nil {
		return false, err
	}

	// 鉴权放在 op 分派**之前**:stop / status 同样是能力,不能让外人用
	// (实测裸 socket 发 {"op":"stop"} 就能把别人的 daemon 打停)。
	if !tokenOK(req.Token, token) {
		return false, writeResponse(conn, ipc.Response{
			OK:   false,
			Auth: true,
			Errors: []string{"daemon: 鉴权失败。token 随端口文件一起生成,只有本用户的 winbash client 读得到它;" +
				"若你确是本用户,重跑一次命令即可(会连上新 daemon)"},
			Exit: 1,
		})
	}

	switch req.Op {
	case "stop":
		return true, writeResponse(conn, ipc.Response{OK: true, Exit: 0})

	case "status":
		out := fmt.Sprintf("pid=%d\ncwd=%s\naddr=%s\n%s\n", os.Getpid(), sess.Dir(), conn.LocalAddr(), sess.EngineStatus())
		return false, writeResponse(conn, ipc.Response{OK: true, Out: []byte(out)})
	}

	// 请求自带超时。client 放弃**不代表** daemon 该继续跑:一条无限循环会一直占着
	// 这条串行的 handle,把后续所有命令饿死(实测第二条命令要等满自己的超时)。
	// 有了这个 ctx,解释器会随取消停下来,handle 才能回到 Accept。
	ctx := context.Background()
	if req.TimeoutMS > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(req.TimeoutMS)*time.Millisecond)
		defer cancel()
	}

	if req.Cwd != "" {
		// client 传来 cwd 时先切过去；失败不致命，继续在旧目录执行。
		sess.Run(ctx, "cd "+shellQuote(req.Cwd), false)
	}

	out, exit, errs := sess.Run(ctx, req.Script, req.JSON)
	return false, writeResponse(conn, ipc.Response{
		OK:     exit == 0,
		Out:    []byte(out),
		Errors: errs,
		Exit:   exit,
	})
}

func writeResponse(conn net.Conn, resp ipc.Response) error {
	b, err := ipc.EncodeResponse(resp)
	if err != nil {
		return err
	}
	return ipc.WriteMessage(conn, b)
}

// newToken 生成一个 128 位随机 token(hex 编码,32 字符)。
//
// 用 crypto/rand 而不是 math/rand:后者可预测,而 token 是这里唯一的鉴权凭据。
func newToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// tokenOK 用常数时间比较,避免把 token 的长度/前缀经由响应时间泄漏出去。
//
// want 为空时一律拒绝:那种 daemon 等于没有鉴权,宁可全拒也不要悄悄放行。
func tokenOK(got, want string) bool {
	if want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// restrictEndpointFile 把端口文件的权限收到只有当前用户。
//
// 为什么必须做:os.WriteFile 的 0o600 在 Windows 上**不起作用**(Go 只处理只读位),
// 实测文件的 ACL 里带着 `CodexSandboxUsers:(I)(M)` 这类条目 —— 低权限的沙箱进程因此
// 读得到地址与 token,那样修了 token 也等于白修。
//
// 用 icacls 而不是 Go 的 API:设 DACL 要走 golang.org/x/sys/windows,而它现在是
// **间接**依赖,提成直接依赖就破坏「唯一直接依赖 mvdan/sh」这条约束。icacls 是系统
// 自带的,代价是它可能因本地化或权限失败 —— 那种情况只警告、不改成败:
// daemon 照样能用、token 照样在,只是这层加固没生效,而使用者有权知道。
func restrictEndpointFile(endpoint string) {
	user := os.Getenv("USERNAME")
	if user == "" {
		return
	}
	// /inheritance:r 去掉从父目录继承来的 ACE(沙箱组的权限正是这么来的),
	// /grant:r 只给当前用户读写。
	cmd := exec.Command("icacls", endpoint, "/inheritance:r", "/grant:r", user+":(R,W)")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "winbash: 警告: 收紧端口文件权限失败(%v): %s\n",
			err, strings.TrimSpace(string(out)))
	}
}

// shellQuote 把一段文本变成安全的单引号 shell 字面量。
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// removeIfOwned 只在端口文件里**还是自己的地址**时才删除它。
//
// 为什么必须这么谨慎:daemon A 正在退出时,client 可能已经拉起了 daemon B,
// 并把同一个端口文件写成了 B 的地址。A 若照删不误,就会把 B 的文件删掉 ——
// 之后每个 client 都看到「没在运行」,而后台还挂着一个完好的 B,
// 谁也找不到它。实测 `--stop` 之后紧接着一条命令就会撞上这个窗口。
func removeIfOwned(endpoint, mine string) {
	b, err := os.ReadFile(endpoint)
	if err != nil {
		return
	}
	// 端口文件是两行(地址 + token),只比第一行。
	first := strings.TrimSpace(strings.SplitN(strings.TrimSpace(string(b)), "\n", 2)[0])
	if first == mine {
		_ = os.Remove(endpoint)
	}
}
