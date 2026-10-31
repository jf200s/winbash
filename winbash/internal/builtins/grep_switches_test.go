package builtins

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"winbash/internal/errs"
)

// 本文件钉住 2026-10-31 那轮修的三条 P0:
//
//	1. -F 从"静默按 BRE 跑"变成真的字面匹配;
//	2. -E 与 -F 组合从"静默取后者"变成 GNU 同款响亮拒绝(conflicting matchers,exit 2);
//	3. 补齐 -A/-B/-C/-o/-w/-e 六个 AI 最高频开关。
//
// **除特别注明外,全部期望值取自本机真 Git Bash 的 grep**
// (`C:\Program Files\Git\usr\bin\grep.exe`,GNU grep 3.x),逐字对照跑出来的,
// 不是手写 —— 与 grep_test.go 里 BRE 那批 golden 同一条纪律。

// ctx9 是上下文开关共用的 9 行语料;l5 是最常用的命中目标。
const ctx9 = "l1\nl2\nl3\nl4\nl5\nl6\nl7\nl8\nl9\n"

func runGrepStdin(t *testing.T, argv []string, input string) (string, int) {
	t.Helper()
	var out, errw bytes.Buffer
	code := Grep(context.Background(), ".", argv, strings.NewReader(input), &out, &errw)
	return out.String(), code
}

func runGrepErr(t *testing.T, argv []string, input string) (string, string, int) {
	t.Helper()
	var out, errw bytes.Buffer
	code := Grep(context.Background(), ".", argv, strings.NewReader(input), &out, &errw)
	return out.String(), errw.String(), code
}

// ─── 1. -F 字面匹配 ───────────────────────────────────────────────────────────

// 修之前:`-F` 对非 -E 分支毫无作用,`grep -F 'a.c'` 按 BRE 跑,abc 与 a.c 两个都
// 匹配(静默)。实测真 grep 只给 a.c。
func TestGrepDashFIsFixedString(t *testing.T) {
	out, code := runGrepStdin(t, []string{"-F", "a.c"}, "abc\na.c\nxyz\n")
	if code != 0 || out != "a.c\n" {
		t.Fatalf("-F a.c: code=%d out=%q, want a.c(实测真 grep)", code, out)
	}

	out, _ = runGrepStdin(t, []string{"-F", "a+b"}, "a+b\nab\n")
	if out != "a+b\n" {
		t.Fatalf("-F a+b: %q(字面 +,不是重复)", out)
	}

	out, _ = runGrepStdin(t, []string{"-F", "^abc"}, "^abc\nabc\n")
	if out != "^abc\n" {
		t.Fatalf("-F ^abc: %q(字面 ^,不是锚点)", out)
	}
}

// fgrep 是 grep -F:修之前前置的 -F 在 parse 里被静默丢掉,从未有过 fixed-string
// 语义。现在必须有。adapter 只做 `append([]string{"-F"}, argv...)`(它要从
// interp.HandlerCtx 取 stdin,单测里给不了),所以这里直接给前置后的 argv;
// adapter 本身的形状由 e2e(winbash -c 'fgrep a.c')钉。
func TestFgrepHasRealFixedStringSemantics(t *testing.T) {
	out, code := runGrepStdin(t, []string{"-F", "a.c"}, "abc\na.c\n")
	if code != 0 || out != "a.c\n" {
		t.Fatalf("fgrep a.c: code=%d out=%q, want a.c", code, out)
	}
}

// ─── 2. -E 与 -F 组合:conflicting matchers ──────────────────────────────────

// 实测 GNU 对 -FE / -EF / -E -F / -F -E 一律 `grep: conflicting matchers
// specified`、exit 2、stdout 为空;重复给同一个(-EE / -FF)不冲突。
func TestGrepConflictingMatchersRejected(t *testing.T) {
	for _, argv := range [][]string{
		{"-FE", "x"}, {"-EF", "x"}, {"-E", "-F", "x"}, {"-F", "-E", "x"},
	} {
		out, errw, code := runGrepErr(t, argv, "x\ny\n")
		if code != errs.ExitUsage {
			t.Errorf("%v: code=%d want 2(GNU 实测 exit 2)", argv, code)
		}
		if out != "" {
			t.Errorf("%v: 竟有输出 %q", argv, out)
		}
		if !strings.Contains(errw, "conflicting matchers specified") {
			t.Errorf("%v: 错误要对齐 GNU 的 conflicting matchers 措辞: %q", argv, errw)
		}
		// 三行式里要有下一步:说明只能选一个方言。
		if !strings.Contains(errw, "怎么办") {
			t.Errorf("%v: 缺三行式的怎么办段: %q", argv, errw)
		}
	}

	// 同一方言给两遍不冲突(实测 -FF 合法)。
	out, code := runGrepStdin(t, []string{"-F", "-F", "x"}, "x\n")
	if code != 0 || out != "x\n" {
		t.Fatalf("-F -F: code=%d out=%q", code, out)
	}
}

