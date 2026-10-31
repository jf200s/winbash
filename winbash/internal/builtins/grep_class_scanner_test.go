package builtins

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"
)

// 本文件补的是 §3.2 那类改动的**反方向**守卫。
//
// 上一轮修好 `[[:alpha:]\d]` 时,我的说法是"绝不能把'响亮报错'变成'静默少匹配'"。
// 但**意图不是机制**:当时那份 golden 只钉了一个方向(合法 ERE 必须能跑、结果要对),
// 反方向(改写动作本身把类内的 `\` 原样透传出去)没有任何测试能抓住。
//
// 所以这里钉两件事,都用**纯函数**、不需要真 bash:
//
//	1. normalizeEreClasses 的**输出字符串** —— 改写这个动作本身对不对;
//	2. Perl 简写的拒收**仍然生效** —— 修 §3.2 时最容易顺手放开的正是这里。

// TestNormalizeEreClassesExactOutput 钉住**改写后的字符串**,不是"跑出来的结果"。
//
// 为什么非钉输出不可:归一化只有一条失败路径是危险的 —— "原样透传"。一旦透传,
// Go 的 regexp.CompilePOSIX 会把类内的 `\d` 当**数字类**,于是 `grep -E '[\d]'` 从
// "匹配反斜杠或字母 d" 变成 "匹配数字"。这是**静默少匹配**:结果看起来就像
// "真的没有匹配",而且在语料上不一定露头 —— 只有当输入里恰好同时有数字和反斜杠时
// 才看得出差别。golden 语料(钉结果)在这种情况下会**漏**,这条不会。
//
// 期望值不是推理出来的,是把当前实现跑一遍打印出来的(见提交说明)。所以它钉的是
// **已经验证过与真 grep 一致的行为**,不是"我以为它该怎么写"。
func TestNormalizeEreClassesExactOutput(t *testing.T) {
	cases := []struct {
		in, want string
		why      string
	}{
		// --- 类内的单个 `\`:必须变成字面反斜杠(这是 §3.2 的核心) ---
		{`[\d]`, `[\\d]`, "类内的 \\ 必须改写成字面反斜杠;原样透传 = Go 当数字类 = 静默少匹配"},
		{`[\w]`, `[\\w]`, "同上"},
		{`[a\-z]`, `[a\\-z]`, "类内的 \\- 同理"},
		{`[\[]`, `[\\[]`, "类内的 \\[ 同理"},
		{`[\d-x]`, `[\\d-x]`, "`-` 在类内是字面量,不影响改写"},
		{`[^\d]`, `[^\\d]`, "类首的 ^ 原样抄,里面的 \\d 仍要改写"},

		// --- `[:name:]` 子结构:它的收尾 `]` 不结束类(§3.2 的另一个面) ---
		{`[[:alpha:]\d]`, `[[:alpha:]\\d]`, "不认 [:name:] 就会在这里把 \\d 当类外,于是从'误拒'变成'透传'"},
		{`[[:digit:]\w]`, `[[:digit:]\\w]`, "同上"},
		{`[[:alpha:]]`, `[[:alpha:]]`, "没有类内反斜杠时,could 完全不动"},

		// --- 类首的字面量 `]`:它不结束类 ---
		{`[]\d]`, `[]\\d]`, "类首的 ] 是字面量,后面的 \\d 仍在类内"},
		{`[^]\d]`, `[^]\\d]`, "`[^]` 开头的字面量 ] 同理"},

		// --- 已经转义的 `\\`:不能再转一次(上一轮踩过的边界) ---
		{`[\\d]`, `[\\d]`, "已经转义的 \\\\ 再转一次会报 invalid escape sequence"},
		{`[\\]`, `[\\]`, "再转一次会变成未闭合的 [\\\\\\]"},

		// --- 类外:一概不动 ---
		{`\d`, `\d`, "类外的 \\d 由 firstPerlClass 负责拒收,不是这里改写"},
		{`\.`, `\.`, "类外的 \\. 是有意义的转义,不能碰"},
		{`a\.b`, `a\.b`, "同上"},
		{`[*]`, `[*]`, "没有反斜杠就不动"},
	}

	for _, c := range cases {
		if got := normalizeEreClasses(c.in); got != c.want {
			t.Errorf("normalizeEreClasses(%q) = %q, 期望 %q\n  为什么:%s", c.in, got, c.want, c.why)
		}
	}
}

