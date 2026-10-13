package shell

import (
	"context"
	"strings"
	"testing"
)

// `command -v` 是 AI 做"这个命令有没有"的标准写法,而 mvdan/sh 的原逻辑
// 只查**进程 PATH** 与它自己的内建表 —— 于是它对 winbash 自己的内建(`ls`)
// 和只在 Git Bash usr\bin 里的工具(`sed`)都会说"没有"。
// 这一误判是**静默**的:`command -v ls >/dev/null && …` 会安静地不执行。
//
// 修法:CallHandler 把这种必然答错的调用改写成我们的 `which`(它认识四层)。
func TestCommandVDiscoversWinbashBuiltinsAndGitBashTools(t *testing.T) {
	cases := []struct{ script, want string }{
		{`command -v ls`, "ls"},
		{`command -v cat`, "cat"},
		{`command -v which`, "which"},
		{`command -v svc`, "svc"}, // ② 层
		{`command -v ls >/dev/null && echo found`, "found"},
		{`command -v sed`, "sed.exe"}, // ③ 层,只在 Git Bash 的 usr\bin 里
	}
	for _, c := range cases {
		s := NewSession()
		out, exit, errsOut := s.Run(context.Background(), c.script, false)
		if exit != 0 {
			t.Errorf("script %q: exit=%d errs=%v", c.script, exit, errsOut)
			continue
		}
		if got := strings.TrimSpace(out); !strings.Contains(got, c.want) {
			t.Errorf("script %q: got %q want to contain %q", c.script, got, c.want)
		}
	}
}

// 找不到时要非零(存在性判断的两个分支都得对),而且不能把 stdout 弄脏。
func TestCommandVStaysNonZeroForMissingCommand(t *testing.T) {
	s := NewSession()
	out, exit, errsOut := s.Run(context.Background(),
		`command -v definitely-not-installed-xyz >/dev/null 2>&1 && echo found || echo missing`, false)
	if exit != 0 {
		t.Fatalf("exit=%d errs=%v", exit, errsOut)
	}
	if strings.TrimSpace(out) != "missing" {
		t.Fatalf("got %q", out)
	}
}

// 与之相邻的用法不能被误伤:`command -v` 在**函数**上的原逻辑是对的(mvdan/sh 知道函数),
// 而我们只在「①/② 层 or 只有我们找得到」时改写,所以函数这条路径不受影响。
func TestCommandVStillResolvesShellFunctions(t *testing.T) {
	s := NewSession()
	out, exit, errsOut := s.Run(context.Background(),
		`f() { echo hi; }; command -v f >/dev/null && f`, false)
	if exit != 0 {
		t.Fatalf("exit=%d errs=%v", exit, errsOut)
	}
	if strings.TrimSpace(out) != "hi" {
		t.Fatalf("got %q", out)
	}
}

// ① 层的 `which` 本身:三个层各给什么,以及找不到时的行为。
func TestWhichCommandReportsEachLayer(t *testing.T) {
	s := NewSession()
	out, exit, errsOut := s.Run(context.Background(), `which ls svc git`, false)
	if exit != 0 {
		t.Fatalf("exit=%d errs=%v", exit, errsOut)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %q", out)
	}
	// ① 层与 ② 层报名字(bash 的 command -v 对内建就是这样);③ 层报绝对路径。
	if strings.TrimSpace(lines[0]) != "ls" || strings.TrimSpace(lines[1]) != "svc" {
		t.Fatalf("①/② 层应当报名字: %q", out)
	}
	if !strings.Contains(lines[2], "git") {
		t.Fatalf("③ 层应当报路径: %q", lines[2])
	}
}

func TestWhichCommandIsNonZeroWhenMissing(t *testing.T) {
	s := NewSession()
	out, exit, errsOut := s.Run(context.Background(), `which definitely-not-installed-xyz`, false)
	if exit == 0 {
		t.Fatal("expected non-zero exit")
	}
	if out != "" {
		t.Fatalf("nothing should reach stdout, got %q", out)
	}
	if !strings.Contains(strings.Join(errsOut, "\n"), "找不到") {
		t.Fatalf("expected an explicit message, got %v", errsOut)
	}
}
