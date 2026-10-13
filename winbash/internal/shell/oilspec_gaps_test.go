package shell

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 这一组测试来自"用公开测试集跑一遍"的结果:oils-for-unix 的 spec/ 用例
// (筛出 105 个 bash 语义相关的 suite、2246 条,与真 Git Bash 逐条对照)。
//
// 它们都是那份跑分抓到、而我们自己那份 143 条语料**没有覆盖**的写法。
// 每条都注明对应的 suite,方便回头再查那份报告。

// ---- 1. grep -q ----

// `grep -q` 是 AI 做条件判断的标准写法(`if grep -q x f; then`)。
// 我们原来报「不认识的参数: -q」并 exit 2 —— 而 exit 2 在 if 里既不是真也不是假,
// 分支直接走错,而且是**静默**的。
//
// 来源:spec/vars-special.test.sh(`echo $PWD | grep -q /`)、spec/builtin-bracket.test.sh
func TestGrepHasQuietFlag(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "in.txt"), []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := NewSession()
	if code := s.runForSetup(t, `cd "`+dir+`"`); code != 0 {
		t.Fatalf("cd 失败:%d", code)
	}

	cases := []struct {
		script string
		exit   int
	}{
		{`grep -q alpha in.txt`, 0},
		{`grep -q zzz in.txt`, 1},
		{`grep -q -i ALPHA in.txt`, 0},
		{`grep -qi alpha in.txt`, 0}, // 合并写法
	}
	for _, c := range cases {
		out, exit, errsOut := s.Run(context.Background(), c.script, false)
		if exit != c.exit {
			t.Errorf("%s: exit=%d want %d (errs=%v)", c.script, exit, c.exit, errsOut)
		}
		if out != "" {
			t.Errorf("%s: -q 不该有任何输出, got %q", c.script, out)
		}
	}

	// 条件判断才是它的主战场:exit 0 必须让 then 分支真的走到。
	out, exit, errsOut := s.Run(context.Background(),
		`if grep -q alpha in.txt; then echo hit; fi`, false)
	if exit != 0 || strings.TrimSpace(out) != "hit" {
		t.Errorf("if grep -q …: exit=%d out=%q errs=%v", exit, out, errsOut)
	}

	// 不带 -q 时照旧要输出(别把 -q 实现成"永远静音")。
	out, exit, _ = s.Run(context.Background(), `grep alpha in.txt`, false)
	if exit != 0 || strings.TrimSpace(out) != "alpha" {
		t.Fatalf("不带 -q 时应当输出: exit=%d out=%q", exit, out)
	}
}

// ---- 2. egrep / fgrep ----

// Git Bash 里的 `egrep`/`fgrep` 是 28 字节的**无扩展名** wrapper 脚本,而我们的
// ③ 层只认 PATHEXT 列出的扩展名 —— 于是 `egrep` 直接 127(找不到)。
// 语义上它们就是 `grep -E` 与 `grep -F`。
//
// 来源:spec/pipeline.test.sh(`echo a | egrep '[0-9]+'`)
func TestEgrepAndFgrepMapToGrepFlags(t *testing.T) {
	cases := []struct{ script, want string }{
		{`echo 'a1' | egrep '[0-9]+'`, "a1"},
		{`echo 'a1' | fgrep 1`, "a1"},
		{`echo 'a.b' | fgrep .`, "a.b"}, // -F:点号是字面
		{`echo 'axb' | egrep 'a.b'`, "axb"},
		{`echo 'a b' | egrep 'a\s+b'`, ""}, // -E 里 \s 是 Perl 简写 → 明确拒收(已有行为)
		{`echo 'a1' | egrep -q '[0-9]'; echo $?`, "0"},
	}
	for _, c := range cases {
		s := NewSession()
		out, exit, errsOut := s.Run(context.Background(), c.script, false)
		if c.want == "" {
			// 这一条期望的是"报错",不该静默成功。
			if exit == 0 {
				t.Errorf("%s: 应当报错,exit=0 out=%q", c.script, out)
			}
			continue
		}
		if exit != 0 {
			t.Errorf("%s: exit=%d errs=%v", c.script, exit, errsOut)
			continue
		}
		if strings.TrimSpace(out) != c.want {
			t.Errorf("%s: got %q want %q", c.script, strings.TrimSpace(out), c.want)
		}
	}
}