// egrep / fgrep 前置的 -E / -F 同样算显式方言(实测 GNU 的 egrep -F 也是
// conflicting matchers;fgrep -F 合法)。adapter 只是把前缀插到 argv 最前,
// 所以下面直接给"前置之后"的 argv —— 与 registry.go 里两个 adapter 的行为一致。
func TestGrepAliasesConflictToo(t *testing.T) {
	// egrep -F(= grep -E -F)→ 冲突。
	_, errw, code := runGrepErr(t, []string{"-E", "-F", "x"}, "x\n")
	if code != errs.ExitUsage || !strings.Contains(errw, "conflicting matchers") {
		t.Errorf("egrep -F: code=%d err=%q", code, errw)
	}
	// fgrep -E(= grep -F -E)→ 冲突。
	_, errw, code = runGrepErr(t, []string{"-F", "-E", "x"}, "x\n")
	if code != errs.ExitUsage || !strings.Contains(errw, "conflicting matchers") {
		t.Errorf("fgrep -E: code=%d err=%q", code, errw)
	}
	// fgrep -F(= grep -F -F)→ 合法,且是字面匹配。
	out, code := runGrepStdin(t, []string{"-F", "-F", "a.c"}, "abc\na.c\n")
	if code != 0 || out != "a.c\n" {
		t.Fatalf("fgrep -F: code=%d out=%q", code, out)
	}
}

// ─── 3. 上下文开关 -A / -B / -C ──────────────────────────────────────────────

// 期望值全部来自真 grep(实测:`grep -A2 l5`、`-A 2`、`-B 2`、`-C 2` 对 ctx9)。
func TestGrepContextFlags(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want string
	}{
		{"-A2 附着", []string{"-A2", "l5"}, "l5\nl6\nl7\n"},
		{"-A 2 隔开", []string{"-A", "2", "l5"}, "l5\nl6\nl7\n"},
		{"-B 2", []string{"-B", "2", "l5"}, "l3\nl4\nl5\n"},
		{"-C 2", []string{"-C", "2", "l5"}, "l3\nl4\nl5\nl6\nl7\n"},
		{"-n -A1(上下文行号用 - 后缀,实测)", []string{"-n", "-A1", "l5"}, "5:l5\n6-l6\n"},
		{"-n -B1", []string{"-n", "-B1", "l5"}, "4-l4\n5:l5\n"},
		{"-n -C1", []string{"-n", "-C1", "l5"}, "4-l4\n5:l5\n6-l6\n"},
		{"-nA1 合并附着", []string{"-nA1", "l5"}, "5:l5\n6-l6\n"},
	}
	for _, c := range cases {
		out, code := runGrepStdin(t, c.argv, ctx9)
		if code != 0 || out != c.want {
			t.Errorf("%s: code=%d got %q want %q", c.name, code, out, c.want)
		}
	}
}

// 组间隔线 `--`:两组之间有没被覆盖的空档时补一条;相邻/重叠的组合并。
// 全部实测(`grep -A1 -e l3 -e l7`、`-A1 -e l3 -e l5`、`-A0 -e l3 -e l7`)。
func TestGrepContextGroupSeparator(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want string
	}{
		{"隔空两组补 --", []string{"-A1", "-e", "l3", "-e", "l7"}, "l3\nl4\n--\nl7\nl8\n"},
		{"相邻组合并不打 --", []string{"-A1", "-e", "l3", "-e", "l5"}, "l3\nl4\nl5\nl6\n"},
		{"-A0 隔空照样补 --(实测)", []string{"-A0", "-e", "l3", "-e", "l7"}, "l3\n--\nl7\n"},
		{"-B 组起点与上一组相接不打 --(实测 -A1 -B1 l3,l6)", []string{"-A1", "-B1", "-e", "l3", "-e", "l6"},
			"l2\nl3\nl4\nl5\nl6\nl7\n"},
		{"组重叠时命中行按命中格式重打(实测 -n -A2 l3,l4)", []string{"-n", "-A2", "-e", "l3", "-e", "l4"},
			"3:l3\n4:l4\n5-l5\n6-l6\n"},
	}
	for _, c := range cases {
		out, code := runGrepStdin(t, c.argv, ctx9)
		if code != 0 || out != c.want {
			t.Errorf("%s: code=%d got %q want %q", c.name, code, out, c.want)
		}
	}
}

// 没给任何 -A/-B/-C 时,裸的多组命中之间**不打** `--`(实测);显式 -A0 才打。
func TestGrepNoSeparatorWithoutContextFlags(t *testing.T) {
	out, _ := runGrepStdin(t, []string{"-e", "l3", "-e", "l7"}, ctx9)
	if out != "l3\nl7\n" {
		t.Fatalf("裸两命中不该有 --(实测): %q", out)
	}
}

