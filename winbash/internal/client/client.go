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
	addr, err := ensureDaemon(endpoint)
	if err != nil {
		return ipc.Response{}, err
	}

	conn, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err != nil {
		// 端口文件指向的 daemon 已经不在了。清掉陈旧端口再拉一次。
		_ = os.Remove(endpoint)
		if addr, err = ensureDaemon(endpoint); err != nil {
			return ipc.Response{}, err
		}
		if conn, err = net.DialTimeout("tcp", addr, dialTimeout); err != nil {
			return ipc.Response{}, fmt.Errorf("client: 连接 daemon 失败: %w", err)
		}
	}
	defer conn.Close()

	body, err := ipc.Encode(ipc.Request{Script: script, Cwd: cwd, JSON: jsonOut})
	if err != nil {
		return ipc.Response{}, err
	}
	if err := ipc.WriteMessage(conn, body); err != nil {
		return ipc.Response{}, err
	}

	raw, err := ipc.ReadMessage(conn)
	if err != nil {
		return ipc.Response{}, fmt.Errorf("client: 读取响应失败: %w", err)
	}
	return ipc.DecodeResponse(raw)
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
