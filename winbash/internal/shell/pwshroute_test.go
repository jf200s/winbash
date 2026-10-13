package shell

import (
	"context"
	"strings"
	"testing"
)

// pwsh 被 ② 层接管("透明覆盖")。
//
// 为什么值得单独钉一条:这不是一个普通命令,而是**覆盖**了一个真实存在的 exe。
// 走错层的后果很隐蔽 —— 输出照样对,只是每次都白付 ~350ms 的启动钱,
// 而"慢"不会让人怀疑路由。所以断言不只看输出,还看路由本身:
// ③ 层只是 exec 一个新进程,永远不会启动常驻引擎;引擎被启动过,就说明走了 ② 层。
func TestPwshIsTakenOverByLayerTwo(t *testing.T) {
	s := NewSession()
	defer s.Close()

	out, exit, errsOut := s.Run(context.Background(), `pwsh -c 'Write-Output routed'`, false)
	if exit != 0 {
		t.Fatalf("exit=%d errs=%v", exit, errsOut)
	}
	if strings.TrimSpace(out) != "routed" {
		t.Fatalf("out=%q(want routed)", out)
	}
	if strings.HasPrefix(s.EngineStatus(), "pwsh=not-started") {
		t.Fatalf("pwsh 没被 ② 层接管(引擎没启动),走了 ③ 层: %s", s.EngineStatus())
	}
}

// 反向保证:③ 层的命令**不惊动**常驻引擎。
// 少了这条,上面那条可以被"反正每条命令都会启动引擎"糊弄过去。
func TestLayerThreeCommandsDoNotStartTheEngine(t *testing.T) {
	s := NewSession()
	defer s.Close()

	_, _, _ = s.Run(context.Background(), "git --version", false)
	if !strings.HasPrefix(s.EngineStatus(), "pwsh=not-started") {
		t.Fatalf("③ 层的命令不该启动 ② 层的引擎: %s", s.EngineStatus())
	}
}