// -v 与上下文同给:被选中(-v 反转后)的行同样带上下文。实测 `grep -v -A1 l5`
// 对 ctx9 输出全部 9 行(各组相接,无 `--`)。
func TestGrepInvertWithContext(t *testing.T) {
	out, code := runGrepStdin(t, []string{"-v", "-A1", "l5"}, ctx9)
	if code != 0 || out != ctx9 {
		t.Fatalf("-v -A1: code=%d out=%q want 全部 9 行(实测)", code, out)
	}
}

// -c / -l / -q 下上下文全部失效(实测:-c -A1 只给计数;-l -A1 只报名字)。
func TestGrepContextSuppressedByCountListQuiet(t *testing.T) {
	out, code := runGrepStdin(t, []string{"-c", "-A1", "l5"}, ctx9)
	if code != 0 || out != "1\n" {
		t.Fatalf("-c -A1: code=%d out=%q want 1(实测)", code, out)
	}
	out, _ = runGrepStdin(t, []string{"-q", "-A1", "l5"}, ctx9)
	if out != "" {
		t.Fatalf("-q -A1 竟有输出: %q", out)
	}
}

// 非法的上下文长度:实测 GNU 是 `<值>: invalid context length argument`、exit 2;
// 负数与带尾巴的数字同样拒收。`-A` 后面紧跟的下一参照样被当值吃掉(实测
// `grep -nA -e x` 报 `-e: invalid context length argument`)。
func TestGrepInvalidContextLength(t *testing.T) {
	for _, argv := range [][]string{
		{"-Ax", "l5"}, {"-A", "2x", "l5"}, {"-A", "-1", "l5"}, {"-nA", "-e", "l5"},
	} {
		_, errw, code := runGrepErr(t, argv, ctx9)
		if code != errs.ExitUsage {
			t.Errorf("%v: code=%d want 2", argv, code)
		}
		if !strings.Contains(errw, "invalid context length argument") {
			t.Errorf("%v: 错误要对齐 GNU 措辞: %q", argv, errw)
		}
	}
}

// -A 缺参数:实测 GNU 给 exit 2(option requires an argument)。
func TestGrepContextMissingArgument(t *testing.T) {
	_, _, code := runGrepErr(t, []string{"-A"}, ctx9)
	if code != errs.ExitUsage {
		t.Fatalf("code=%d want 2", code)
	}
}

// -A3 -C1 与 -C1 -A3 都是 after=3/before=1,-C1 -B3 是 before=3/after=1
// (实测 GNU:某维度被 -A/-B 显式给过时,-C 对该维度让位,与顺序无关)。
func TestGrepContextFlagPrecedence(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want string
	}{
		{"-A3 -C1", []string{"-A3", "-C1", "l5"}, "l4\nl5\nl6\nl7\nl8\n"},
		{"-C1 -A3", []string{"-C1", "-A3", "l5"}, "l4\nl5\nl6\nl7\nl8\n"},
		{"-C1 -B3", []string{"-C1", "-B3", "l5"}, "l2\nl3\nl4\nl5\nl6\n"},
	}
	for _, c := range cases {
		out, code := runGrepStdin(t, c.argv, ctx9)
		if code != 0 || out != c.want {
			t.Errorf("%s: code=%d got %q want %q", c.name, code, out, c.want)
		}
	}
}

// 多文件:每行都带文件名前缀(命中 `:`、上下文 `-`),组间隔线是**裸**的 `--`,
// 文件之间不打 `--`。实测 GNU 对两个文件 -A1 的输出正是这个形状。
func TestGrepContextMultiFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "f1.txt"), []byte("a1\nb\na2\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "f2.txt"), []byte("a3\nz\n"), 0o644)

	var out bytes.Buffer
	code := Grep(context.Background(), dir, []string{"-A1", "^a", "f1.txt", "f2.txt"},
		strings.NewReader(""), &out, nil)
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	want := "f1.txt:a1\nf1.txt-b\nf1.txt:a2\n--\nf2.txt:a3\nf2.txt-z\n"
	if out.String() != want {
		t.Fatalf("got %q want %q(实测 GNU 多文件 -A1)", out.String(), want)
	}
}

// 非递归单文件 + 递归树:组间隔线的游标是调用级的,文件边界上也补 `--`
// (实测 GNU `grep -r -A1 '^a' 目录` 每个文件之间都有 `--`)。
func TestGrepContextSeparatorAcrossFilesRecursive(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "f1.txt"), []byte("a1\nb\na2\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "sub", "f2.txt"), []byte("a3\nz\n"), 0o644)

	var out bytes.Buffer
	code := Grep(context.Background(), dir, []string{"-r", "-A1", "^a", "."},
		strings.NewReader(""), &out, nil)
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	want := "./f1.txt:a1\n./f1.txt-b\n./f1.txt:a2\n--\n./sub/f2.txt:a3\n./sub/f2.txt-z\n"
	if out.String() != want {
		t.Fatalf("got %q want %q(实测 GNU -r:文件边界也有 --)", out.String(), want)
	}
}

