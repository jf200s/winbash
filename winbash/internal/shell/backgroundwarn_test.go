package shell

// 2026-10-31 会话语义决议的回归:脚本含后台任务(`cmd &`)时,RunArgs 在响应
// errors 里追加**一行**指路 `--detach` 的警告;`&` 的行为本身不变(后台 job 仍随
// 请求结束被取消,由 cmd/winbash/e2e_test.go 的 TestBackgroundJobDiesWithTheRequest
// 钉住),退出码也不变。检测并进 rejectUnsupportedSyntax 的单遍 Walk(收集性信息,
// 不剪枝、不报错)。
//
// AST 形态经探针实测确认:`cmd &` 是 *syntax.Stmt 的 Background 字段,
// `nohup cmd &` / `( … ) &` / `a | b &` / `a & b` / 嵌套 `if …; then x & fi`
// 全部命中;`a && b` 是 AndExpr,不置 Background。

import (
	"context"
	"strings"
	"testing"
)

const wantWarnSubstr = "--detach"

func TestBackgroundJobWarningFiresOncePerScript(t *testing.T) {
	// 每种 `&` 形态都要命中(探针实测过的全部落点),且整个脚本只警告一次。
	for _, script := range []string{
		"nohup cmd &",
		"( echo BG ) & echo NOW",
		"echo a | cat & echo done",
		"echo a & echo b",
		"echo a & echo b & echo c",
		"if true; then x & fi",
	} {
		s := NewSession()
		out, exit, errsOut := s.Run(context.Background(), script, false)
		if exit != 0 {
			t.Errorf("script %q: 警告不能改退出码, got %d errs=%q", script, exit, errsOut)
		}
		joined := strings.Join(errsOut, "\n")
		if n := strings.Count(joined, wantWarnSubstr); n != 1 {
			t.Errorf("script %q: 警告应恰一次,got %d 次 errs=%q", script, n, joined)
		}
		if !strings.Contains(joined, "随本次请求结束被取消") {
			t.Errorf("script %q: 警告应说明现状(随请求结束取消), got %q", script, joined)
		}
		if strings.Contains(out, wantWarnSubstr) {
			t.Errorf("script %q: 警告走 errors 通道,不该混进 stdout, got %q", script, out)
		}
	}
}

func TestBackgroundJobWarningAbsentWithoutAmpersand(t *testing.T) {
	for _, script := range []string{
		"echo hello",
		"echo a && echo b",  // AndExpr 不是后台任务
		"echo a || echo b",  // OrExpr 同理
		"echo 'a&b'", // 引号里的字面 & 不算后台任务(mvdan 解析成参数的一部分)
	} {
		s := NewSession()
		_, exit, errsOut := s.Run(context.Background(), script, false)
		if exit != 0 {
			t.Errorf("script %q: exit=%d errs=%q", script, exit, errsOut)
		}
		if joined := strings.Join(errsOut, "\n"); strings.Contains(joined, wantWarnSubstr) {
			t.Errorf("script %q: 没有后台任务不该有警告, got %q", script, joined)
		}
	}
}

func TestBackgroundJobWarningKeepsExitCodeAndStderr(t *testing.T) {
	s := NewSession()
	// 退出码与 stderr 内容原样保留,警告追加在其后 —— 警告不是"错误替代品"。
	out, exit, errsOut := s.Run(context.Background(), "echo BG > /dev/null & exit 3", false)
	_ = out
	if exit != 3 {
		t.Errorf("exit=%d want 3(警告不得改写退出码)", exit)
	}
	joined := strings.Join(errsOut, "\n")
	if strings.Count(joined, wantWarnSubstr) != 1 {
		t.Errorf("带错误的脚本也该恰一条警告, got %q", joined)
	}
}

func TestBackgroundJobWarningDoesNotBreakControlGroup(t *testing.T) {
	// 对照组(设计文档 ③ 的 c):脚本存活期间后台 job 正常 —— 警告加入后这条
	// 真实用法必须原样可用。
	s := NewSession()
	out, exit, errsOut := s.Run(context.Background(), "( echo LATE ) & echo NOW; sleep 2", false)
	if exit != 0 {
		t.Fatalf("exit=%d errs=%q", exit, errsOut)
	}
	if !strings.Contains(out, "NOW") || !strings.Contains(out, "LATE") {
		t.Errorf("对照组应同时打出 NOW 与 LATE, got %q", out)
	}
	if strings.Count(strings.Join(errsOut, "\n"), wantWarnSubstr) != 1 {
		t.Errorf("对照组也该看到恰一条警告, errs=%q", errsOut)
	}
}
