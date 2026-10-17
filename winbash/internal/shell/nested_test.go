package shell

import (
	"context"
	"strings"
	"testing"
)

// 同一个会话内是严格 FIFO、串行执行的:会话里某条命令正在跑时,若它又去连同一个
// daemon,内层会排在**自己后面** —— 两边互相等到 300s 超时(实测过)。所以 ③ 层派生的
// 子进程必须带一个标记,让嵌套调用的 winbash 认出自己在哪、不去连 daemon
// (见 client.Run 的嵌套分支)。
func TestExternalCommandsCarryTheNestedMarker(t *testing.T) {
	if gitBashUsrBin() == "" {
		t.Skip("Git Bash 不在标准位置,跳过")
	}
	s := NewSession()
	out, exit, errs := s.Run(context.Background(), "env", false)
	if exit != 0 {
		t.Fatalf("exit=%d errs=%v", exit, errs)
	}
	if !strings.Contains(out, EnvNested+"=") {
		t.Fatalf("③ 层子进程必须带 %s 标记,否则嵌套调用会挂到超时:\n%s", EnvNested, out)
	}
}

// 反向:会话绝不能继承外面那个标记。
//
// daemon 是 client 用 os/exec 拉起来的,而 NewSession 从**进程环境**建 ① 层 ——
// 不清掉的话标记会经 daemon 漏进后来每一条命令的子进程,于是「嵌套」变成常态:
// 明明没嵌套的调用也走本地一次性会话,白白丢掉常驻带来的速度。
func TestSessionDoesNotInheritTheNestedMarker(t *testing.T) {
	t.Setenv(EnvNested, "1")

	s := NewSession()
	out, exit, errs := s.Run(context.Background(), "echo [${"+EnvNested+":-undef}]", false)
	if exit != 0 {
		t.Fatalf("exit=%d errs=%v", exit, errs)
	}
	if strings.TrimSpace(out) != "[undef]" {
		t.Fatalf("继承来的标记没有被清掉: %q", out)
	}
}
