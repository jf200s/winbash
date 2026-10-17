package builtins

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"winbash/internal/errs"
)

// Grep 实现 bash 风格的 grep。
//
// 与其它 ① 层命令不同的是它多接一个 in io.Reader:grep 的输入本来就是管道,
// 而 interp 的管道是进程内的 —— 从 HandlerCtx 取 stdin 直接读,不必落盘也不必 spawn。
// 无文件名时读 in;有文件名时逐个读;`-` 作为文件名显式表示 in。
// (cat 没有这个入口,因为它的签名没有 in —— 那是 Task 5 的既定形状,先记着。)
//
// 方言是这里最要紧的决策(见 compileGrep):默认按字面匹配,`-E` 走 POSIX ERE,
// 两者都不接受 Perl 简写。理由是「不翻车」:`grep -E '\d+'` 在真 grep 里匹配不到数字,
// 如果 winbash 悄悄用 Perl 语义让它匹配了,使用者会得到和真实环境不同的结果,
// 而且是在没人告诉他的情况下。
func Grep(ctx context.Context, cwd string, argv []string, in io.Reader, out, errw io.Writer) int {
	opts, pattern, files, aerr := parseGrepArgs(argv)
	if aerr != nil {
		remedy := "用法: grep [-i -n -v -c -l -r -E -F -q] PATTERN [文件...]"
		if ue, ok := aerr.(*usageErr); ok {
			// 带针对性指路的错误(例如 GNU 的 --include):通用那句「用法:」对它
			// 没有任何帮助,使用者还得再花一轮问"那该怎么写"。
			remedy = ue.remedy
		}
		fmt.Fprintln(errw, errs.Errorf("grep", aerr.Error(), remedy))
		return errs.ExitUsage
	}

	// -q:只要退出码,不要输出。用 io.Discard 顶掉输出端,而不是在每个打印点加判断 ——
	// 这样 -q 与 -c/-l/-n 叠加时的行为自动与 bash 一致(静默)。
	//
	// 为什么值得单列一条:`grep -q` 是 AI 做条件判断的标准写法
	// (`if grep -q x f; then`),而我们原来把它当"不认识的参数"报 exit 2 ——
	// exit 2 在 if 里既不是真也不是假,分支直接走错,而且是静默的。
	// 来源:oils spec `spec/vars-special.test.sh`(`echo $PWD | grep -q /`)。
	if opts.quiet {
		out = io.Discard
	}

	re, problem, remedy := compileGrep(pattern, opts)
	if problem != "" {
		fmt.Fprintln(errw, errs.Errorf("grep", problem, remedy))
		return errs.ExitUsage
	}

	// 多文件或递归时行前带文件名 —— 与 bash 一致:少了前缀,结果就没法归属到文件。
	showName := len(files) > 1 || opts.recursive

	anyMatch := false
	code := errs.ExitOK
	report := func(err error, remedy string) {
		fmt.Fprintln(errw, errs.Errorf("grep", err.Error(), remedy))
		code = errs.ExitError
	}

	if len(files) == 0 {
		res, binary, ierr := grepStream(in, re, opts, "", showName, out)
		if ierr != nil {
			report(ierr, "读输入时出错")
		}
		if binary && !opts.namesOnly && !opts.quiet {
			noteBinary(errw, "")
		}
		anyMatch = res.matched
	} else {
		for _, f := range files {
			if f == "-" {
				// `-` 显式表示标准输入。与无文件名时的区别是:这时它是「文件列表里的一员」,
				// 多文件输出要给出行归属 —— bash 用的名字就是 "(standard input)"。
				res, binary, ierr := grepStream(in, re, opts, "(standard input)", showName, out)
				if ierr != nil {
					report(ierr, "读标准输入时出错")
				}
				if binary && !opts.namesOnly && !opts.quiet {
					noteBinary(errw, "(standard input)")
				}
				anyMatch = anyMatch || res.matched
				continue
			}

			abs, rerr := ResolvePath(cwd, f)
			if rerr != nil {
				report(rerr, "检查路径写法: "+f)
				continue
			}

			info, serr := os.Stat(abs)
			if serr != nil {
				fmt.Fprintln(errw, errs.Errorf("grep",
					userPathProblem("cannot access", f, serr, abs),
					"先确认路径存在,或用 winbash -c 'ls 目录' 看有哪些名字"))
				code = errs.ExitError
				continue
			}

			if info.IsDir() {
				if !opts.recursive {
					// bash 在这里只留一句 "Is a directory",使用者不知道下一步。
					// 明确说出该加什么开关,是 ① 层「不翻车」的一部分。
					report(fmt.Errorf("%s 是目录", f), "递归搜索目录请加 -r: grep -r PATTERN "+f)
					continue
				}
				anyMatch = grepTree(abs, f, re, opts, out, errw, report) || anyMatch
				continue
			}

			m, ok := grepFile(abs, re, opts, f, showName, out, errw, report)
			anyMatch = anyMatch || (ok && m)
		}
	}

	if anyMatch {
		return errs.ExitOK
	}
	if code != errs.ExitOK {
		return code
	}
	// 没有命中任何一行:grep 的惯例是 1,而不是 0 —— AI 靠这个判断分支。
	return errs.ExitError
}

