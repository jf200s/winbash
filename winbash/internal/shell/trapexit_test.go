// 持久会话 EXIT trap 的触发时机:**每条命令收尾都触发一次**,不是只在会话真正退出时。
//
// 这是**现状钉子**(2026-10-31),不是对理想语义的背书。实测(同一 Session 先后三次 Run):
//
//	Run1 `trap 'echo BYE' EXIT; echo armed` → armed + BYE
//	Run2 `echo second`                     → second + BYE
//	Run3 `echo third`                      → third + BYE
//
// 交互 bash 里 EXIT trap 只在真正退出 shell 时触发一次;这里条条命令收尾各触发一次,
// 是持久会话与一次性脚本的语义分叉。**根因**:`RunArgs` 每次 Run 都把脚本解析成新的
// `*syntax.File` 交给持久 runner( shell.go 的 RunArgs:`s.runner.Run(ctx, prog)`),
// 而 mvdan/sh 对 `*syntax.File` 的每次 Run 都触发 EXIT trap
// (third_party/mvdan-sh/interp/api.go:1046-1049,注释原文
// 「Running an entire file implies an exit」)—— 持久 runner + 会话复用,于是每条命令一次。
//
// 「每条命令触发一次」还是「只在会话结束时触发一次」是会话语义级取舍,待
// docs/superpowers 的会话语义专项设计裁决;在那之前谁改了触发时机(不管改成
// 只在退出时触发,还是反过来),这条测试先红。另见 internal/daemon/fresh_test.go
// 的 TestFreshDoesNotInheritTrap(端到端侧:trap 跨命令持久 + --fresh 不继承)。
//
// 断言用 Contains 不逐字节相等:trap 输出与脚本输出的相对顺序、空行细节都来自
// interp 的收尾路径,不是这条要钉的东西;要钉的只有「这一次 Run 的输出里有 BYE」。
package shell

import (
	"context"
	"strings"
	"testing"
)

func TestExitTrapFiresOnEveryPersistentCommand(t *testing.T) {
	s := NewSession()
	defer s.Close()

	runs := []struct{ script, own string }{
		{"trap 'echo BYE' EXIT; echo armed", "armed"},
		{"echo second", "second"},
		{"echo third", "third"},
	}
	for i, r := range runs {
		out, exit, errs := s.Run(context.Background(), r.script, false)
		if exit != 0 {
			t.Fatalf("第 %d 条命令 exit=%d errs=%v", i+1, exit, errs)
		}
		if !strings.Contains(out, r.own) {
			t.Fatalf("第 %d 条命令(%q)自身输出丢失,trap 断言无从谈起:out=%q", i+1, r.script, out)
		}
		if !strings.Contains(out, "BYE") {
			t.Fatalf("第 %d 条命令(%q)的输出没有 BYE:现状是每条命令收尾都触发 EXIT trap"+
				"(现状钉子,语义取舍见会话语义专项设计);out=%q errs=%v", i+1, r.script, out, errs)
		}
	}
}
