package shell

import (
	"context"
	"strings"
	"testing"
)

// trap 信号的拦截边界。
//
// 为什么要这一层(而不是让 mvdan/sh 自己报错):上游只实现了 ERR 与 EXIT 两个信号,
// 其余的通通走 default 分支 failf —— 但它只把一句 `invalid signal specification`
// 写到 **stderr**,而脚本的退出码仍是 0、hook 一个字节都没生效:
//
//	trap 'echo d' DEBUG; echo x
//	→ stderr: trap: DEBUG: invalid signal specification
//	→ stdout: x        (hook 静默不执行)
//	→ 整体 exit 0
//
// `trap '' INT`(想忽略 Ctrl+C)这类写法受害更深:使用者以为信号被屏蔽了,其实没有。
// trapGuard 把它变成响亮失败 —— 这是 README「已知差异与限制」里那条的来源。

// runScriptForTrap 跑一条脚本,返回 stdout、退出码、stderr 文本。
func runScriptForTrap(t *testing.T, script string) (string, int, string) {
	t.Helper()
	s := NewSession()
	defer s.Close()
	out, exit, errs := s.Run(context.Background(), script, false)
	return out, exit, strings.Join(errs, " / ")
}

// 该拦的:任何不是 ERR / EXIT 的信号,出现在**信号位置**。
//
// 一条都不能漏 —— 漏了就是"脚本以为自己装了 hook、其实没有",而且退出码还告诉它成功。
func TestUnsupportedTrapSignalsAreRejected(t *testing.T) {
	cases := []struct {
		name   string
		script string
		signal string
	}{
		{"DEBUG 带 callback", `trap 'printf d' DEBUG; echo x`, "DEBUG"},
		{"RETURN 带 callback", `f() { trap 'printf r' RETURN; printf body; }; f; echo`, "RETURN"},
		{"DEBUG 空 callback(只清 hook)", `trap '' DEBUG; echo x`, "DEBUG"},
		{"DEBUG 用 - 重置", `trap - DEBUG; echo x`, "DEBUG"},
		{"DEBUG 与 EXIT 混写", `trap 'printf d' DEBUG EXIT; echo x`, "DEBUG"},
		{"RETURN 与 USR1 混写", `f() { trap 'printf r' RETURN USR1; }; f; echo`, "RETURN"},
		// 下面三个是"以为能靠信号做保护"的高危写法:一个字节都没生效。
		{"INT(想忽略 Ctrl+C)", `trap '' INT; echo x`, "INT"},
		{"TERM", `trap 'printf t' TERM; echo x`, "TERM"},
		{"USR1", `trap 'true' USR1; echo listed`, "USR1"},
		{"HUP", `trap 'printf h' HUP; echo x`, "HUP"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, exit, errText := runScriptForTrap(t, c.script)

			// 必须是**响亮**失败:非零退出,而不是"照跑、exit 0"。
			if exit == 0 {
				t.Fatalf("trap %s 没有被拦下(exit=0,out=%q);"+
					"这正是要消灭的静默形态:hook 没生效,调用方却以为成功", c.signal, out)
			}
			// 三行式里必须点名是哪个信号,否则使用者不知道改哪一句。
			if !strings.Contains(errText, "trap "+c.signal) {
				t.Errorf("错误信息没点名 trap %s: %q", c.signal, errText)
			}
			// 并且要给替代写法 —— 只报"不支持"等于把人留在原地。
			if !strings.Contains(errText, "怎么办") {
				t.Errorf("错误信息缺「怎么办」替代写法: %q", errText)
			}
		})
	}
}

// 不该拦的:误拦会破坏本来能正常工作的脚本,比漏拦更糟。
//
// 关键的两处:ERR / EXIT 是上游真正支持的,必须照常工作;
// callback **文本**里出现的信号名(如 `trap 'echo DEBUG' ...`)不是信号位置。
func TestSupportedTrapSignalsStillWork(t *testing.T) {
	cases := []struct {
		name string
		want string
		code int
	}{
		{"EXIT 照常触发", "hi\nbye", 0},
		{"ERR 照常触发", "err-hook exit=1\n", 0},
		{"EXIT 与 ERR 混写", "e\n", 0},
		// callback 文本里含信号名,但信号位置是 EXIT —— 不该误拦。
		// (EXIT hook 的 printf 不带换行,所以紧跟在 ok 的换行之后。)
		{"callback 文本含 DEBUG 不误拦", "ok\nDEBUG-hit", 0},
		{"callback 文本含 RETURN 不误拦", "ok\nRETURN-hit", 0},
		{"callback 文本含 INT 不误拦", "ok\nINT-hit", 0},
		{"非 trap 命令提到 DEBUG 不误拦", "DEBUG=x\n", 0},
		{"trap 无参数不拦(只是少个查询功能)", "listed\n", 0},
	}
	scripts := []string{
		`trap 'printf bye' EXIT; echo hi`,
		`trap 'printf "err-hook "' ERR; false; printf 'exit=%s\n' "$?"`,
		`trap 'echo e' ERR; trap 'echo e' EXIT; true`,
		`trap 'printf DEBUG-hit' EXIT; echo ok`,
		`trap 'printf RETURN-hit' EXIT; echo ok`,
		`trap 'printf INT-hit' EXIT; echo ok`,
		`printf 'DEBUG=x\n'`,
		`trap >/dev/null 2>&1; echo listed`,
	}
	if len(scripts) != len(cases) {
		t.Fatalf("用例表长度不一致: %d vs %d", len(cases), len(scripts))
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, exit, errText := runScriptForTrap(t, scripts[i])
			if exit != c.code {
				t.Errorf("exit=%d want %d(out=%q err=%q)", exit, c.code, out, errText)
			}
			if out != c.want {
				t.Errorf("out=%q want %q(err=%q)", out, c.want, errText)
			}
		})
	}
}

// trapGuard 的解析边界单独钉一遍 —— 上面两个测试走整条 Run 链路,
// 这条直接对着"信号列表怎么切"这个唯一的判断点。
func TestTrapGuardSignalPosition(t *testing.T) {
	cases := []struct {
		args    []string
		blocked bool
		why     string
	}{
		{[]string{"trap", "DEBUG"}, true, "两参数:第二个就是信号"},
		{[]string{"trap", "echo DEBUG", "EXIT"}, false, "DEBUG 是 callback 文本"},
		{[]string{"trap", "", "INT"}, true, "空 callback + INT 信号"},
		{[]string{"trap", "-", "INT"}, true, "`-` 是 arg 位置的重置,INT 仍是信号"},
		{[]string{"trap", "x", "USR1", "ERR"}, true, "多信号里只要有一个不支持就拦"},
		{[]string{"trap", "-l"}, false, "-l 是 flag,上游自己报 NOT IMPLEMENTED"},
		{[]string{"trap", "-p"}, false, "-p 同上"},
		{[]string{"trap"}, false, "无参数:列出,放行"},
		{[]string{"printf", "DEBUG"}, false, "不是 trap 命令"},
		{[]string{"trap", "x", "EXIT"}, false, "EXIT 上游支持"},
		{[]string{"trap", "x", "ERR"}, false, "ERR 上游支持"},
		{[]string{"trap", "x", "ERR", "EXIT"}, false, "两个都支持"},
	}
	for _, c := range cases {
		_, err := trapGuard(c.args)
		if blocked := err != nil; blocked != c.blocked {
			t.Errorf("trapGuard(%q) blocked=%v want %v(%s)", c.args, blocked, c.blocked, c.why)
		}
	}
}