// grepOpts 是解析后的开关集合。
type grepOpts struct {
	ignoreCase bool
	lineNumber bool
	invert     bool
	count      bool
	namesOnly  bool
	recursive  bool
	extended   bool
	quiet      bool
}

// parseGrepArgs 吃开关,返回 opts、pattern 与文件名列表。
//
// 开关解析在第一个非开关参数处停止 —— 之后的都是 pattern 和文件名。
// pattern 以 `-` 开头时必须用 `--` 分隔(bash 惯例),这样 `grep -- -x f` 才有解。
func parseGrepArgs(argv []string) (*grepOpts, string, []string, error) {
	opts := &grepOpts{}
	i := 0
	sawDashDash := false
	for ; i < len(argv); i++ {
		a := argv[i]
		if a == "--" {
			i++
			sawDashDash = true
			break
		}
		if len(a) < 2 || a[0] != '-' {
			break
		}
		// 长开关整体报出。逐字符解析会把 `--nope` 报成 `--`,
		// 使用者拿着这个提示定位不到问题(ls 那边踩过同一个坑)。
		if strings.HasPrefix(a, "--") {
			return nil, "", nil, fmt.Errorf("不认识的参数: %s", a)
		}
		// 合并写法 -iE / -in 是 bash 惯例,AI 也常写。
		for _, c := range a[1:] {
			switch c {
			case 'i':
				opts.ignoreCase = true
			case 'n':
				opts.lineNumber = true
			case 'v':
				opts.invert = true
			case 'c':
				opts.count = true
			case 'l':
				opts.namesOnly = true
			case 'r', 'R':
				opts.recursive = true
			case 'E':
				opts.extended = true
			case 'F':
				// -F 是"按字面匹配",而这**本来就是默认方言**(见 compileGrep:
				// 非 -E 分支用 QuoteMeta)。原先这里还设了一个 opts.fixed 字段,
				// 但全包没有任何地方读它 —— 那个字段只是让"支持 -F"看起来有实现。
				// 保留这个 case 有两个理由:-F 必须被接受;它还要把 -E 顶掉
				// (`grep -EF` 按字面,与 bash 一致:后出现的那个胜)。
				opts.extended = false
			case 'q':
				opts.quiet = true
			default:
				// 不认识的开关必须报错:静默忽略会让使用者以为它生效了。
				return nil, "", nil, fmt.Errorf("不认识的参数: -%c", c)
			}
		}
	}

	rest := argv[i:]
	if len(rest) == 0 {
		return nil, "", nil, fmt.Errorf("没有指定 pattern")
	}
	// 选项写在 PATTERN **之后**:bash 的 getopt 会做置换(permutation)把它挪到前面,
	// 我们不做置换 —— 于是 `--include` 会被当成**文件名**。实测的后果比"报错"更坏:
	//
	//	grep -rn x --include="*.go" .
	//	→ stderr 一句"路径不存在",stdout 却把**全部**文件都搜了,而且 exit 0
	//
	// 过滤没生效、退出码还是成功 —— 这正是本项目要消灭的"静默做错事"。所以这里
	// 必须响亮拒绝,并把"该改用哪种写法"直接说清楚(见 afterPatternOptionErr)。
	// `--` 之后的 `-` 开头参数是**文件名**,不在此列。
	if !sawDashDash {
		for _, f := range rest[1:] {
			// 单独的 `-` 是「读 stdin」的惯用文件名(grep x a.txt -),不是选项。
			if f != "-" && strings.HasPrefix(f, "-") {
				return nil, "", nil, afterPatternOptionErr(f)
			}
		}
	}
	return opts, rest[0], rest[1:], nil
}

