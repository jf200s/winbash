package shell

import (
	"context"
	"strings"
	"testing"

	"winbash/internal/errs"
)

// 计划里的 5 个用例来自 plan Task 13;其中 2 条的断言按下面的理由改过,
// 另外补了 2 条(见注释)。

func TestUnknownCommandExits127WithThreeLineError(t *testing.T) {
	s := NewSession()
	_, exit, errsOut := s.Run(context.Background(), "definitely-not-a-command-xyz", false)
	if exit != errs.ExitNotFound {
		t.Fatalf("exit=%d want %d", exit, errs.ExitNotFound)
	}
	if len(errsOut) == 0 {
		t.Fatal("expected an error")
	}
	// spec §7:错误固定三行 —— 命令 / 问题 / 怎么办。
	joined := strings.Join(errsOut, "\n")
	if n := strings.Count(joined, "\n"); n != 2 {
		t.Fatalf("want 3 lines, got %d: %q", n+1, joined)
	}
	if !strings.Contains(joined, "definitely-not-a-command-xyz") {
		t.Fatalf("error must name the command: %q", joined)
	}
}

func TestBashCommandIsTranslatedToGitBashNotSilentlyWSL(t *testing.T) {
	// 本机 'bash' 指向损坏的 WSL relay；必须给出可操作提示
	problem, remedy, ok := errs.TranslateKnownTrap("bash", []string{"script.sh"}, "execvpe(/bin/bash) failed")
	if !ok {
		t.Fatal("trap not recognized")
	}
	if !strings.Contains(remedy, "Git") {
		t.Fatalf("remedy must point at Git Bash: %q", remedy)
	}
	if problem == "" {
		t.Fatal("problem line must not be empty")
	}
}

func TestRegSlashVIsRecognizedAsMSYSTrap(t *testing.T) {
	// 这个坑在 winbash 里本不该发生；若发生，必须解释清楚
	_, remedy, ok := errs.TranslateKnownTrap("reg", []string{"query", "x", "/v", "y"}, "Invalid syntax")
	if !ok {
		t.Fatal("trap not recognized")
	}
	if !strings.Contains(remedy, "reg get") {
		t.Fatalf("remedy should point at the winbash form: %q", remedy)
	}
}

func TestPrintfDashVIsRejectedNotSilentlyMangled(t *testing.T) {
	// 实测缺口：mvdan/sh 不支持 printf -v，会把 `-v` 当普通参数静默输出
	s := NewSession()
	out, exit, _ := s.Run(context.Background(), `printf -v x '%s' a; echo $x`, false)
	if strings.TrimSpace(out) == "-v" {
		t.Fatalf("printf -v silently degraded to %q instead of erroring", out)
	}
	if exit == 0 {
		t.Errorf("expected non-zero exit for unsupported printf -v")
	}
}

func TestProcessSubstitutionIsRejectedBeforeRunning(t *testing.T) {
	// 实测缺口：Windows 上 mvdan/sh 会把 TODO 文本打到 stdout
	//
	// 改动一处:plan 原断言要求这句说明出现在 **stdout** 里。本项目一贯的约定是
	// stdout 只放结果、错误走 stderr(spec §7 的三行式也是错误行),所以断言落在 errs 上,
	// 并额外要求 stdout 为空 —— 这样「绝不静默」的意图被钉得更死。
	s := NewSession()
	out, exit, errsOut := s.Run(context.Background(), `diff <(echo a) <(echo a)`, false)
	if strings.Contains(out, "TODO") {
		t.Fatalf("process substitution leaked a TODO into stdout: %q", out)
	}
	if out != "" {
		t.Fatalf("nothing should reach stdout when the command is rejected: %q", out)
	}
	joined := strings.Join(errsOut, "\n")
	if !strings.Contains(joined, "不支持") && !strings.Contains(joined, "not supported") {
		t.Fatalf("expected an explicit unsupported message, got %q (exit %d)", joined, exit)
	}
	if exit == 0 {
		t.Fatal("expected a non-zero exit")
	}
}

// 集成:翻译表必须真的接在 ③ 层上,而不只是一个能通过单元测试的纯函数。
// 实测本机 `bash --version` 会打出 WSL relay 的 execvpe 错误 —— 那条信息本身
// 不能让使用者知道「你要的多半是 Git Bash」,所以必须补一句。
func TestBashWSLTrapIsTranslatedInARealRun(t *testing.T) {
	s := NewSession()
	out, exit, errsOut := s.Run(context.Background(), "bash --version", false)
	joined := strings.Join(errsOut, "\n") + out

	if strings.Contains(joined, "execvpe") && !strings.Contains(joined, "Git") {
		t.Fatalf("the WSL-relay trap was shown verbatim without the actionable translation:\n%s", joined)
	}
	// 这条命令在本机必然失败(WSL relay 已损坏);换台没装 WSL 的机器可能不同,
	// 所以只断言「没有 Git 提示 + 有 execvpe 痕迹」这一种情况下必须给提示。
	if exit == 0 && strings.Contains(joined, "execvpe") {
		t.Fatal("bash succeeded yet reported an execvpe failure?")
	}
}

// POSIX 路径给外部工具用是这个项目里最常见的翻车之一(git/curl 只认 Windows 形式)。
func TestPosixPathTrapIsRecognized(t *testing.T) {
	problem, remedy, ok := errs.TranslateKnownTrap("git", []string{"-C", "/c/Users/x", "status"},
		"fatal: cannot change to '/c/Users/x': No such file or directory")
	if !ok {
		t.Fatal("trap not recognized")
	}
	if !strings.Contains(remedy, `C:\`) && !strings.Contains(remedy, "两种") {
		t.Fatalf("remedy should explain the two path forms: %q", remedy)
	}
	if problem == "" {
		t.Fatal("problem line must not be empty")
	}
}
