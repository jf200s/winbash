package shell

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func TestSessionRunsEchoVerbatim(t *testing.T) {
	s := NewSession()
	out, exit, errs := s.Run(context.Background(), "echo hello", false)
	if exit != 0 || strings.TrimSpace(out) != "hello" || len(errs) != 0 {
		t.Fatalf("out=%q exit=%d errs=%v", out, exit, errs)
	}
}

func TestSessionKeepsCwdAcrossRuns(t *testing.T) {
	// Ruling R6：这里原写作 `cd /c/`，但 POSIX 路径形式（/c/ → C:\）的支持属于
	// Task 4 的 ResolvePath，Task 2 还没有。先用 Windows 原生形式验证"cwd 跨 Run 保持"，
	// POSIX 形式的断言留给 Task 4。
	s := NewSession()
	s.Run(context.Background(), "cd C:/", false)
	out, _, _ := s.Run(context.Background(), "pwd", false)
	if !strings.Contains(out, "C:") {
		t.Fatalf("cwd not kept: %q", out)
	}
}

func TestSessionReportsExitCodeNotZeroOnFailure(t *testing.T) {
	s := NewSession()
	_, exit, errs := s.Run(context.Background(), "exit 3", false)
	if exit != 3 {
		t.Fatalf("exit=%d want 3", exit)
	}
	if len(errs) == 0 {
		t.Fatal("expected an error line")
	}
}

func TestBashSemanticsMatchRealBash(t *testing.T) {
	// 语料取自 pure-bash-bible 的字符串/数组/参数展开章节 —— 社区验证过、AI 真会写的写法。
	// 实测 mvdan/sh v3.14.1 在抽样的 40 条里 38 条一致；这里钉住代表性的，防止升级回归。
	//
	// Ruling R7：mapfile 那条原写作 `... > s.txt; mapfile -t A < s.txt`，
	// 会在测试的 cwd（internal/shell/）留下文件、污染仓库。改用 here-string，零副作用。
	cases := []struct{ script, want string }{
		{`v=/a/b/c; echo ${v##*/}`, "c"},                 // basename
		{`v=/a/b/c; echo ${v%/*}`, "/a/b"},               // dirname
		{`v=axbxc; echo ${v//x/-}`, "a-b-c"},             // 全局替换
		{`v=abc; echo ${v^^}`, "ABC"},                    // 大写
		{`v=abcdef; echo ${v:1:3}`, "bcd"},               // 子串
		{`v=abc; echo ${#v}`, "3"},                       // 长度
		{`unset v; echo ${v:-def}`, "def"},               // 默认值
		{`a=(1 2 3); echo ${a[@]}`, "1 2 3"},             // 数组
		{`a=(1 2 3 4); echo ${a[@]:1:2}`, "2 3"},         // 数组切片
		{`mapfile -t A <<< $'a\nb'; echo ${#A[@]}`, "2"}, // mapfile
		{`echo {1..3}`, "1 2 3"},                         // 花括号展开
		{`n=v; v=hi; echo ${!n}`, "hi"},                  // 间接引用
		{`declare -A m; m[k]=v; echo ${m[k]}`, "v"},      // 关联数组
	}
	for _, c := range cases {
		s := NewSession()
		out, exit, _ := s.Run(context.Background(), c.script, false)
		if exit != 0 || strings.TrimSpace(out) != c.want {
			t.Errorf("script %q: got %q (exit %d) want %q", c.script, strings.TrimSpace(out), exit, c.want)
		}
	}
}

// ② 层的接线:名字在 winmgmt.Registry 里 → 用常驻 pwsh 执行 → 固定列序的 TSV。
func TestSessionRoutesWinmgmtCommandsToPwsh(t *testing.T) {
	s := NewSession()
	defer s.Close()

	out, exit, errs := s.Run(context.Background(), "svc list --running --header", false)
	if exit != 0 {
		t.Fatalf("exit=%d errs=%v", exit, errs)
	}
	if firstLine(out) != "Name\tStatus\tStartType" {
		t.Fatalf("header: %q", firstLine(out))
	}
}

// --json 必须一路传到 ② 层,而不是在路上被丢掉(丢了会静默给 TSV)。
func TestSessionJSONFlagReachesLayerTwo(t *testing.T) {
	s := NewSession()
	defer s.Close()

	out, exit, errs := s.Run(context.Background(), "svc status W32Time", true)
	if exit != 0 {
		t.Fatalf("exit=%d errs=%v", exit, errs)
	}
	var items []map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &items); err != nil {
		t.Fatalf("--json did not reach ② 层: %v (%q)", err, out)
	}
	if len(items) != 1 || items[0]["Status"] == nil {
		t.Fatalf("unexpected record: %q", out)
	}
}

// ③ 层:真实 PATH 里的工具必须能跑,而且退出码是它自己的。
func TestSessionRunsExternalCommandFromPath(t *testing.T) {
	s := NewSession()

	out, exit, errs := s.Run(context.Background(), "git --version", false)
	if exit != 0 {
		t.Fatalf("exit=%d errs=%v", exit, errs)
	}
	if !strings.Contains(out, "git version") {
		t.Fatalf("unexpected output: %q", out)
	}

	// 非零退出码要原样带回来,不能被吞成 0。
	_, exit, _ = s.Run(context.Background(), "git definitely-not-a-subcommand-xyz", false)
	if exit == 0 {
		t.Fatal("external command's non-zero exit code was swallowed")
	}
}

// Git Bash 的 usr\bin 只存在于我们构造的搜索路径里(不在进程 PATH 里)——
// 这条钉住「③ 层确实用上了它」,而不是碰巧进程 PATH 里就有 sed。
func TestSessionReachesGitBashToolsNotOnProcessPath(t *testing.T) {
	if gitBashUsrBin() == "" {
		t.Skip("Git Bash 不在标准位置,跳过")
	}
	if strings.Contains(strings.ToLower(os.Getenv("PATH")), `git\usr\bin`) {
		t.Skip("这台机器的进程 PATH 里本来就有 Git 的 usr\\bin,这条断言证明不了什么")
	}

	s := NewSession()
	out, exit, errs := s.Run(context.Background(), "echo hello | sed s/hello/world/", false)
	if exit != 0 {
		t.Fatalf("exit=%d errs=%v", exit, errs)
	}
	if strings.TrimSpace(out) != "world" {
		t.Fatalf("got %q", out)
	}
}

// export 必须对 ③ 层的子进程可见 —— 否则「在 winbash 里导出变量再跑工具」
// 是**静默无效**的,而那正是本项目要消灭的失败形态。
func TestSessionExportedVarReachesExternalCommand(t *testing.T) {
	s := NewSession()
	out, exit, errs := s.Run(context.Background(), `export WINBASH_PROBE=42; pwsh -NoProfile -c '$env:WINBASH_PROBE'`, false)
	if exit != 0 {
		t.Fatalf("exit=%d errs=%v", exit, errs)
	}
	if strings.TrimSpace(out) != "42" {
		t.Fatalf("exported variable did not reach the child process: %q", out)
	}
}
