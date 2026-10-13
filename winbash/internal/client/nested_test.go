package client

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"winbash/internal/shell"
)

// 嵌套时 Run 必须走本地一次性会话,而不是去连 daemon —— 连的话会和正在跑的那条
// 命令互相等待,直到 300s 超时。
//
// 判别方式:走了 daemon 就会写出端口文件;本地执行根本不碰它。
func TestNestedRunDoesNotDialTheDaemon(t *testing.T) {
	t.Setenv(shell.EnvNested, "1")
	endpoint := filepath.Join(t.TempDir(), "nested.port")

	resp, err := Run(endpoint, "echo hi", "", false)
	if err != nil {
		t.Fatalf("嵌套时 Run 不该去连 daemon: %v", err)
	}
	if got := strings.TrimSpace(string(resp.Out)); got != "hi" {
		t.Fatalf("out=%q want hi", got)
	}
	if _, serr := os.Stat(endpoint); serr == nil {
		t.Fatal("端口文件被写出来了,说明它真的去拉 daemon 了")
	}
}

// --stop 在嵌套里必须拒绝:它要停的正是**自己所在的**那个 daemon。
func TestNestedStopRefuses(t *testing.T) {
	t.Setenv(shell.EnvNested, "1")

	err := Stop(filepath.Join(t.TempDir(), "nested.port"))
	if err == nil {
		t.Fatal("嵌套里 Stop 必须报错,不能停掉自己所在的 daemon")
	}
	if !strings.Contains(err.Error(), "嵌套") {
		t.Fatalf("错误该说明原因(提到「嵌套」): %v", err)
	}
}
