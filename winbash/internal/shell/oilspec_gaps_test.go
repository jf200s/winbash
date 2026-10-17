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

	// `/` = **Git 安装根**,不是当前盘根。
	//
	// 这条早先断言的是"当前盘根(`D:\`)",那是没有挂载表时的权宜之计(注释原话:
	// "在 Windows 上最接近的语义就是它")。后来补上 MSYS 挂载表(见 builtins/msyspath.go),
	// 真 bash 的证据也拿到了:
	//
	//	$ bash -c 'cd / && pwd -W'
	//	C:/Program Files/Git
	//	$ bash -c 'cd / && ls'      → usr/ etc/ bin/ …(盘根下没有这些)
	//
	// 所以改成与真 bash 一致。这里断言"指向 Git 根"而不硬编码路径,换安装位置仍成立。
	if code := s.runForSetup(t, `cd "`+cwd+`"`); code != 0 {
		t.Fatalf("回到测试目录失败:%d", code)
	}
	out, exit, errsOut = s.Run(context.Background(), `cd / && pwd`, false)
	if exit != 0 {
		t.Fatalf("cd / 失败:exit=%d errs=%v", exit, errsOut)
	}
	if got := strings.TrimSpace(out); got != gitInstallRootForTest() {
		t.Errorf("cd / 之后 pwd=%q, want %q(Git 安装根;真 bash 给 C:/Program Files/Git)",
			got, gitInstallRootForTest())
	}
	_ = root

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

// ---- 2. 管道段不是真子 shell ----

// ★ `cmd | <右侧>` 的右侧**不是**独立子 shell:其中对变量的赋值会泄漏到外层。
//
// 这是本项目**唯一一处静默的 bash 语义差异** —— 两边都 exit 0、都不报错,
// 只有输出不同。其余已知差异(进程替换 / printf 精度 / ${v/#x/y} / ls -d …)
// 都是响亮失败,AI 干活时能立刻察觉;这一条不会。
//
// 根因是上游的**架构限制**,不是本项目的 bug。mvdan/sh 的 README 原话
// (third_party/mvdan-sh/README.md):
//
//	Go does not support forking its own process, so subshells use a goroutine
//	instead, meaning that real PIDs and file descriptors cannot be used directly.
//
// 也就是说管道每一段是一个 goroutine,而不是 fork 出来的进程;变量绑定因此
// 共享同一套作用域,赋值漏得出去。真 bash 里每段是独立进程,赋值随进程消失。
//
// 为什么这条要单独钉住:
//  1. 它是静默的 —— 将来某个升级让它从「泄漏」变成「不泄漏」(或反过来),
//     没有任何现有测试会红,而 AI 的判定逻辑会**悄悄**换一种结果;
//  2. 它影响一种极常见的写法:`cmd | while read x; do n=...; done`。
//
// 顺带说方向:泄漏后的值(`n=2`)才是 AI 直觉预期的,所以这条差异对"AI 干活"
// 是**有利**的;真正要防的是它无声无息地变。
//
// 三条独立证据(都对着同一个机制),任何一条变了就说明机制变了:
//   - `| while read` 的循环计数器
//   - `| f` 里函数内的赋值
//   - `| { export X=1; }` 的 export
func TestPipelineSegmentsAreNotRealSubshells(t *testing.T) {
	cases := []struct {
		what   string
		script string
		exit   int
		// want 是**当前实现**下的输出(即泄漏后的值),不是 bash 的值。
		want string
		// bashWouldBe 是真 Git Bash 给出的值,写在这里作为"这条差异存在"的记录。
		bashWouldBe string
	}{
		{
			what:        "| while read 的循环计数器",
			script:      `n=0; printf "a\nb\n" | while read -r _; do n=$((n+1)); done; echo "n=$n"`,
			want:        "n=2",
			bashWouldBe: "n=0",
		},
		{
			what:        "| f 里函数内的赋值",
			script:      `f(){ n=99; }; echo x | f; echo "n=${n:-unset}"`,
			want:        "n=99",
			bashWouldBe: "n=unset",
		},
		{
			what:        "| { export X=1; } 的 export",
			script:      `echo x | { export PX=1; }; echo "PX=${PX:-unset}"`,
			want:        "PX=1",
			bashWouldBe: "PX=unset",
		},
	}

	for _, c := range cases {
		// 每条用**独立 Session**:上面这些赋值都会落到会话里,共用会互相污染,
		// 让后面的用例因为"变量早就有值"而假绿。
		sub := NewSession()
		out, exit, errsOut := sub.Run(context.Background(), c.script, false)
		_ = sub.Close()

		if exit != c.exit {
			t.Errorf("%s: exit=%d want %d (errs=%v)", c.what, exit, c.exit, errsOut)
		}
		if got := strings.TrimSpace(out); got != c.want {
			t.Errorf("%s:\n  得到 %q\n  期望 %q(当前实现:管道段是 goroutine,赋值会泄漏)\n"+
				"  若这条变了,说明 mvdan/sh 的子 shell 实现变了 —— 请同步更新\n"+
				"  README 的「已知差异」表与本测试,并复查 AI 侧依赖它的判定逻辑",
				c.what, got, c.want)
		}
	}

	// 与上面三条**成对**的:显式 `( )` 子 shell 的隔离是**对的**,必须一起钉住。
	//
	// 为什么必须成对:若将来有人为了"让管道不泄漏"去改子 shell 的实现,很可能
	// 顺手把这三条正确的也改坏 —— 而它们与 bash 一致,改坏才是真回归。
	isolated := []struct {
		what   string
		script string
		want   string
	}{
		{"( ) 子 shell 隔离赋值", `x=OUT; (x=IN); echo "x=$x"`, "x=OUT"},
		{"( ) 里新设的变量不外泄", `(v=IN); echo "v=${v:-unset}"`, "v=unset"},
		{"{ } 分组**不**隔离(与 bash 一致)", `x=OUT; { x=GRP; }; echo "x=$x"`, "x=GRP"},
		{"$( ) 命令替换里赋值不外泄", `z=$(w=SUB; echo got); echo "w=${w:-unset} z=$z"`, "w=unset z=got"},
		{"管道右侧**能读到**外层变量", `lv=7; echo x | { echo "lv=${lv:-unset}"; }`, "lv=7"},
	}
	for _, c := range isolated {
		sub := NewSession()
		out, exit, errsOut := sub.Run(context.Background(), c.script, false)
		_ = sub.Close()
		if exit != 0 {
			t.Errorf("%s: exit=%d (errs=%v)", c.what, exit, errsOut)
			continue
		}
		if got := strings.TrimSpace(out); got != c.want {
			t.Errorf("%s:\n  得到 %q,期望 %q(这条与 bash 一致,不该被「管道泄漏」的修改带坏)",
				c.what, got, c.want)
		}
	}
}