// ─── 4. -o 只输出命中段 ──────────────────────────────────────────────────────

// 实测:`grep -o 'mm'` 逐段输出;`-o -n` 行号后仍是 `:`;多文件时名字在最前。
func TestGrepOnlyMatching(t *testing.T) {
	corpus := "aaa mmm bbb\nzzz\nccc mmm ddd\n"
	out, code := runGrepStdin(t, []string{"-o", "mm"}, corpus)
	if code != 0 || out != "mm\nmm\n" {
		t.Fatalf("-o mm: code=%d out=%q(实测)", code, out)
	}
	// -o 与 -E:贪婪给最长(实测 `grep -o -n -E 'm+'` 给 1:mmm / 3:mmm)。
	out, _ = runGrepStdin(t, []string{"-o", "-n", "-E", "m+"}, corpus)
	if out != "1:mmm\n3:mmm\n" {
		t.Fatalf("-o -n -E: %q(实测)", out)
	}
	// 空命中不输出(实测 `grep -o 'b*'` 对 abc 只给一个 b;`x*` 一个字都没有但 exit 0)。
	out, _ = runGrepStdin(t, []string{"-o", "b*"}, "abc\n")
	if out != "b\n" {
		t.Fatalf("-o b*: %q(实测只给 b,空命中不输出)", out)
	}
	_, code = runGrepStdin(t, []string{"-o", "x*"}, "abc\n")
	if code != 0 {
		t.Fatalf("-o x*: 行仍被选中(实测 exit 0), got %d", code)
	}
}

// -o 与上下文同给:上下文行不打印,但组间隔线照打(实测 `-o -A1 -e l3 -e l7`
// 给 l3 / -- / l7;相邻组 `-o -C1 -e l3 -e l5` 不打)。
func TestGrepOnlyMatchingWithContext(t *testing.T) {
	out, code := runGrepStdin(t, []string{"-o", "-A1", "-e", "l3", "-e", "l7"}, ctx9)
	if code != 0 || out != "l3\n--\nl7\n" {
		t.Fatalf("-o -A1 隔空: code=%d out=%q(实测含 --)", code, out)
	}
	out, _ = runGrepStdin(t, []string{"-o", "-C1", "-e", "l3", "-e", "l5"}, ctx9)
	if out != "l3\nl5\n" {
		t.Fatalf("-o -C1 相邻: %q(实测不打 --)", out)
	}
}

// 实测 `-o -v` 一个字都不出(被选中的行本来就不含命中段),exit 仍看有没有行被选中。
func TestGrepOnlyMatchingInvertPrintsNothing(t *testing.T) {
	out, code := runGrepStdin(t, []string{"-o", "-v", "mmm"}, "aaa mmm bbb\nzzz\nccc mmm ddd\n")
	if code != 0 || out != "" {
		t.Fatalf("-o -v: code=%d out=%q(实测无输出、exit 0)", code, out)
	}
}

// -c / -l 优先于 -o(实测:-c -o 给行数 2,不给命中段)。
func TestGrepCountWinsOverOnlyMatching(t *testing.T) {
	out, code := runGrepStdin(t, []string{"-c", "-o", "mmm"}, "mmm\nx\nmmm\ny\n")
	if code != 0 || out != "2\n" {
		t.Fatalf("-c -o: code=%d out=%q(实测给 2)", code, out)
	}
}

// ─── 5. -w 全词匹配 ──────────────────────────────────────────────────────────

// 词字符 = 字母、数字、下划线(实测:abc_def 不被 -w abc 命中,abc1 也不命中)。
func TestGrepWholeWord(t *testing.T) {
	corpus := "abc\nabbc\nxabc\nabcx\nabc_def\nabc1\na-bc\n"
	out, code := runGrepStdin(t, []string{"-w", "abc"}, corpus)
	if code != 0 || out != "abc\n" {
		t.Fatalf("-w abc: code=%d out=%q want abc(实测)", code, out)
	}
	// 词内命中不算、词边界命中算(实测 `-w oo` 对 `foo oo` 给整行)。
	out, _ = runGrepStdin(t, []string{"-w", "oo"}, "foo oo\nxx\n")
	if out != "foo oo\n" {
		t.Fatalf("-w oo: %q(实测给 foo oo 行)", out)
	}
	// -w 是"该行有没有全词命中",与 -o 无关时输出整行;命中段两侧词字符检查
	// 以**命中段**为准(实测 -w 'o' 对 `f-o- o` 命中:末尾那个 o 两侧都是词边界)。
	out, _ = runGrepStdin(t, []string{"-w", "o"}, "f-o- o\nxx\n")
	if out != "f-o- o\n" {
		t.Fatalf("-w o: %q(实测)", out)
	}
	// -w 与 -E 同给:检查落在正则的命中段上(实测 -w -E 'm+' 对 xmmm 不命中)。
	out, _ = runGrepStdin(t, []string{"-w", "-E", "m+"}, "xmmm\nmmm\n")
	if out != "mmm\n" {
		t.Fatalf("-w -E m+: %q(实测只有 mmm 行)", out)
	}
	// -w 与 -c:数的是**行**(实测 -c -w o 对 `foo oo` 给 0、exit 1)。
	out, code = runGrepStdin(t, []string{"-c", "-w", "o"}, "foo oo\n")
	if code != 1 || out != "0\n" {
		t.Fatalf("-c -w o: code=%d out=%q(实测 0/exit 1)", code, out)
	}
}

