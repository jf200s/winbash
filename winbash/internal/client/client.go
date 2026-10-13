// Package client 是 winbash 的轻客户端:连上常驻 daemon,发一条脚本,取回结果。
package client

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"winbash/internal/ipc"
)

const (
	dialTimeout    = 3 * time.Second
	startupBudget  = 20 * time.Second
	pollInterval   = 20 * time.Millisecond
	createNoWindow = 0x08000000 // CREATE_NO_WINDOW
)

// Run 连上 endpoint 指向的 daemon 执行 script。
//
// daemon 没起来时先拉起它;若 daemon 刚死而端口文件还在,清掉陈旧文件后重拉一次
// (Review Focus 第 5 条:daemon 死后 client 必须自行恢复,不能挂起等待)。
func Run(endpoint, script, cwd string, jsonOut bool) (ipc.Response, error) {
	return RunTimeout(endpoint, script, cwd, jsonOut, 0)
}

// RunTimeout 同 Run,但给整个往返设一个上限(timeout<=0 表示不设限)。
func RunTimeout(endpoint, script, cwd string, jsonOut bool, timeout time.Duration) (ipc.Response, error) {
	return oneShot(endpoint, ipc.Request{Script: script, Cwd: cwd, JSON: jsonOut}, false, timeout)
}

// Stop 让 daemon 退出。
//
// mustExist 的语义在这里很要紧:daemon 没在跑时**不把它拉起来** ——
// 「停一个没在跑的 daemon」不该先把 daemon 启动、再把它停掉。
func Stop(endpoint string) error {
	resp, err := oneShot(endpoint, ipc.Request{Op: "stop"}, true, 0)
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("client: daemon 拒绝停止: %s", strings.Join(resp.Errors, "; "))
	}
	return nil
}

// Status 取 daemon 的状态(pid / cwd / ② 层引擎)。同样不拉起 daemon。
func Status(endpoint string) (string, error) {
	resp, err := oneShot(endpoint, ipc.Request{Op: "status"}, true, 0)
	if err != nil {
		return "", err
	}
	return string(resp.Out), nil
}

// oneShot 发一个请求收一个响应。
//
// mustExist=true:daemon 不在就报错(给 --stop/--status 用);
// false:daemon 不在就把它拉起来(给执行脚本用)。
func oneShot(endpoint string, req ipc.Request, mustExist bool, timeout time.Duration) (ipc.Response, error) {
	addr, err := dialAddr(endpoint, mustExist)
	if err != nil {
		return ipc.Response{}, err
	}

	conn, err := dial(endpoint, addr, mustExist)
	if err != nil {
		return ipc.Response{}, err
	}

	resp, xerr := exchange(conn, req, timeout)
	conn.Close()
	if xerr == nil {
		return resp, nil
	}

	// 半路上掉了:daemon 可能刚被 `--stop` 停掉(它还在退出的路上),或者被杀掉了。
	// 清掉端口文件、重拉一个、原样重试一次 —— 这正是 Review Focus 第 5 条要的行为:
	// 自行恢复,而不是把 WSAECONNRESET 丢给使用者。
	//
	// 只重试一次是有意的:重试可能让脚本执行两遍,而**更常见**的情况是请求根本没送达
	// (连接被重置)。一次是「从偶发竞态里恢复」和「不在真故障上打转」之间的中点。
	// mustExist(--stop/--status)不重试:那两个不该把 daemon 拉起来。
	if mustExist {
		return ipc.Response{}, xerr
	}
	forgetStaleEndpoint(endpoint, addr)
	addr2, aerr := ensureDaemon(endpoint)
	if aerr != nil {
		return ipc.Response{}, xerr
	}
	conn2, derr := dial(endpoint, addr2, false)
	if derr != nil {
		return ipc.Response{}, xerr
	}
	defer conn2.Close()
	return exchange(conn2, req, timeout)
}

