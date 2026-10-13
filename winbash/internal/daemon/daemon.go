// Package daemon 是 winbash 的常驻服务端。
//
// 传输层用 TCP loopback 而不是命名管道：Go 标准库没有 CreateNamedPipe 封装，
// 而项目只允许一个直接依赖(mvdan/sh)。127.0.0.1 仅本机可达，对本机开发工具可接受。
package daemon

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"winbash/internal/ipc"
	"winbash/internal/shell"
)

// Serve 在 endpoint(端口文件路径)上启动服务并阻塞,直到被 --stop 或空闲超时结束。
//
// 退出时(两种情况都算)必须删除端口文件 —— 否则下次 client 会读到陈旧端口、
// 连上一个不存在的地址。
func Serve(endpoint string, idleTimeout time.Duration) error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer ln.Close()

	if err := os.WriteFile(endpoint, []byte(ln.Addr().String()), 0o600); err != nil {
		return fmt.Errorf("daemon: 写端口文件失败: %w", err)
	}
	defer os.Remove(endpoint)

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

		quit, herr := handle(conn, sess)
		conn.Close()

		// 关键:--stop 直接在 Accept 处返回,绝不回到循环顶部。
		// pwshx 上踩过的 bug 正是"只设了标志、外层循环又重建监听"，导致 --stop 静默失效。
		if quit {
			return nil
		}
		if herr != nil {
			// 单个连接的坏帧不该打死整个 daemon。
			continue
		}
	}
}

// handle 处理一个连接，返回是否应当停止 daemon。
func handle(conn net.Conn, sess *shell.Session) (quit bool, err error) {
	body, err := ipc.ReadMessage(conn)
	if err != nil {
		return false, err
	}
	req, err := ipc.DecodeRequest(body)
	if err != nil {
		return false, err
	}

	switch req.Op {
	case "stop":
		return true, writeResponse(conn, ipc.Response{OK: true, Exit: 0})

	case "status":
		out := fmt.Sprintf("pid=%d\ncwd=%s\n", os.Getpid(), sess.Dir())
		return false, writeResponse(conn, ipc.Response{OK: true, Out: []byte(out)})
	}

	ctx := context.Background()
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

// shellQuote 把一段文本变成安全的单引号 shell 字面量。
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