// -w 与 -o 同给:只输出**全词**的命中段(实测 `-o -w oo` 对 `foo oo` 只给
// 独立的 oo,不给 foo 里的)。
func TestGrepWholeWordOnlyMatching(t *testing.T) {
	out, _ := runGrepStdin(t, []string{"-o", "-w", "oo"}, "foo oo\n")
	if out != "oo\n" {
		t.Fatalf("-o -w oo: %q(实测只给独立的 oo)", out)
	}
	out, _ = runGrepStdin(t, []string{"-o", "-w", "o"}, "f-o- o\n")
	if out != "o\no\n" {
		t.Fatalf("-o -w o: %q(实测两段:夹在 - 之间的与行尾的)", out)
	}
}

// -w 对 -F 同样生效(fgrep -w 就是"字面量两侧都是词边界",实测)。
func TestGrepWholeWordWithFixedStrings(t *testing.T) {
	out, _ := runGrepStdin(t, []string{"-F", "-w", "oo"}, "foo oo\n")
	if out != "foo oo\n" {
		t.Fatalf("-F -w oo: %q", out)
	}
	out, _ = runGrepStdin(t, []string{"-F", "-o", "-w", "oo"}, "foo oo\n")
	if out != "oo\n" {
		t.Fatalf("-F -o -w oo: %q(实测只给独立的 oo)", out)
	}
}

// ─── 6. -e 多模式 ────────────────────────────────────────────────────────────

// 实测:-e 可多次,OR 语义;模式可以 - 开头;空模式匹配所有行;缺参数 exit 2;
// -c -e 数"命中任一模式的行"。
func TestGrepMultiplePatterns(t *testing.T) {
	out, code := runGrepStdin(t, []string{"-e", "l3", "-e", "l7"}, ctx9)
	if code != 0 || out != "l3\nl7\n" {
		t.Fatalf("-e 多模式: code=%d out=%q(实测)", code, out)
	}
	// 模式以 - 开头:必须用 -e(实测 `grep -e -x` 命中 `-x` 行)。
	out, _ = runGrepStdin(t, []string{"-e", "-x"}, "-x\ny\n")
	if out != "-x\n" {
		t.Fatalf("-e -x: %q(实测)", out)
	}
	// -c -e:数行,不数模式(实测给 2)。
	out, _ = runGrepStdin(t, []string{"-c", "-e", "l3", "-e", "l7"}, ctx9)
	if out != "2\n" {
		t.Fatalf("-c -e: %q(实测 2)", out)
	}
	// -o 与多个 -e:按**行内位置**排序,不按 -e 的给出顺序
	// (实测 `grep -oE -e b -e a` 对 `ab` 给 a、b)。
	out, _ = runGrepStdin(t, []string{"-o", "-E", "-e", "b", "-e", "a"}, "ab\n")
	if out != "a\nb\n" {
		t.Fatalf("-o -e 顺序: %q(实测按位置 a 在前)", out)
	}
	// -e 缺参数:exit 2(实测 GNU 同样 exit 2)。
	_, _, code = runGrepErr(t, []string{"-e"}, "x\n")
	if code != errs.ExitUsage {
		t.Fatalf("-e 缺参: code=%d want 2", code)
	}
}

// 实测 GNU:给了 -e 之后**所有**位置参数都是文件名(pattern 只有 -e 那些一个)。
// `grep -e a b.txt f.txt` 在 b.txt、f.txt 里找 a,而不是把 b.txt 当 pattern。
func TestGrepDashEPositionalArgsAreFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("a\nb\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("a\nz\n"), 0o644)

	var out bytes.Buffer
	code := Grep(context.Background(), dir, []string{"-e", "a", "b.txt", "f.txt"},
		strings.NewReader(""), &out, nil)
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	want := "b.txt:a\nf.txt:a\n"
	if out.String() != want {
		t.Fatalf("got %q want %q(实测:位置参数全是文件名)", out.String(), want)
	}
}

// -e 与 -F 同给:每个模式都是字面量(实测 -Fe 成立)。
func TestGrepDashEWithFixedStrings(t *testing.T) {
	out, code := runGrepStdin(t, []string{"-F", "-e", "a.c", "-e", "a+b"}, "abc\na.c\na+b\n")
	if code != 0 || out != "a.c\na+b\n" {
		t.Fatalf("-F -e: code=%d out=%q(两个字面量都按字面)", code, out)
	}
}