// ---- 3. echo 的组合开关 ----

// `echo -en …` / `echo -ne …`:mvdan/sh 的 echo 只认单个 -n/-e/-E,拿到组合会把它
// **当普通文本打印出来** —— 输出里凭空多出 "-en ",而且是静默错值。
//
// 来源:spec/builtin-echo.test.sh(`echo -en 'abc\ndef\n'`)
func TestEchoAcceptsCombinedFlags(t *testing.T) {
	cases := []struct{ script, want string }{
		{`echo -en "abc\ndef\n"`, "abc\ndef"},
		{`echo -ne "x\ty"`, "x\ty"},
		{`echo -e "a\tb"`, "a\tb"},  // 单个开关本来就行
		{`echo -n hi; echo`, "hi"},  // -n 不换行
		{`echo -E 'a\tb'`, `a\tb`},  // -E 关掉转义
		{`echo -nE 'a\tb'`, `a\tb`}, // 组合里带 E
	}
	for _, c := range cases {
		s := NewSession()
		out, exit, errsOut := s.Run(context.Background(), c.script, false)
		if exit != 0 {
			t.Errorf("%s: exit=%d errs=%v", c.script, exit, errsOut)
			continue
		}
		if got := strings.TrimRight(out, "\n"); got != c.want {
			t.Errorf("%s: got %q want %q", c.script, got, c.want)
		}
	}
}

// ---- 4. cd 的 POSIX 路径 ----

// spec §8 要求"两种写法都接受",但 `cd` 是 mvdan/sh 的**内建**,不走我们的
// OpenHandler —— 于是 `cd /c/Users` 报 no such file or directory,而 `cd /`
// 更糟:它**静默什么都不做**(`cd / && pwd` 打印的还是原目录)。
//
// 来源:spec/builtin-cd.test.sh
func TestCdAcceptsPosixPaths(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.VolumeName(cwd) + `\` // 当前盘根,例如 D:\

	s := NewSession()

	// `/c/Windows` 形式:与 `C:/Windows` 等价。
	out, exit, errsOut := s.Run(context.Background(), `cd /c/Windows && pwd`, false)
	if exit != 0 {
		t.Fatalf("cd /c/Windows 失败:exit=%d errs=%v", exit, errsOut)
	}
	if strings.TrimSpace(out) != `C:\Windows` {
		t.Errorf("got %q want %q", strings.TrimSpace(out), `C:\Windows`)
	}

	// `/` = 当前盘根。这条以前是**静默失效**的。
	// 先显式回到测试所在的目录,期望才明确(盘根取决于上一次 cd 落在哪个盘)。
	if code := s.runForSetup(t, `cd "`+cwd+`"`); code != 0 {
		t.Fatalf("回到测试目录失败:%d", code)
	}
	out, exit, errsOut = s.Run(context.Background(), `cd / && pwd`, false)
	if exit != 0 {
		t.Fatalf("cd / 失败:exit=%d errs=%v", exit, errsOut)
	}
	if got := strings.TrimSpace(out); got != strings.TrimSuffix(root, `\`) && got != root {
		t.Errorf("cd / 之后 pwd=%q,want %q(当前盘根)", got, root)
	}

	// Windows 形式不能被改坏。
	out, exit, errsOut = s.Run(context.Background(), `cd C:/Windows && pwd`, false)
	if exit != 0 || strings.TrimSpace(out) != `C:\Windows` {
		t.Errorf("cd C:/Windows: exit=%d out=%q errs=%v", exit, strings.TrimSpace(out), errsOut)
	}

	// 相对路径与 `cd ~` 也不受影响。
	if _, exit, errsOut := s.Run(context.Background(), `cd .. && pwd`, false); exit != 0 {
		t.Errorf("cd .. 失败:exit=%d errs=%v", exit, errsOut)
	}
	if out, exit, errsOut := s.Run(context.Background(), `cd ~ && pwd`, false); exit != 0 {
		t.Errorf("cd ~ 失败:exit=%d out=%q errs=%v", exit, out, errsOut)
	}
}
