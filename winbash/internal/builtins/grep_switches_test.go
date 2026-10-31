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