// ─── 7. -x 整行匹配(2026-10-31 实测补齐)─────────────────────────────────────
//
// 期望值取自本机真 Git Bash 的 grep(GNU grep 3.0,C.UTF-8),同上纪律。
// 口径:模式被隐式锚定到**整行**;部分命中一律不算;-o 下唯一的命中段就是整行;
// -F 时等价于"整行字面相等";-v/-c/-n/-m 照常叠加。
func TestGrepWholeLine(t *testing.T) {
	cases := []struct {
		name  string
		argv  []string
		input string
		want  string
	}{
		{"部分命中不算(实测 xabc/abcx/ab c 都不中)", []string{"-x", "abc"}, "abc\nxabc\nabcx\nab c\n", "abc\n"},
		{"-x -F 整行字面相等(实测 abc 不中 a.c 的整行)", []string{"-x", "-F", "a.c"}, "a.c\nabc\nxa.c\n", "a.c\n"},
		{"-x -v(实测)", []string{"-x", "-v", "abc"}, "abc\nxabc\n", "xabc\n"},
		{"-x -c(实测 2)", []string{"-x", "-c", "abc"}, "abc\nxabc\nabc\n", "2\n"},
		{"-x -o 命中段就是整行(实测一段 abc)", []string{"-x", "-o", "abc"}, "abc\n", "abc\n"},
		{"-x -o 部分命中无输出(实测 b* 不跨整行、rc=1)", []string{"-x", "-o", "-E", "b*"}, "abc\n", ""},
		{"-x -o -E 'a*' 对 aaa 给整行(实测)", []string{"-x", "-o", "-E", "a*"}, "aaa\n", "aaa\n"},
		{"-x -E 正则整行(实测 ab+ 中 ab、不中 abbc/ab*)", []string{"-x", "-E", "ab+"}, "ab\nabbc\nab*\n", "ab\n"},
		{"-x -n(实测 1:abc)", []string{"-n", "-x", "abc"}, "abc\nxabc\n", "1:abc\n"},
		{"-x 空模式只中空行(实测)", []string{"-x", ""}, "abc\n\n", "\n"},
		{"-x -E 'x*' 对空行命中(实测)", []string{"-x", "-E", "x*"}, "\n", "\n"},
		{"-x -E 'x*' 对 abc 不中(实测 rc=1)", []string{"-x", "-E", "x*"}, "abc\n", ""},
		{"-x -w 叠加:整行命中自带词边界(实测)", []string{"-x", "-w", "abc"}, "abc\nxabc\n", "abc\n"},
		{"-x -c -v(实测 1)", []string{"-x", "-c", "-v", "abc"}, "abc\nxabc\nabc\n", "1\n"},
		{"-x -m 1 组合(实测)", []string{"-x", "-m", "1", "abc"}, "abc\nxabc\nabc\n", "abc\n"},
	}
	for _, c := range cases {
		out, code := runGrepStdin(t, c.argv, c.input)
		wantCode := 0
		if c.want == "" {
			wantCode = 1 // GNU 实测:无命中 exit 1
		}
		if code != wantCode || out != c.want {
			t.Errorf("%s: code=%d got %q want %q(rc=%d)", c.name, code, out, c.want, wantCode)
		}
	}
}

// -x 多文件:-c 每文件独立计数(实测 f1:1 / f2:1)。
func TestGrepWholeLineMultiFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "f1.txt"), []byte("abc\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "f2.txt"), []byte("xabc\nabc\n"), 0o644)

	var out bytes.Buffer
	code := Grep(context.Background(), dir, []string{"-x", "-c", "abc", "f1.txt", "f2.txt"},
		strings.NewReader(""), &out, nil)
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	want := "f1.txt:1\nf2.txt:1\n"
	if out.String() != want {
		t.Fatalf("got %q want %q(实测 GNU -x -c 多文件)", out.String(), want)
	}
}

