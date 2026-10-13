package shell

import (
	"context"
	"strings"
	"testing"
)

func TestSessionRunsEchoVerbatim(t *testing.T) {
	s := NewSession()
	out, exit, errs := s.Run(context.Background(), "echo hello")
	if exit != 0 || strings.TrimSpace(out) != "hello" || len(errs) != 0 {
		t.Fatalf("out=%q exit=%d errs=%v", out, exit, errs)
	}
}

func TestSessionKeepsCwdAcrossRuns(t *testing.T) {
	// Ruling R6：这里原写作 `cd /c/`，但 POSIX 路径形式（/c/ → C:\）的支持属于
	// Task 4 的 ResolvePath，Task 2 还没有。先用 Windows 原生形式验证"cwd 跨 Run 保持"，
	// POSIX 形式的断言留给 Task 4。
	s := NewSession()
	s.Run(context.Background(), "cd C:/")
	out, _, _ := s.Run(context.Background(), "pwd")
	if !strings.Contains(out, "C:") {
		t.Fatalf("cwd not kept: %q", out)
	}
}

func TestSessionReportsExitCodeNotZeroOnFailure(t *testing.T) {
	s := NewSession()
	_, exit, errs := s.Run(context.Background(), "exit 3")
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
		out, exit, _ := s.Run(context.Background(), c.script)
		if exit != 0 || strings.TrimSpace(out) != c.want {
			t.Errorf("script %q: got %q (exit %d) want %q", c.script, strings.TrimSpace(out), exit, c.want)
		}
	}
}