// usageErr 是一个**带针对性指路**的参数错误。
//
// 普通参数错误配上通用的「用法: grep [...]」就够了;但 GNU 的 `--include` 这类
// 选项需要说清"改用哪种写法",否则使用者只知道"不支持",还得再花一轮问怎么替代。
type usageErr struct {
	problem string
	remedy  string
}

func (e *usageErr) Error() string { return e.problem }

// afterPatternOptionErr 给"选项写在 PATTERN 之后"这一种错误一句能省一轮的指路。
func afterPatternOptionErr(opt string) error {
	name, _, _ := strings.Cut(opt, "=")
	switch name {
	case "--include", "--exclude", "--include-dir", "--exclude-dir":
		return &usageErr{
			problem: name + " 是 GNU grep 的选项,① 层的 grep 不支持;" +
				"而且它写在 PATTERN 之后 —— 我们不做选项置换,只会把它当文件名",
			remedy: "按文件名过滤改用: find . -name '*.go' -exec grep -n PATTERN {} + ;" +
				"需要 GNU 的完整方言就显式调真 grep:(Git 安装根)\\usr\\bin\\grep.exe -rn PATTERN --include='*.go' .",
		}
	}
	return &usageErr{
		problem: "不认识的参数: " + opt + "(它写在 PATTERN 之后,我们不做选项置换)",
		remedy: "选项要写在 PATTERN **之前**;真要搜以 - 开头的**文件名**,把 -- 也写在 PATTERN 之前: " +
			"grep -n -- PATTERN -weird-name",
	}
}