// ─── 8. -m 次数上限(2026-10-31 实测补齐)─────────────────────────────────────
//
// 期望值取自本机真 Git Bash 的 grep(同上纪律)。关键口径(全部实测):
//
//   - -m N 按**选中行**数封顶 —— 即使 -o 也按行算:-m1 -o 对一行里三个 a 给 3 段,
//     对两行给前 1 行的全部段;
//   - -c 的计数同样封顶;-m 0 时连 -c 的计数行都不给(无输出、exit 1);
//   - 负数不设上限(GNU 的 strtoumax 把 -1 回绕成极大值,实测 -m -1 给全部命中);
//   - 多个 -m 后者胜;
//   - 到顶后 GNU 停止读入:尾随 -A 上下文只吐"到顶后已读到"的行,且到顶后
//     本应选中的行整个跳过 —— 不输出、也不当作上一组的上下文
//     (实测 -m1 -A1 对 a1,a2,a3 只给 a1;-m1 -A2 对 a1,x1,a2,… 只给 a1,x1)。
func TestGrepMaxCount(t *testing.T) {
	cases := []struct {
		name     string
		argv     []string
		input    string
		want     string
		wantCode int // -1 = 按 want 推(want 空 → 1);默认 -1;-q 命中时显式给 0
	}{
		{"-m2 附着", []string{"-m2", "a"}, "a\nb\na\n", "a\na\n", -1},
		{"-m 2 隔开", []string{"-m", "2", "a"}, "a\nb\na\nb\na\n", "a\na\n", -1},
		{"-m 2 -c 计数封顶", []string{"-m", "2", "-c", "a"}, "a\nb\na\nb\na\n", "2\n", -1},
		{"-m 0 无输出 rc=1", []string{"-m", "0", "a"}, "a\nb\na\n", "", -1},
		{"-m 0 -c 连计数都不给(实测)", []string{"-m", "0", "-c", "a"}, "a\nb\na\n", "", -1},
		{"-m 0 -v 无输出 rc=1(实测)", []string{"-m", "0", "-v", "a"}, "a\nb\na\n", "", -1},
		{"-m -1 不设上限(GNU 负数回绕,实测)", []string{"-m", "-1", "a"}, "a\nb\na\n", "a\na\n", -1},
		{"多个 -m 后者胜(实测)", []string{"-m", "2", "-m", "1", "a"}, "a\na\na\n", "a\n", -1},
		{"-m 1 -o 按行封顶,行内段全给(实测 3 段)", []string{"-m", "1", "-o", "a"}, "a a a\nz\n", "a\na\na\n", -1},
		{"-m 2 -o 两行 8 段(实测)", []string{"-m", "2", "-o", "a"}, "aa aa\nz\naa aa\n",
			"a\na\na\na\na\na\na\na\n", -1},
		{"-m 2 -v 封顶(实测 b,b)", []string{"-m", "2", "-v", "a"}, "a\nb\na\nb\na\n", "b\nb\n", -1},
		{"-m 2 -v -c(实测 2)", []string{"-m", "2", "-v", "-c", "a"}, "a\nb\na\nb\na\n", "2\n", -1},
		{"-m 1 无上下文(实测只给第一行)", []string{"-m", "1", "a"}, "a1\nx1\na2\nx2\na3\n", "a1\n", -1},
		{"-m 1 -A1:到顶后的命中行不输出也不作上下文(实测 a1,x1)", []string{"-m", "1", "-A1", "a"},
			"a1\nx1\na2\nx2\na3\n", "a1\nx1\n", -1},
		{"-m 1 -A1 相邻:第二条根本不读(实测只给 a1)", []string{"-m", "1", "-A1", "a"},
			"a1\na2\na3\n", "a1\n", -1},
		{"-m 1 -A2:第二条上下文行不读(实测 a1,x1)", []string{"-m", "1", "-A2", "a"},
			"a1\nx1\na2\nx2\na3\n", "a1\nx1\n", -1},
		{"-m 1 -C1(实测 x0,a1,x1)", []string{"-m", "1", "-C1", "a"}, "x0\na1\nx1\na2\nx2\n", "x0\na1\nx1\n", -1},
		{"-m 2 -A1 -o 隔空组照样补 --(实测 a,--,a)", []string{"-m", "2", "-A1", "-o", "a"},
			"a\nx\nx\nx\na\nx\nx\nx\na\n", "a\n--\na\n", -1},
		{"-m 1 -c(实测 1)", []string{"-m", "1", "-c", "a"}, "a1\nx1\na2\n", "1\n", -1},
		{"-m 2 -q 无输出 rc=0(实测)", []string{"-m", "2", "-q", "a"}, "a\na\n", "", 0},
	}
	for _, c := range cases {
		out, code := runGrepStdin(t, c.argv, c.input)
		wantCode := c.wantCode
		if wantCode == -1 {
			if c.want == "" {
				wantCode = 1 // GNU 实测:没有行被选中 exit 1(-m 0 连 -c 也如此)
			} else {
				wantCode = 0
			}
		}
		if code != wantCode || out != c.want {
			t.Errorf("%s: code=%d got %q want %q(rc=%d)", c.name, code, out, c.want, wantCode)
		}
	}
}