// dial 建立到 addr 的连接;mustExist 时连不上就直接报"没在运行"。
func dial(endpoint, addr string, mustExist bool) (net.Conn, error) {
	conn, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err == nil {
		return conn, nil
	}
	if mustExist {
		return nil, fmt.Errorf("client: daemon 没在运行(端口文件 %s 指向的地址连不上)", endpoint)
	}
	// 端口文件指向的 daemon 已经不在了。清掉陈旧端口再拉一次。
	forgetStaleEndpoint(endpoint, addr)
	addr2, aerr := ensureDaemon(endpoint)
	if aerr != nil {
		return nil, aerr
	}
	conn, err = net.DialTimeout("tcp", addr2, dialTimeout)
	if err != nil {
		return nil, fmt.Errorf("client: 连接 daemon 失败: %w", err)
	}
	return conn, nil
}

// forgetStaleEndpoint 只在端口文件**仍然指向 addr** 时才删它。
//
// 与 daemon 侧的 removeIfOwned 是同一个理由:这个窗口里可能有新 daemon 已经把
// 文件改写成自己的地址了,照删不误会把新 daemon 变成"谁也找不到"的孤儿。
func forgetStaleEndpoint(endpoint, addr string) {
	b, err := os.ReadFile(endpoint)
	if err != nil {
		return
	}
	if strings.TrimSpace(string(b)) == addr {
		_ = os.Remove(endpoint)
	}
}

// exchange 在一条连接上完成一次"写请求 + 读响应"。
func exchange(conn net.Conn, req ipc.Request, timeout time.Duration) (ipc.Response, error) {
	if timeout > 0 {
		// 给整个往返设上限:daemon 卡在一条长命令上时,使用者要的是"明确超时",
		// 不是"无限等下去"(后者看起来像挂死)。
		_ = conn.SetDeadline(time.Now().Add(timeout))
	}

	body, err := ipc.Encode(req)
	if err != nil {
		return ipc.Response{}, err
	}
	if err := ipc.WriteMessage(conn, body); err != nil {
		return ipc.Response{}, fmt.Errorf("client: 发送请求失败: %w", err)
	}

	raw, err := ipc.ReadMessage(conn)
	if err != nil {
		return ipc.Response{}, fmt.Errorf("client: 读取响应失败: %w", err)
	}
	return ipc.DecodeResponse(raw)
}

// dialAddr 决定往哪个地址连。mustExist 时只读端口文件,绝不拉起新 daemon。
func dialAddr(endpoint string, mustExist bool) (string, error) {
	if !mustExist {
		return ensureDaemon(endpoint)
	}
	addr, err := readEndpoint(endpoint)
	if err != nil {
		return "", fmt.Errorf("client: daemon 没在运行(端口文件 %s 不存在)", endpoint)
	}
	return addr, nil
}

// ensureDaemon 返回可拨号地址;daemon 不在就先把它拉起来。
func ensureDaemon(endpoint string) (string, error) {
	if addr, err := readEndpoint(endpoint); err == nil {
		return addr, nil
	}
	if err := spawnDaemon(endpoint); err != nil {
		return "", fmt.Errorf("client: 启动 daemon 失败: %w", err)
	}
	deadline := time.Now().Add(startupBudget)
	for time.Now().Before(deadline) {
		if addr, err := readEndpoint(endpoint); err == nil {
			return addr, nil
		}
		time.Sleep(pollInterval)
	}
	return "", fmt.Errorf("client: daemon 在 %s 内没有起来", startupBudget)
}

func readEndpoint(endpoint string) (string, error) {
	b, err := os.ReadFile(endpoint)
	if err != nil {
		return "", err
	}
	addr := strings.TrimSpace(string(b))
	if addr == "" {
		return "", fmt.Errorf("client: 端口文件 %s 是空的", endpoint)
	}
	return addr, nil
}

// spawnDaemon 以 --serve 重新启动自己。隐藏窗口,不打扰使用者。
func spawnDaemon(endpoint string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "--serve", "--endpoint", endpoint)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow,
	}
	return cmd.Start()
}