// breToEre 把 POSIX BRE(grep 的默认方言)翻译成 ERE,好交给 CompilePOSIX。
//
// 为什么需要它:这里原本用 QuoteMeta 把整个 pattern 当**字面**,注释还写着
// 「与 BRE 在 AI 常用场景下等价」—— 实测是错的。真 grep 默认是 BRE,而
// `. * [ ] ^ $ \` 在 BRE 里**全是元字符**,于是下面这些高频写法全部**静默给错值**
// (同一份 `abc\nabbbc\nxyz\n` 输入,`grep -c` 的结果):
//
//	pattern   winbash(旧)   真 grep
//	^abc      0             1
//	abc$      0             1
//	a.c       0             1
//	ab*c      0             2
//	[ab]      0             2
//
// `grep -q '^foo' file` 是最常见的一类判断,静默不匹配会让 if 直接走错分支 ——
// 比报错坏得多,也正是本项目底线要消灭的形态。
//
// 两边真正的差别只有下面这几对,逐字符翻过去即可:
//
//	BRE \( \) \{ \} \+ \? \|   → ERE ( ) { } + ? |           分组 / 重复 / 交替
//	BRE 裸 ( ) { } + ? |       → ERE \( \) \{ \} \+ \? \|     它们在 BRE 里是**字面**
//	BRE \X(X 是 . * [ ] ^ $ \) → ERE \X                      两边都要转义才是字面
//	BRE \X(X 是普通字符)       → ERE \\X                     沿用既有决策:\d 找字面 "\d"
//	BRE 裸 . * ^ $ / […] 类     → 原样                        两边语义一致
//
// 最后一条里那个"字面 `\d`"是**刻意**的、与 GNU grep 不同(GNU 会忽略反斜杠只匹配
// `d`):本项目选择让 `grep '\d'` 报出它找的是哪两个字符,而不是悄悄退化成 Perl 简写。
// 这条差异记在 README 的已知差异表里。
//
// 字符类内部两边规则相同(类内的 `\` 只是普通字符),所以整段照抄 —— 交给
// normalizeEreClasses 去修 Go 不认类内 `\` 的问题,边界由 classEnd 判定(唯一实现)。
func breToEre(pattern string) string {
	var b strings.Builder
	b.Grow(len(pattern) + 8)
	for i := 0; i < len(pattern); {
		switch c := pattern[i]; c {
		case '\\':
			if i+1 >= len(pattern) {
				// 孤立的反斜杠留在末尾:转成 `\\`(字面反斜杠)而不是让 CompilePOSIX
				// 抛一句没有上下文的语法错。
				b.WriteString(`\\`)
				i++
				continue
			}
			n := pattern[i+1]
			switch {
			case strings.IndexByte("(){}+?|", n) >= 0:
				// BRE 的元字符:ERE 里直接写。
				b.WriteByte(n)
			case strings.IndexByte(`.*[]^$\`, n) >= 0:
				// 两边都要转义才是字面(\. \* \[ \] \^ \$ \\)。
				b.WriteByte('\\')
				b.WriteByte(n)
			default:
				// 既有决策:`\d` 找的是**字面**的 `\d` 两个字符。
				b.WriteString(`\\`)
				b.WriteByte(n)
			}
			i += 2
		case '[':
			// 字符类整段照抄,边界交给 classEnd(POSIX 类规则唯一那份实现)。
			end := classEnd(pattern, i)
			b.WriteString(pattern[i:end])
			i = end
		case '(', ')', '{', '}', '+', '?', '|':
			// 这些在 BRE 里是**字面**,进 ERE 必须转义。
			b.WriteByte('\\')
			b.WriteByte(c)
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// compileGrep 按方言决策编译 pattern,失败时返回「问题 + 怎么办」两行文案。
//
// 默认(无 -E):先把 BRE 翻成 ERE(见 breToEre),再走与 -E 相同的路径。
//
// -E:走 regexp.CompilePOSIX,即 POSIX ERE,最接近 `grep -E`。
// Perl 简写(\d \w \s \D \W \S)在这里没有定义,先显式拒收再编译 ——
// 实测 `regexp.CompilePOSIX("\d+")` 确实会报 invalid escape sequence,
// 但那条信息不会告诉使用者「你写的是 Perl 方言」,更不会给出替代写法。
func compileGrep(pattern string, opts *grepOpts) (re *regexp.Regexp, problem, remedy string) {
	if !opts.extended {
		// 默认方言 = 真 grep 的 **BRE**。翻译成 ERE 之后走与 -E 完全相同的编译
		// 路径:两种方言的差别只在翻译层,后面的方言校验、-i、leftmost-longest
		// 因此只有一份实现(见 breToEre 里那张对照表)。
		pattern = breToEre(pattern)
	}

	if cls, ok := firstPerlClass(pattern); ok {
		return nil, "-E 使用 POSIX ERE 方言," + cls + " 属于 Perl 简写,在这里没有定义",
			"改用 POSIX 写法: [0-9] / [[:digit:]] / [[:alpha:]] / [[:space:]]"
	}

	// `(?...)` 内联开关在 ERE 里没有定义。Perl 引擎会接受它,放过去等于默默换了
	// 方言,所以先显式挡掉 —— 这是下面那条回退路径唯一的例外。
	if strings.Contains(pattern, "(?") {
		return nil, "模式里有 (?...) 内联开关,那是 Perl 语法,POSIX ERE 里没有定义",
			"-E 的方言是 POSIX ERE;要忽略大小写用 -i,不要把 (?i) 写进模式"
	}

	// 先用 POSIX 模式校验方言,把 `*?`、`\b` 这类 Perl 语法挡在门外,
	// 而不是留给 Perl 引擎编译时「恰好也能过」。
	//
	// 归一化是必须的:POSIX ERE 里,字符类**内部**的 `\` 不是转义引导符 ——
	// `[\d]` 的含义是「反斜杠或字母 d」,与 `\d` 这种 Perl 简写毫无关系。而 Go 的
	// regexp.CompilePOSIX 不认字符类里的 `\`,直接报 invalid escape sequence,
	// 于是 `grep -E '[\d]'` 这种完全合法的写法在我们这里变成语法错(真 bash 给
	// exit 1:无匹配)。normalizeEreClasses 把它改写成 `[\\d]`,保留 ERE 的原意。
	//
	// 曾经试过"编译失败就回退到 Perl 引擎 + Longest()",那更糟:Perl 把 `[\w]`
	// 当成词字符,于是它匹配 `b`,而真 bash 不匹配 —— 语义**静默变了**,只有
	// 内容里正好有词字符时才显形。精确改写比宽泛回退好。
	normalized := normalizeEreClasses(pattern)
	if posixErr := func() error { _, err := regexp.CompilePOSIX(normalized); return err }(); posixErr != nil {
		return nil, "模式不是合法的 POSIX ERE: " + posixErr.Error(),
			`要按字面匹配特殊字符就转义它(\\. ),字符类用 [[:alpha:]] 这种 POSIX 写法`
	}

	if opts.ignoreCase {
		// 实测:Go 的 POSIX 模式不支持内联开关(`(?i)alpha` 报 missing argument),
		// 所以 -i 与 -E 只能叠加成「POSIX 校验 + Perl 引擎 + Longest()」:
		// 方言已由上面钉死,Longest() 再把匹配语义还原成 leftmost-longest。
		r, err := regexp.Compile("(?i)" + normalized)
		if err != nil {
			return nil, "模式无法编译: " + err.Error(), "检查模式里的括号与转义"
		}
		r.Longest()
		return r, "", ""
	}

	r, err := regexp.CompilePOSIX(normalized)
	if err != nil {
		return nil, "模式不是合法的 POSIX ERE: " + err.Error(), "检查模式里的括号与转义"
	}
	return r, "", ""
}

// classEnd 返回从 start(必须指向 `[`)开始的字符类的**闭合 `]` 之后**那个下标。
// 没有闭合时返回 len(pattern)。
//
// 这是字符类规则的**唯一一份实现**。它认三条 POSIX 规则:
//
//	`[` 或 `[^` **紧跟**的 `]` 是字面量,不结束字符类(`[]]` 是"含 `]` 的类")
//	`[:name:]` / `[=ch=]` / `[.ch.]` 是类内的括号子结构,它的 `]` 不结束字符类
//	(以及)真正的结束是第一个不落在上面两种情况里的 `]`
//
// 为什么必须只有一份:原先 normalizeEreClasses 与 firstPerlClass 各写一套,规则不一致 ——
// 前者认字面量 `]`、后者也认,但**两者都不认 `[:name:]`**。于是 `[[:alpha:]\d]` 里
// `[:alpha:]` 的收尾 `]` 被当成类的结束,`\d` 就被判成**类外**的 Perl 简写而拒收 ——
// 而真 grep 给 `\dazbw`(exit 0)。修这一处必须**两个函数同时改**:只放开拒收的话,
// 类内的 `\d` 会被 Go 当成数字类,变成"静默少匹配",比响亮报错更坏。
func classEnd(pattern string, start int) int {
	i := start + 1
	if i < len(pattern) && pattern[i] == '^' {
		i++
	}
	if i < len(pattern) && pattern[i] == ']' {
		i++ // 类首的字面量 `]`
	}
	for i < len(pattern) {
		switch pattern[i] {
		case '\\':
			i += 2 // 成对消费:转义的那个字符不再单独看
			continue
		case '[':
			// `[:name:]` / `[=ch=]` / `[.ch.]` —— 找配对的 `<mark>]`。
			if i+1 < len(pattern) && strings.ContainsRune(":=.", rune(pattern[i+1])) {
				mark := pattern[i+1]
				j := i + 2
				for j+1 < len(pattern) && !(pattern[j] == mark && pattern[j+1] == ']') {
					j++
				}
				if j+1 < len(pattern) {
					i = j + 2
					continue
				}
			}
		case ']':
			return i + 1
		}
		i++
	}
	return len(pattern)
}

// normalizeEreClasses 把字符类**内部**的 `\X` 改写成 ERE 该有的样子。
//
// POSIX ERE 里字符类内部的 `\` 只是个普通字符,所以 `[\d]` 等价于「反斜杠或 d」。
// 真 Git Bash 也是如此 —— 实测 `grep -E '[\d]'` 在只含 `\ d ] a z - b w [` 的语料上,
// 输出正是 `\` 与 `d`(两边逐字相同)。而 Go 的 regexp.CompilePOSIX **不认**类内的 `\`,
// 直接报 invalid escape sequence。所以把单个 `\X` 写成 `\\X`(字面反斜杠 + X):
// 保留 ERE 原意,同时让 POSIX 引擎能编译。
//
// 边界用 classEnd 定(类首字面量 `]`、`[:name:]` 收尾的 `]`、已转义的 `\\`),这里只负责
// **类内**的改写 —— 类外一概不动。曾经的写法自己维护 `inClass`,于是 `[:alpha:]` 的收尾
// `]` 被误当成类的结束,`\d` 落到"类外"分支原样透传给了 POSIX 引擎(静默少匹配)。
func normalizeEreClasses(pattern string) string {
	var b strings.Builder
	for i := 0; i < len(pattern); {
		if pattern[i] != '[' {
			// 类外:原样抄。`\.` 这类转义在类外是有意义的,不能碰。
			if pattern[i] == '\\' && i+1 < len(pattern) {
				b.WriteByte(pattern[i])
				b.WriteByte(pattern[i+1])
				i += 2
				continue
			}
			b.WriteByte(pattern[i])
			i++
			continue
		}
		end := classEnd(pattern, i)
		if end <= i {
			end = i + 1 // 防御:不该发生
		}
		b.WriteString(normalizeClassBody(pattern[i:end]))
		i = end
	}
	return b.String()
}

// normalizeClassBody 改写一个**完整**字符类(含首尾方括号)内部的 `\X`。
func normalizeClassBody(body string) string {
	var b strings.Builder
	// 类首的 `^` 与字面量 `]` 原样抄过去,它们不参与 `\X` 处理。
	head := 1
	if head < len(body) && body[head] == '^' {
		head++
	}
	if head < len(body) && body[head] == ']' {
		head++
	}
	b.WriteString(body[:head])
	for i := head; i < len(body); {
		if body[i] == '\\' && i+1 < len(body) {
			if body[i+1] == '\\' {
				b.WriteString(`\\`) // 已经是转义的转义,原样保留
			} else {
				b.WriteString(`\\`) // 类内的 `\X` = 字面反斜杠 + X
				b.WriteByte(body[i+1])
			}
			i += 2
			continue
		}
		b.WriteByte(body[i])
		i++
	}
	return b.String()
}

// firstPerlClass 找出 pattern 里第一个 Perl 简写类,返回它的字面写法。
//
// 两条规则:
//
//   - 成对跳过转义:类外的 `\\d` 是「字面反斜杠 + d」,不是 Perl 类。
//   - **不进字符类内部**:`[\d]`、`[[:alpha:]\d]` 在 POSIX ERE 里都合法,类内的 `\` 只是
//     普通字符。整个字符类交给 classEnd 一次跳过 —— 早先自己维护 `inClass` 且不认
//     `[:name:]`,于是 `[[:alpha:]\d]` 里 `[:alpha:]` 的收尾 `]` 被当成类结束,`\d`
//     落到"类外"而拒收了一整段合法 ERE(真 grep 给 `\dazbw`,exit 0)。
func firstPerlClass(pattern string) (string, bool) {
	for i := 0; i+1 < len(pattern); i++ {
		switch pattern[i] {
		case '\\':
			switch pattern[i+1] {
			case 'd', 'D', 'w', 'W', 's', 'S':
				return pattern[i : i+2], true
			}
			i++ // 跳过被转义的那个字符
		case '[':
			i = classEnd(pattern, i) - 1 // 整个字符类跳过去(含 `[:alpha:]`)
		}
	}
	return "", false
}

// grepResult 是一次输入的统计。
type grepResult struct {
	matched bool // 是否有任何一行符合条件(含 -v 反转后的结果)
	lines   int  // 符合条件的行数
}

// grepStream 逐行扫描一个输入。
//
// 用 bufio.Reader.ReadBytes 而不是 bufio.Scanner:Scanner 有 64KB 的行长上限,
// 超长会静默截断或以 ErrTooLong 中止 —— 对一个「值必须无歧义」的工具来说,
// 静默截断是最不能接受的失败形态。
//
// 行尾只剥 \n,不剥 \r:CRLF 文件在 Git Bash 的 grep 下也会把 \r 带进匹配与输出,
// 这里保持一致,而不是替使用者做「这应该是文本行」的假设。
//
// binary 这第三个返回值见 noteBinary:它只用于**提示**,不影响输出内容。
func grepStream(r io.Reader, re *regexp.Regexp, opts *grepOpts, label string, showName bool, out io.Writer) (grepResult, bool, error) {
	var res grepResult
	binary := false
	br := bufio.NewReaderSize(SafeStdin(r), 64*1024)
	lineNo := 0

	for {
		raw, err := br.ReadBytes('\n')

		if len(raw) > 0 {
			lineNo++
			content := raw
			if content[len(content)-1] == '\n' {
				content = content[:len(content)-1]
			}

			hit := re.Match(content)
			if opts.invert {
				hit = !hit
			}

			if hit {
				// 匹配到了,而且这行里有 NUL —— 说明"命中"可能只是二进制噪声
				// (例如压缩包、可执行文件、UTF-16 文本)。**输出照旧**(见 noteBinary)。
				if bytes.IndexByte(content, 0) >= 0 {
					binary = true
				}
				res.matched = true
				res.lines++
				if opts.namesOnly {
					// -l 每个文件只报一次,不必扫完。
					fmt.Fprintln(out, nameForListing(label))
					return res, binary, nil
				}
				if !opts.count {
					writeGrepLine(out, label, showName, lineNo, opts, content)
				}
			}
		}

		if err != nil {
			if err == io.EOF {
				break
			}
			return res, binary, err
		}
	}

	if opts.count {
		writeGrepCount(out, label, showName, res.lines)
	}
	return res, binary, nil
}

// noteBinary 说明"这个文件里有匹配行含 NUL"。
//
// 为什么只提示、不改写输出:真 GNU grep 检测到 NUL 会切到二进制模式,把内容压成一行
// `Binary file X matches`。但那套判定是 **libc 内部 buffer 策略**(实测:一个 45KB
// 的文件,前 40000 字节是纯文本匹配行、后面才有 NUL,GNU 照样判它 binary)——
// 依赖那个边界等于给使用者一个"像 GNU 但边界不同"的假预期。
//
// 更有害的是把 NUL 与 0xFF 直接吐给下游:对面向 AI 的 shell 来说那是污染 token、
// 破坏解析。所以两头都不要走极端:**输出不动**(规则可解释、行号可复现,`grep -c`
// 与真 grep 逐字一致),但**不再静默** —— 匹配行里有 NUL 就往 stderr 说一句,
// 让使用者知道"你刚才拿到的可能不是文本"。
//
// 默认参数下真 grep 不会打印内容,所以这条在 stdout 上与真 grep 不同 ——
// README 的「已知差异」表里记着这一条。
func noteBinary(errw io.Writer, label string) {
	name := label
	if name == "" {
		name = "(standard input)"
	}
	fmt.Fprintln(errw, errs.Errorf("grep",
		"匹配行里有 NUL 字节("+name+" 是二进制文件)",
		"真 grep 在这里只打一行 `Binary file ... matches`;我们保持逐行输出以便定位与计数。"+
			"要跳过二进制文件请用 `grep -l` 先看是哪些,或先用 `file`/`head -c 16 | od -c` 确认内容类型"))
}

// nameForListing 给出 -l 要打印的名字。从标准输入读时 bash 打印 "(standard input)"。
func nameForListing(label string) string {
	if label == "" {
		return "(standard input)"
	}
	return label
}

func writeGrepLine(out io.Writer, label string, showName bool, lineNo int, opts *grepOpts, content []byte) {
	if showName && label != "" {
		io.WriteString(out, label+":")
	}
	if opts.lineNumber {
		fmt.Fprintf(out, "%d:", lineNo)
	}
	out.Write(content)
	io.WriteString(out, "\n")
}

func writeGrepCount(out io.Writer, label string, showName bool, n int) {
	if showName && label != "" {
		fmt.Fprintf(out, "%s:%d\n", label, n)
		return
	}
	fmt.Fprintf(out, "%d\n", n)
}

// grepFile 处理一个文件,返回 (是否命中, 是否有结论)。
//
// 第二个返回值为 false 表示这个文件根本没读成(已被 report 报了错),
// 调用方不能把它当「没命中」——否则退出码会从「错误」退化成「没找到」。
func grepFile(abs string, re *regexp.Regexp, opts *grepOpts, label string, showName bool, out io.Writer, errw io.Writer, report func(error, string)) (matched, ok bool) {
	f, err := os.Open(abs)
	if err != nil {
		report(err, "先确认文件存在,或用 winbash -c 'ls 目录' 看有哪些名字")
		return false, false
	}
	defer f.Close()

	res, binary, err := grepStream(f, re, opts, label, showName, out)
	if err != nil {
		report(err, "读 "+label+" 时出错")
		return res.matched, false
	}
	// -l 与 -q 的输出本来就只是"名字"或"什么都没有",它们不吐内容 —— 不必提示。
	if binary && !opts.namesOnly && !opts.quiet {
		noteBinary(errw, label)
	}
	return res.matched, true
}

// grepTree 递归搜一棵子树。
//
// display 是**使用者写下的那个路径**(例如 `.`)。输出里的文件名要基于它算,
// 而不是把 WalkDir 给的绝对路径直接打出去 —— 实测 `grep -r b .` 原本输出
// `/c/Users/…/d.txt:b`,而真 bash 给 `./d.txt:b`。AI 拿到绝对路径后很难
// 与它自己的输入对应起来(它写的是 `.`),这是那种"能用但总会让人怀疑"的差异。
func grepTree(root, display string, re *regexp.Regexp, opts *grepOpts, out io.Writer, errw io.Writer, report func(error, string)) bool {
	anyMatch := false
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			report(werr, "跳过 "+p+",继续搜索其余文件")
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			// 设备文件、命名管道之类先不管:打开它们可能永久阻塞。
			return nil
		}
		// 递归输出里的路径用正斜杠:① 层的文本语义按 bash 来。
		label := filepath.ToSlash(displayRel(display, p, root))
		if matched, ok := grepFile(p, re, opts, label, true, out, errw, report); ok && matched {
			anyMatch = true
		}
		return nil
	})
	if err != nil {
		report(err, "遍历 "+root+" 时出错")
	}
	return anyMatch
}

// displayRel 把 WalkDir 给的绝对路径还原成"使用者视角"的写法。
//
// 还原不了(跨盘符等)就退回绝对路径 —— 给一个准确的怪名字,好过给一个错的短名字。
func displayRel(display, p, root string) string {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return p
	}
	rel = filepath.ToSlash(rel)
	if display == "" || display == "." {
		return "./" + rel
	}
	return strings.TrimSuffix(filepath.ToSlash(display), "/") + "/" + rel
}