// TestPerlShorthandRejectionIsBidirectional 钉住拒收的**两个方向**。
//
// 修 §3.2 时,`firstPerlClass` 从"自己维护 inClass"改成"交给 classEnd 跳过整个字符类"。
// 这一步很容易**改过头**:只要跳过逻辑写错(比如把 `[` 也当成类外字符),类外的 `\d`
// 就会同时被放过 —— 那就是从"响亮报错"变成"静默少匹配"的正门。
//
// 所以两个方向都要钉:
//
//	该拒的仍然拒(类外的 Perl 简写、Perl 内联开关)
//	该放的仍然放(类内的 `\d`,包括 `[:name:]` 之后的那种)
func TestPerlShorthandRejectionIsBidirectional(t *testing.T) {
	t.Run("该拒的仍然拒", func(t *testing.T) {
		rejected := map[string]string{
			`\d+`:     `\d`,
			`\w`:      `\w`,
			`\s`:      `\s`,
			`[a-z]\d`: `\d`, // 类**之后**的 `\d` 是类外
			`(?i)abc`: `(?`,
		}
		for pattern, wantFragment := range rejected {
			m, problem, _ := compileGrep([]string{pattern}, &grepOpts{extended: true})
			if problem == "" {
				t.Errorf("compileGrep(%q) 被接受了;它是类外的 Perl 语法,必须拒收 —— 放过去就是'静默少匹配'的正门", pattern)
				continue
			}
			if m != nil {
				t.Errorf("compileGrep(%q) 拒收时不该同时返回匹配器", pattern)
			}
			if !contains(problem, wantFragment) {
				t.Errorf("compileGrep(%q) 的报错没点出 %q: %s", pattern, wantFragment, problem)
			}
		}
	})

	t.Run("该放的仍然放", func(t *testing.T) {
		accepted := []string{
			`[\d]`,          // 类内的 `\d`
			`[[:alpha:]\d]`, // `[:name:]` 之后仍在类内(§3.2 修的那条)
			`[[:digit:]\w]`, // 同上,换成 `\w`
			`[]\d]`,         // 类首字面量 `]` 之后仍在类内
			`[^\d]`,         // `[^` 之后
			`a*?`,           // ERE 里 `*?` 是合法的 —— 见 README 已知差异(vs Perl 的懒惰量词)
		}
		for _, pattern := range accepted {
			if _, problem, remedy := compileGrep([]string{pattern}, &grepOpts{extended: true}); problem != "" {
				t.Errorf("compileGrep(%q) 被拒收了;它是合法 ERE\n  问题: %s\n  怎么办: %s", pattern, problem, remedy)
			}
		}
	})
}

// TestClassInRegexMeansLiteralBackslash 端到端:类内的 `\d` 必须匹配**反斜杠**。
//
// 这条是前两条的"后果"版本:如果有人既改了 normalizeEreClasses 又改了拒收逻辑,
// 前两条会指出"哪个动作错了";这条指出"用户看到的是什么"。三条一起才形成闭环 ——
// 一条盯动作、一条盯闸门、一条盯后果。
func TestClassInRegexMeansLiteralBackslash(t *testing.T) {
	dir := t.TempDir()
	writeGrepClassCorpus(t, dir)

	cases := []struct {
		pattern string
		want    []string
	}{
		{`[\d]`, []string{`\`, `d`}},
		{`[[:alpha:]\d]`, []string{`\`, `d`, `a`, `z`, `b`, `w`}},
	}
	for _, c := range cases {
		var out, errw bytes.Buffer
		code := Grep(context.Background(), dir, []string{"-E", c.pattern, "chars.txt"},
			strings.NewReader(""), &out, &errw)
		if code != 0 {
			t.Errorf("grep -E %q exit=%d, stderr=%s", c.pattern, code, errw.String())
			continue
		}
		got := grepClassLines(out.String())
		if !slices.Equal(got, c.want) {
			t.Errorf("grep -E %q 匹配到 %q, 期望 %q\n  这条要是挂了,说明类内的 \\ 语义变了 —— 真 grep 给的是同一组", c.pattern, got, c.want)
		}
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
