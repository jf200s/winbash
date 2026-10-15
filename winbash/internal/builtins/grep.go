package builtins

import (
	"bufio"
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
		fmt.Fprintln(errw, errs.Errorf("grep", aerr.Error(), "用法: grep [-i -n -v -c -l -r -E -F -q] PATTERN [文件...]"))
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
		res, ierr := grepStream(in, re, opts, "", showName, out)
		if ierr != nil {
			report(ierr, "读输入时出错")
		}
		anyMatch = res.matched
	} else {
		for _, f := range files {
			if f == "-" {
				// `-` 显式表示标准输入。与无文件名时的区别是:这时它是「文件列表里的一员」,
				// 多文件输出要给出行归属 —— bash 用的名字就是 "(standard input)"。
				res, ierr := grepStream(in, re, opts, "(standard input)", showName, out)
				if ierr != nil {
					report(ierr, "读标准输入时出错")
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
				report(serr, "先确认路径存在,或用 winbash -c 'ls 目录' 看有哪些名字")
				continue
			}

			if info.IsDir() {
				if !opts.recursive {
					// bash 在这里只留一句 "Is a directory",使用者不知道下一步。
					// 明确说出该加什么开关,是 ① 层「不翻车」的一部分。
					report(fmt.Errorf("%s 是目录", f), "递归搜索目录请加 -r: grep -r PATTERN "+f)
					continue
				}
				anyMatch = grepTree(abs, f, re, opts, out, report) || anyMatch
				continue
			}

			m, ok := grepFile(abs, re, opts, f, showName, out, report)
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
	for ; i < len(argv); i++ {
		a := argv[i]
		if a == "--" {
			i++
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
	return opts, rest[0], rest[1:], nil
}

// compileGrep 按方言决策编译 pattern,失败时返回「问题 + 怎么办」两行文案。
//
// 默认(无 -E):QuoteMeta 后按字面匹配。这与 grep 的 BRE 在 AI 常用场景下等价 ——
// `grep 'a.c' f` 在真 grep 里也匹配字面 a.c,把点当通配才是意外。
//
// -E:走 regexp.CompilePOSIX,即 POSIX ERE,最接近 `grep -E`。
// Perl 简写(\d \w \s \D \W \S)在这里没有定义,先显式拒收再编译 ——
// 实测 `regexp.CompilePOSIX("\d+")` 确实会报 invalid escape sequence,
// 但那条信息不会告诉使用者「你写的是 Perl 方言」,更不会给出替代写法。
func compileGrep(pattern string, opts *grepOpts) (re *regexp.Regexp, problem, remedy string) {
	if !opts.extended {
		lit := regexp.QuoteMeta(pattern)
		if opts.ignoreCase {
			lit = "(?i)" + lit
		}
		r, err := regexp.Compile(lit)
		if err != nil {
			return nil, "模式无法编译: " + err.Error(), "检查模式里是否有孤立的反斜杠"
		}
		return r, "", ""
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

// normalizeEreClasses 把字符类**内部**的 `\X` 改写成 ERE 该有的样子。
//
// POSIX ERE 里字符类内部的 `\` 只是个普通字符,所以 `[\d]` 等价于「反斜杠或 d」。
// 真 Git Bash 也是如此 —— 实测 `grep -E '[\d]'` 在只含 `\ d ] a z - b w [` 的语料上,
// 输出正是 `\` 与 `d`(两边逐字相同)。而 Go 的 regexp.CompilePOSIX **不认**类内的 `\`,
// 直接报 invalid escape sequence。所以把单个 `\X` 写成 `\\X`(字面反斜杠 + X):
// 保留 ERE 原意,同时让 POSIX 引擎能编译。
//
// 两个边界必须守住,**两个都踩过**:
//
//	`\\` 是**已经转义**的反斜杠,不能再动 —— 否则 `[\\]` 变成 `[\\\]`(未闭合),
//	     `[\\d]` 变成不可编译的形态(实测报 invalid escape sequence,exit 2)。
//	`[` 或 `[^` **紧跟**的 `]` 是字面量,不结束字符类 —— 否则 `[\]]` 被截断成
//	     "反斜杠后跟 ]"的空集合,静默返回无匹配(exit 1,看着像"真的没有匹配")。
//
// 这两条与 firstPerlClass 是同一套字符类规则,那个函数也处理了字面量 `]`。以后若再动
// 字符类扫描,应当抽一个共用的状态机,别在一个包里放两份规则不同的实现。
func normalizeEreClasses(pattern string) string {
	var b strings.Builder
	inClass, head := false, false
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]

		// 类开头允许一个 `^`,以及一个字面量 `]`(紧跟 `[` 或 `[^`)。
		if inClass && head {
			if c == '^' {
				b.WriteByte(c)
				continue // head 仍为 true:后面还可能跟字面量 `]`
			}
			if c == ']' {
				b.WriteByte(c)
				head = false
				continue
			}
			head = false
			// 落到下面的常规处理
		}

		if c == '\\' && i+1 < len(pattern) {
			next := pattern[i+1]
			switch {
			case !inClass:
				b.WriteByte(c)
				b.WriteByte(next)
			case next == '\\':
				b.WriteString(`\\`) // 已经是转义的转义,原样保留
			default:
				b.WriteString(`\\`) // 类内的 `\X` = 字面反斜杠 + X
				b.WriteByte(next)
			}
			i++
			head = false
			continue
		}

		b.WriteByte(c)
		switch {
		case c == '[':
			inClass, head = true, true
		case c == ']':
			inClass, head = false, false
		default:
			head = false
		}
	}
	return b.String()
}

// firstPerlClass 找出 pattern 里第一个 Perl 简写类,返回它的字面写法。
//
// 两处必须小心:
//
//   - 成对跳过转义:`\\d` 是「字面反斜杠 + d」,不是 Perl 类。
//   - **不进字符类内部**:`[\d]` 在 POSIX ERE 里是合法的,它匹配反斜杠或字母 d。
//     原先的扫描器只看反斜杠,于是 `grep -E '[\d]'` 被判成 Perl 简写而拒收 ——
//     一段合法脚本凭空失败(真 bash 给 exit 1:「无匹配」,不是「语法错」)。
func firstPerlClass(pattern string) (string, bool) {
	inClass := false
	for i := 0; i+1 < len(pattern); i++ {
		switch pattern[i] {
		case '\\':
			if !inClass {
				switch pattern[i+1] {
				case 'd', 'D', 'w', 'W', 's', 'S':
					return pattern[i : i+2], true
				}
			}
			i++ // 跳过被转义的那个字符
		case '[':
			if !inClass {
				inClass = true
				// POSIX:紧跟 `[` 或 `[^` 的 `]` 是字面量,不结束字符类。
				if i+1 < len(pattern) && pattern[i+1] == '^' {
					i++
				}
				if i+1 < len(pattern) && pattern[i+1] == ']' {
					i++
				}
			}
		case ']':
			inClass = false
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
func grepStream(r io.Reader, re *regexp.Regexp, opts *grepOpts, label string, showName bool, out io.Writer) (grepResult, error) {
	var res grepResult
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
				res.matched = true
				res.lines++
				if opts.namesOnly {
					// -l 每个文件只报一次,不必扫完。
					fmt.Fprintln(out, nameForListing(label))
					return res, nil
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
			return res, err
		}
	}

	if opts.count {
		writeGrepCount(out, label, showName, res.lines)
	}
	return res, nil
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
func grepFile(abs string, re *regexp.Regexp, opts *grepOpts, label string, showName bool, out io.Writer, report func(error, string)) (matched, ok bool) {
	f, err := os.Open(abs)
	if err != nil {
		report(err, "先确认文件存在,或用 winbash -c 'ls 目录' 看有哪些名字")
		return false, false
	}
	defer f.Close()

	res, err := grepStream(f, re, opts, label, showName, out)
	if err != nil {
		report(err, "读 "+label+" 时出错")
		return res.matched, false
	}
	return res.matched, true
}

// grepTree 递归搜一棵子树。
//
// display 是**使用者写下的那个路径**(例如 `.`)。输出里的文件名要基于它算,
// 而不是把 WalkDir 给的绝对路径直接打出去 —— 实测 `grep -r b .` 原本输出
// `/c/Users/…/d.txt:b`,而真 bash 给 `./d.txt:b`。AI 拿到绝对路径后很难
// 与它自己的输入对应起来(它写的是 `.`),这是那种"能用但总会让人怀疑"的差异。
func grepTree(root, display string, re *regexp.Regexp, opts *grepOpts, out io.Writer, report func(error, string)) bool {
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
		if matched, ok := grepFile(p, re, opts, label, true, out, report); ok && matched {
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