// ★ 管道泄漏有个**正确的**替代写法,必须保证它一直能用。
//
// 这条是给使用者/AI 的出路:知道 `cmd | while` 会泄漏之后,应当改成
// `while ... done < file` 或先把输出落文件。若那种写法将来坏了,这条会红 ——
// 否则「知道有差异」也无法绕开。
func TestRedirectWhileDoesNotLeak(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "in.txt")
	if err := os.WriteFile(file, []byte("a\nb\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := NewSession()
	defer s.Close()

	// 重定向写法:变量**不该**泄漏出 while 的作用域之外……等等,这里要说清楚:
	// 真 bash 里 `while ... done < file` 是**当前 shell** 里执行(不是子 shell),
	// 所以赋值**会**留下来 —— 两边都该是 n=3。这正是它成为推荐替代写法的原因:
	// 管道写法两边不一致,重定向写法两边一致。
	out, exit, errsOut := s.Run(context.Background(),
		`n=0; while read -r _; do n=$((n+1)); done < "`+file+`"; echo "n=$n"`, false)
	if exit != 0 {
		t.Fatalf("退出码 %d(errs=%v)", exit, errsOut)
	}
	if got := strings.TrimSpace(out); got != "n=3" {
		t.Fatalf("重定向 while 得到 %q,期望 n=3 —— 这条与真 bash 一致(赋值会留下来), "+
			"它是「管道会泄漏」的正确替代写法,不该变", got)
	}
}

// ★ README 里给 `ls -d */` 的两条替代写法必须**真的能用**。
//
// 为什么要测试钉文档:我第一版 README 写的是 `ls -a | grep /`,实测**完全不管用**
// (`ls` 的名字不带斜杠,那条没有任何输出、rc=1)。文档给错的替代写法比不给更糟 ——
// 使用者照着写,只会得到一片空白还得自己查。
//
// 还有第二处坑:winbash 的 glob 用**反斜杠**分隔(`echo */` → `dirA\`),
// 所以常见的 `${d%/}` 去不掉尾部 `\`,必须写 `${d%[/\\]}` 或 `basename`。
// 两条推荐写法都钉在这里,任何一条失效都会红。
func TestLsDashDAlternativesWork(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"dirA", "dirB"} {
		if err := os.Mkdir(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "f1.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := NewSession()
	defer s.Close()
	if code := s.runForSetup(t, `cd "`+dir+`"`); code != 0 {
		t.Fatalf("cd 失败:%d", code)
	}

	cases := []struct {
		what   string
		script string
	}{
		{"${d%[/\\\\]} 去两种斜杠", `for d in */; do echo "${d%[/\\]}"; done`},
		{"basename", `for d in */; do basename "$d"; done`},
	}
	for _, c := range cases {
		out, exit, errsOut := s.Run(context.Background(), c.script, false)
		if exit != 0 {
			t.Errorf("%s: exit=%d (errs=%v)", c.what, exit, errsOut)
			continue
		}
		// 必须恰好是两行 dirA / dirB,**不带**尾部反斜杠或斜杠。
		lines := strings.Fields(out)
		if len(lines) != 2 || lines[0] != "dirA" || lines[1] != "dirB" {
			t.Errorf("%s: 得到 %q,期望恰好 [dirA dirB] —— "+
				"这条是 README 里给 `ls -d */` 的替代写法,必须真能用", c.what, out)
		}
	}

	// 反面对照:README 明确说了"只写 `${d%/}` 不够"(去不掉反斜杠)。
	// 钉住它,免得将来 glob 改成正斜杠后 README 的那段警告变成过时信息却没人发现。
	//
	// 这条断言的变异验证边界(实测记录):把它改成 `if false` **不会**让测试红 ——
	// 当前 glob 确实给反斜杠,断言本来就处于"不触发"状态。要真正触发它必须先改变
	// mvdan/sh 的 glob 实现,那不在我们能改的范围。所以它现在的价值是**将来**的
	// 过时检测,不是当下的抓错能力。写在这里,免得下一个人以为它没验证过。
	out, exit, _ := s.Run(context.Background(), `for d in */; do echo "${d%/}"; done`, false)
	if exit != 0 {
		t.Fatalf("反面对照跑了非零退出:%d", exit)
	}
	if !strings.Contains(out, `dirA\`) {
		t.Errorf("winbash 的 glob 现在不再给反斜杠分隔了(得到 %q)—— "+
			"README 里「只去正斜杠不够」那段警告已过时,请一并更新", out)
	}
}