// -m 非法值:实测 GNU 是 `invalid max count`(消息里**不带**值)、exit 2;
// 缺参数 exit 2。多文件时每文件独立计数(实测各 1)。
func TestGrepMaxCountInvalidAndMultiFile(t *testing.T) {
	for _, argv := range [][]string{{"-m", "2x", "a"}, {"-m", "", "a"}, {"-m", "a", "f"}} {
		_, errw, code := runGrepErr(t, argv, "a\n")
		if code != errs.ExitUsage {
			t.Errorf("%v: code=%d want 2(实测 invalid max count)", argv, code)
		}
		if !strings.Contains(errw, "invalid max count") {
			t.Errorf("%v: 错误要对齐 GNU 的 invalid max count 措辞: %q", argv, errw)
		}
	}
	// 缺参数:exit 2(与 -A/-e 缺参数同款措辞)。
	_, _, code := runGrepErr(t, []string{"-m"}, "a\n")
	if code != errs.ExitUsage {
		t.Errorf("-m 缺参: code=%d want 2", code)
	}

	// 多文件:每文件独立计数、独立封顶(实测 f1:1 / f2:1)。
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "f1.txt"), []byte("a\na\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "f2.txt"), []byte("a\n"), 0o644)

	var out bytes.Buffer
	code = Grep(context.Background(), dir, []string{"-m", "1", "-c", "a", "f1.txt", "f2.txt"},
		strings.NewReader(""), &out, nil)
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	if want := "f1.txt:1\nf2.txt:1\n"; out.String() != want {
		t.Fatalf("got %q want %q(实测每文件独立)", out.String(), want)
	}

	// -l 在第一次命中就返回,-m 不改变它(实测两个名字都在)。
	out.Reset()
	code = Grep(context.Background(), dir, []string{"-m", "2", "-l", "a", "f1.txt", "f2.txt"},
		strings.NewReader(""), &out, nil)
	if code != 0 || out.String() != "f1.txt\nf2.txt\n" {
		t.Fatalf("-m 2 -l: code=%d out=%q(实测)", code, out.String())
	}
}

// ─── 9. -w 词边界的 Unicode 判定(2026-10-31 实测改齐)────────────────────────
//
// 词字符 = 字母/数字/下划线,按 rune 解码用 unicode 包判定 —— GNU 在 UTF-8
// locale 下把非 ASCII 字母/数字也算词字符(旧实现按 ASCII byte 判,`-w '中'`
// 会错放 `中文`)。以下期望值全部来自真 Git Bash grep 实测:
//
//	-w '中'   对 `中文`   → 不命中(右邻的「文」是词字符,挡住词尾边界)
//	-w '中文' 对 `中文`   → 命中(整词)
//	-w '文'   对 `文中`   → 不命中(右邻的「中」挡住;两边一致)
//	-w '中文' 对 `中文中` → 不命中(右邻挡住)
//	-w 'α'    对 `α٣`     → 不命中(阿拉伯-印度数字 ٣ 是 Nd 类,挡右界)
//	-w 'α'    对 `α³`     → 命中(上标 ³ 是 No 类,不算词字符)
//	-w 'àn'   对 `àn`     → 命中(á 是字母,但在**行首侧**,不夹在中间)
//
// GNU 的词边界检查有一条未复刻的不对称(见 已知差异与限制.md):命中段**左**
// 邻是非 ASCII 字符时它不拦(实测 `-w '文'` 命中 `中文`、`-w 'β'` 命中 `αβ`)。
// 我们按对称 Unicode 判、这类行不命中 —— 因此这一批用例**不进** golden。
func TestGrepWholeWordUnicode(t *testing.T) {
	cases := []struct {
		name  string
		argv  []string
		input string
		want  string
	}{
		{"-w 中 对 中文:右邻词字符挡住(旧 ASCII 判定会错放)", []string{"-w", "中"}, "中文\n", ""},
		{"-w 中文 对 中文:整词命中", []string{"-w", "中文"}, "中文\n", "中文\n"},
		{"-w 中 对 '中 中':两侧独立词(实测)", []string{"-w", "中"}, "中 中\n", "中 中\n"},
		{"-o -w 中 对 '中 中'(实测两段)", []string{"-o", "-w", "中"}, "中 中\n", "中\n中\n"},
		{"-o -w 中 对 '中,中文'(实测只给逗号前那个)", []string{"-o", "-w", "中"}, "中,中文\n", "中\n"},
		{"-w 文 对 文中:右邻挡住(实测两边一致)", []string{"-w", "文"}, "文中\n", ""},
		{"-w 中文 对 中文中:右邻挡住(实测)", []string{"-w", "中文"}, "中文中\n", ""},
		{"-F -w 中 对 中文(实测 rc=1)", []string{"-F", "-w", "中"}, "中文\n", ""},
		{"-w α 对 α٣:Nd 数字挡右界(实测)", []string{"-w", "α"}, "α٣\n", ""},
		{"-w α 对 α³:No 类不算词字符(实测)", []string{"-w", "α"}, "α³\n", "α³\n"},
		{"-w àn 对 àn:带音符字母成整词(实测)", []string{"-w", "àn"}, "àn\n", "àn\n"},
		{"ASCII 回归:abc_def 不被 -w abc 命中", []string{"-w", "abc"}, "abc_def\n", ""},
		{"ASCII 回归:abc1 不被 -w abc 命中", []string{"-w", "abc"}, "abc1\n", ""},
	}
	for _, c := range cases {
		out, code := runGrepStdin(t, c.argv, c.input)
		wantCode := 0
		if c.want == "" {
			wantCode = 1
		}
		if code != wantCode || out != c.want {
			t.Errorf("%s: code=%d got %q want %q(rc=%d)", c.name, code, out, c.want, wantCode)
		}
	}
}
