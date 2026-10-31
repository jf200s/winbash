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
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"winbash/internal/errs"
)

// Grep 实现 bash 风格的 grep。
//
// 与其它 ① 层命令不同的是它多接一个 in io.Reader:grep 的输入本来就是管道,
// 而 interp 的管道是进程内的 —— 从 HandlerCtx 取 stdin 直接读,不必落盘也不必 spawn。
// 无文件名时读 in;有文件名时逐个读;`-` 作为文件名显式表示 in。
// (cat 没有这个入口,因为它的签名没有 in —— 那是 Task 5 的既定形状,先记着。)
//
// 方言是这里最要紧的决策(见 compileGrep):默认是 BRE(真 grep 的默认方言),
// `-E` 走 POSIX ERE,`-F` 按字面匹配;`-E` 与 `-F` 同时给出按 GNU 响亮拒绝
// (conflicting matchers,exit 2)。后两者都不接受 Perl 简写。理由是「不翻车」:
// `grep -E '\d+'` 在真 grep 里匹配不到数字,如果 winbash 悄悄用 Perl 语义让它匹配了,
// 使用者会得到和真实环境不同的结果,而且是在没人告诉他的情况下。
func Grep(ctx context.Context, cwd string, argv []string, in io.Reader, out, errw io.Writer) int {
	opts, patterns, files, aerr := parseGrepArgs(argv)
	if aerr != nil {
		remedy := "用法: grep [-i -n -v -c -l -r -E -F -q -A N -B N -C N -m N -o -w -x -e P] PATTERN [文件...]"
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

	m, problem, remedy := compileGrep(patterns, opts)
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

	// 组间隔线的游标是**整个调用**级的:实测 GNU 给了 -A/-B/-C 之后,`--` 也跨
	// 文件/跨流 —— 上一组在 f1、新组在 f2 时照样补一条(文件列表、stdin 的 `-`、
	// -r 递归都是如此)。所以游标放这里,每条流一个递增的 streamID。
	sep := &grepSep{contextGiven: opts.contextGiven}
	streamID := uint64(0)

	if len(files) == 0 {
		streamID++
		res, binary, ierr := grepStream(in, m, opts, "", showName, out, sep, streamID)
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
				streamID++
				res, binary, ierr := grepStream(in, m, opts, "(standard input)", showName, out, sep, streamID)
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
					userPathProblem("", f, serr, abs),
					"先确认路径存在,或用 winbash -c 'ls 目录' 看有哪些名字"))
				code = errs.ExitError
				continue
			}

			if info.IsDir() {
				if !opts.recursive {
					// 措辞与真 bash 一致(实测 `grep x adir` → `grep: adir: Is a directory`),
					// 但多给一步指路:bash 在这里只留一句 "Is a directory",使用者
					// 不知道下一步。明确说出该加什么开关,是 ① 层「不翻车」的一部分。
					report(fmt.Errorf("%s: Is a directory", f), "递归搜索目录请加 -r: grep -r PATTERN "+f)
					continue
				}
				anyMatch = grepTree(abs, f, m, opts, out, errw, report, sep, &streamID) || anyMatch
				continue
			}

			streamID++
			match, ok := grepFile(abs, m, opts, f, showName, out, errw, report, sep, streamID)
			anyMatch = anyMatch || (ok && match)
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
	extended   bool // -E:POSIX ERE
	fixed      bool // -F:按字面匹配(不再"恰好等于默认方言",见 compileGrep)
	quiet      bool

	onlyMatching bool // -o:只输出命中段
	wordRegexp   bool // -w:命中段必须整个落在词边界内
	wholeLine    bool // -x:模式必须命中**整行**(实测 GNU -x 'x*' 对 abc 不命中、对空行命中)

	// maxCount 是 -m N 的上限,按**选中行**数计(与 -o 无关 —— 实测 -m1 -o 对
	// 一行里三个 a 照样给 3 段)。-1 = 无上限,也是默认值:GNU 对负数同样不设限
	// (strtoumax 把 -1 回绕成极大值,实测 -m -1 给全部命中)。-c 的计数同样封顶;
	// -m 0 时 GNU 连 -c 的计数行都不给(实测无输出、exit 1)。
	maxCount int

	afterN  int // -A:命中行之后跟几行上下文
	beforeN int // -B:命中行之前带几行上下文(-C 两者都设,-A/-B 显式给过的维度优先)
	// contextGiven:-A/-B/-C 是否出现过(哪怕显式给 0)。实测 GNU:完全没给上下文
	// 开关时两组命中之间**不打** `--`;显式 -A0 / -B0 / -C0 却照样打。分组发射
	// 整套逻辑只在该开关组被给过时启用。
	contextGiven bool

	// ePatterns 是 -e 给的模式列表(≥1 时**所有**位置参数都是文件名 —— 实测
	// `grep -e a b f.txt` 里 b 与 f.txt 都是文件,pattern 只有 a)。
	ePatterns []string
}

// parseGrepArgs 吃开关,返回 opts、模式列表与文件名列表。
//
// 开关解析在第一个非开关参数处停止 —— 之后的都是 pattern 和文件名。
// pattern 以 `-` 开头时用 `-e -x`(GNU 惯例)或 `--` 分隔,这样 `grep -e -x f`、
// `grep -- -x f` 才有解。
//
// -E 与 -F 同时给出时按 GNU 响亮拒绝(conflicting matchers,exit 2)—— 实测
// GNU 对 -FE / -EF / -E -F / -F -E 一律如此,静默选一个方言是"静默做错事"。
func parseGrepArgs(argv []string) (*grepOpts, []string, []string, error) {
	opts := &grepOpts{maxCount: -1} // -1 = 没给 -m,即不设上限
	i := 0
	sawDashDash := false
	sawExtended, sawFixed := false, false
	var aVal, bVal, cVal int
	sawA, sawB, sawC := false, false, false
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
			return nil, nil, nil, fmt.Errorf("不认识的参数: %s", a)
		}
		// 合并写法 -iE / -in / -nA2 是 bash 惯例,AI 也常写。
		// 带值的开关(A/B/C/e)吃掉本 token 里剩下的字符,没有就吃下一个参数
		// (实测 -nA2、-nA + 下一参、-e'l3' 三种形态 GNU 都这么收)。
		for j := 1; j < len(a); j++ {
			c := a[j]
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
				sawExtended = true
			case 'F':
				opts.fixed = true
				sawFixed = true
			case 'q':
				opts.quiet = true
			case 'o':
				opts.onlyMatching = true
			case 'w':
				opts.wordRegexp = true
			case 'x':
				opts.wholeLine = true
			case 'm':
				val, ok := takeFlagValue(argv, &i, a, j)
				if !ok {
					return nil, nil, nil, fmt.Errorf("开关 -m 需要一个参数(最大命中数)")
				}
				n, nerr := strconv.Atoi(val)
				if nerr != nil {
					// 措辞对齐 GNU(实测:`grep -m 2x` 与 `-m ''` 都给
					// `grep: invalid max count`、exit 2,消息里不带值)。
					return nil, nil, nil, &usageErr{
						problem: "invalid max count",
						remedy:  "-m 后面要跟非负整数,可以直接附着(-m2)也可以隔开(-m 2);不设上限就不要给 -m",
					}
				}
				if n < 0 {
					// 实测 GNU:-m -1 照常跑、不设上限(strtoumax 把负数回绕成极大值)。
					n = -1
				}
				opts.maxCount = n
			case 'A', 'B', 'C':
				val, ok := takeFlagValue(argv, &i, a, j)
				if !ok {
					return nil, nil, nil, fmt.Errorf("开关 -%c 需要一个参数(上下文行数)", c)
				}
				n, nerr := strconv.Atoi(val)
				if nerr != nil || n < 0 {
					// 措辞对齐 GNU(实测:grep: 2x: invalid context length argument)。
					return nil, nil, nil, &usageErr{
						problem: val + ": invalid context length argument",
						remedy:  "-A / -B / -C 后面要跟非负整数,行数可以直接附着(-A2)也可以隔开(-A 2)",
					}
				}
				switch c {
				case 'A':
					aVal, sawA = n, true
				case 'B':
					bVal, sawB = n, true
				case 'C':
					cVal, sawC = n, true
				}
				opts.contextGiven = true
			case 'e':
				val, ok := takeFlagValue(argv, &i, a, j)
				if !ok {
					return nil, nil, nil, fmt.Errorf("开关 -e 需要一个参数(模式)")
				}
				opts.ePatterns = append(opts.ePatterns, val)
			default:
				// 不认识的开关必须报错:静默忽略会让使用者以为它生效了。
				return nil, nil, nil, fmt.Errorf("不认识的参数: -%c", c)
			}
			if c == 'A' || c == 'B' || c == 'C' || c == 'e' || c == 'm' {
				break // 带值开关吃掉了 token 里剩余的字符(或下一个参数)
			}
		}
	}

	// -A/-B/-C 的合成规则(实测 GNU,与出现顺序无关):-A 定 after、-B 定 before、
	// -C 两者都定;某维度被 -A/-B **显式**给过时,-C 对该维度让位。
	// 实测:-A3 -C1 与 -C1 -A3 都是 after=3/before=1;-C1 -B3 是 before=3/after=1。
	if sawA {
		opts.afterN = aVal
	} else if sawC {
		opts.afterN = cVal
	}
	if sawB {
		opts.beforeN = bVal
	} else if sawC {
		opts.beforeN = cVal
	}

	// 方言仲裁:两个都显式给过才冲突(重复给同一个没问题,实测 -FF、-EE 都合法)。
	if sawExtended && sawFixed {
		return nil, nil, nil, &usageErr{
			problem: "conflicting matchers specified(-E 与 -F 是两种匹配方言)",
			remedy:  "只能选一个:-E 按 POSIX ERE,-F 按字面匹配;egrep / fgrep 本身已分别是 grep -E / grep -F,不要再叠加另一个",
		}
	}

	rest := argv[i:]
	var patterns []string
	var files []string
	if len(opts.ePatterns) > 0 {
		// 实测 GNU:`grep -e a b f.txt` 里 b、f.txt 都是文件名,pattern 只有 a。
		patterns, files = opts.ePatterns, rest
	} else {
		if len(rest) == 0 {
			return nil, nil, nil, fmt.Errorf("没有指定 pattern")
		}
		patterns, files = rest[:1], rest[1:]
	}
	// 选项写在 PATTERN **之后**:bash 的 getopt 会做置换(permutation)把它挪到前面,
	// 我们不做置换 —— 于是 `--include` 会被当成**文件名**。实测的后果比"报错"更坏:
	//
	//	grep -rn x --include="*.go" .
	//	→ stderr 一句"路径不存在",stdout 却把**全部**文件都搜了,而且 exit 0
	//
	// 过滤没生效、退出码还是成功 —— 这正是本项目要消灭的"静默做错事"。所以这里
	// 必须响亮拒绝,并把"该改用哪种写法"直接说清楚(见 afterPatternOptionErr)。
	// `--` 之后的 `-` 开头参数是**文件名**,不在此列;-e 模式以 `-` 开头时也不在此列
	// (它已经被 -e 吃成模式了,不会落进文件名列表)。
	if !sawDashDash {
		for _, f := range files {
			// 单独的 `-` 是「读 stdin」的惯用文件名(grep x a.txt -),不是选项。
			if f != "-" && strings.HasPrefix(f, "-") {
				return nil, nil, nil, afterPatternOptionErr(f)
			}
		}
	}
	return opts, patterns, files, nil
}

// takeFlagValue 取带值开关(-A/-B/-C/-e)的值:优先 token 内附着(-A2、-e'x'),
// 否则吃下一个参数(实测 GNU 对 -nA + 下一参也这么收,连它像不像开关都不看 ——
// `grep -nA -e x` 报 `-e: invalid context length argument`)。
// 返回 ok=false 表示没有下一个参数可吃(调用方报"缺参数")。
func takeFlagValue(argv []string, i *int, tok string, j int) (string, bool) {
	if j+1 < len(tok) {
		return tok[j+1:], true
	}
	if *i+1 < len(argv) {
		*i++
		return argv[*i], true
	}
	return "", false
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

// compileGrep 按方言决策编译模式列表,失败时返回「问题 + 怎么办」两行文案。
//
// 方言仲裁(parseGrepArgs 已把 -E/-F 同现拦在门外),三条路:
//
//   - -F:不做任何正则编译,模式就是字面串。实测 GNU `grep -F 'a.c'` 对含
//     abc 与 a.c 的输入只匹配 a.c —— 修之前这里对非 -E 分支一律走 BRE 编译,
//     `-F` 静默按 BRE 跑,`a.c` 连 abc 都匹配上了。
//   - 默认(无 -E):每个模式先按 BRE 翻成 ERE(见 breToEre)。
//   - -E:模式按 POSIX ERE 原样进编译。
//
// 多个 -e 模式编成**一个**正则(各自包一层分组再用 | 连接)—— GNU 也把多个
// -e 合成一个 DFA,于是 `-o` 的输出按**行内位置**排序(实测 `grep -oE -e b -e a`
// 对 `ab` 给 a、b,不是按 -e 的给出顺序)。单模式不包分组,报错文案保持原样。
//
// POSIX ERE 不接受 Perl 简写(\d \w \s \D \W \S),先显式拒收再编译 ——
// 实测 `regexp.CompilePOSIX("\d+")` 确实会报 invalid escape sequence,
// 但那条信息不会告诉使用者「你写的是 Perl 方言」,更不会给出替代写法。
func compileGrep(patterns []string, opts *grepOpts) (*grepMatcher, string, string) {
	if opts.fixed {
		// -F:字面匹配,没有正则可言。多个 -e 各是一条字面量(实测 -Fe 也成立)。
		// -w 对 -F 同样生效(实测 fgrep -w 就是"字面量两侧都是词边界")。
		// 字面量提前转成 []byte:matchLine / findAll 对每行、每个位置都要用,
		// 现场转换是纯浪费。
		lits := make([][]byte, len(patterns))
		for i, p := range patterns {
			lits[i] = []byte(p)
		}
		return &grepMatcher{lits: lits, wordRegexp: opts.wordRegexp, wholeLine: opts.wholeLine}, "", ""
	}

	translated := make([]string, len(patterns))
	for i, p := range patterns {
		if !opts.extended {
			p = breToEre(p)
		}

		if cls, ok := firstPerlClass(p); ok {
			return nil, "-E 使用 POSIX ERE 方言," + cls + " 属于 Perl 简写,在这里没有定义",
				"改用 POSIX 写法: [0-9] / [[:digit:]] / [[:alpha:]] / [[:space:]]"
		}

		// `(?...)` 内联开关在 ERE 里没有定义。Perl 引擎会接受它,放过去等于默默换了
		// 方言,所以先显式挡掉 —— 这是下面那条回退路径唯一的例外。
		if strings.Contains(p, "(?") {
			return nil, "模式里有 (?...) 内联开关,那是 Perl 语法,POSIX ERE 里没有定义",
				"-E 的方言是 POSIX ERE;要忽略大小写用 -i,不要把 (?i) 写进模式"
		}
		translated[i] = p
	}

	joined := translated[0]
	if len(translated) > 1 {
		// 分组再连接:模式里可能有顶层 |,裸连会改变结合;分组对 Match/Find 无副作用。
		joined = "(" + strings.Join(translated, ")|(") + ")"
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
	normalized := normalizeEreClasses(joined)
	if posixErr := func() error { _, err := regexp.CompilePOSIX(normalized); return err }(); posixErr != nil {
		return nil, "模式不是合法的 POSIX ERE: " + posixErr.Error(),
			`要按字面匹配特殊字符就转义它(\\. ),字符类用 [[:alpha:]] 这种 POSIX 写法`
	}

	var r *regexp.Regexp
	if opts.ignoreCase {
		// 实测:Go 的 POSIX 模式不支持内联开关(`(?i)alpha` 报 missing argument),
		// 所以 -i 与 -E 只能叠加成「POSIX 校验 + Perl 引擎 + Longest()」:
		// 方言已由上面钉死,Longest() 再把匹配语义还原成 leftmost-longest。
		rr, err := regexp.Compile("(?i)" + normalized)
		if err != nil {
			return nil, "模式无法编译: " + err.Error(), "检查模式里的括号与转义"
		}
		r = rr
		r.Longest()
	} else {
		rr, err := regexp.CompilePOSIX(normalized)
		if err != nil {
			return nil, "模式不是合法的 POSIX ERE: " + err.Error(), "检查模式里的括号与转义"
		}
		r = rr
	}
	return &grepMatcher{re: r, wordRegexp: opts.wordRegexp, wholeLine: opts.wholeLine}, "", ""
}

// grepMatcher 把三种方言(-F 字面 / BRE / ERE)折成同一个问法:
// 「这一行命中吗」与「命中段在哪」。-w 的词边界检查放在**命中之后**逐段验,
// 而不是给模式包 \b(...) —— 实测两者不等价:GNU 检查的是**命中段**两侧是不是
// 词字符,模式本身以非词字符开头/结尾时(如 `-w 'a!'` 对 `a!`)GNU 照样命中,
// 包 \b 的写法会漏。词字符的定义两边一致:字母、数字、下划线。
type grepMatcher struct {
	re         *regexp.Regexp // -E / BRE(已翻译)的编译结果;-F 时为 nil
	lits       [][]byte       // -F:模式原文就是字面量(提前转好的字节切片)
	wordRegexp bool           // -w:只认两侧都是词边界的命中段
	wholeLine  bool           // -x:只认覆盖整行的命中
}

// matchLine 报告这一行是否命中(考虑 -x 与 -w;不考虑 -v,反转在调用方做)。
//
// -x(整行匹配)不靠给模式包 ^...$ 锚点,而是检查"是否存在覆盖整行的命中":
// POSIX 的最左最长语义保证 —— 只要存在整行命中,它就是 FindIndex 给的那个
// (更靠左的位置不存在,同起点下最长也不可能越过行尾)。实测口径:`-x 'x*'`
// 对 abc 不命中、对空行命中;空模式只命中空行。`-w` 与 `-x` 叠加不用另设卡:
// 整行命中的两端必然是行首/行尾,天然是词边界(GNU 同)。
func (m *grepMatcher) matchLine(content []byte) bool {
	if m.wholeLine {
		if m.re != nil {
			loc := m.re.FindIndex(content)
			return loc != nil && loc[0] == 0 && loc[1] == len(content)
		}
		for _, l := range m.lits {
			if bytes.Equal(content, l) {
				return true // -x -F:整行字面相等(实测 -x -F 'a.c' 不命中 abc)
			}
		}
		return false
	}
	if !m.wordRegexp {
		if m.re != nil {
			return m.re.Match(content)
		}
		for _, l := range m.lits {
			if bytes.Contains(content, l) {
				return true
			}
		}
		return false
	}
	return len(m.findAll(content)) > 0
}

// findAll 返回本行内所有**可用**的命中段:最左最长、互不重叠、跳过空命中;
// -w 时再按词边界过滤。返回值按行内位置升序。
//
// -w 的重试语义对齐 GNU:一个命中段没过词边界检查时,从它的**起点 +1** 重新找
// (不是从终点继续)—— 更靠后、起点落在失败段内部的命中仍然有机会入选。
func (m *grepMatcher) findAll(content []byte) [][]int {
	if m.re != nil {
		if !m.wordRegexp {
			var locs [][]int
			for _, loc := range m.re.FindAllIndex(content, -1) {
				if loc[1] > loc[0] { // GNU 实测:-o 不输出空命中(`b*` 对 `abc` 只给 b)
					locs = append(locs, loc)
				}
			}
			return locs
		}
		var locs [][]int
		for pos := 0; pos <= len(content); {
			loc := m.re.FindIndex(content[pos:])
			if loc == nil {
				break
			}
			s, e := pos+loc[0], pos+loc[1]
			if e > s && wordBounded(content, s, e) {
				locs = append(locs, []int{s, e})
				pos = e // 命中入选:从它后面继续(不重叠)
			} else {
				pos = s + 1 // 没过词边界:起点 +1 重试(GNU 同款)
			}
		}
		return locs
	}

	// -F:在每个位置找**最长**的命中字面量(最左最长,与合成 DFA 的 GNU 一致)。
	var locs [][]int
	for pos := 0; pos < len(content); {
		best := 0
		for _, l := range m.lits {
			if len(l) > best && bytes.HasPrefix(content[pos:], l) {
				best = len(l)
			}
		}
		if best == 0 {
			pos++
			continue
		}
		if !m.wordRegexp || wordBounded(content, pos, pos+best) {
			locs = append(locs, []int{pos, pos + best})
			pos += best
		} else {
			pos++ // 没过词边界:起点 +1 重试
		}
	}
	return locs
}

// wordBounded 报告 content[s:e] 这个命中段两侧是否都是词边界
// (行首/行尾、或紧邻非词字符)。词字符 = 字母、数字、下划线,按 rune 解码后用
// unicode 包判定 —— GNU 在 UTF-8 locale 下把非 ASCII 字母/数字也算词字符
// (实测:`-w '中'` 不命中 `中文` —— 右邻的「文」挡住了词尾边界;旧的 byte
// 判定会把这类行错放进来)。非 UTF-8 序列按 RuneError 落到"非词字符"一侧。
func wordBounded(content []byte, s, e int) bool {
	if s > 0 && isWordRuneBefore(content, s) {
		return false
	}
	if e < len(content) && isWordRuneAfter(content, e) {
		return false
	}
	return true
}

func isWordRuneBefore(content []byte, s int) bool {
	r, _ := utf8.DecodeLastRune(content[:s])
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

func isWordRuneAfter(content []byte, e int) bool {
	r, _ := utf8.DecodeRune(content[e:])
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
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

// grepSep 持有组间隔线(`--`)的**调用级**游标,跨文件/跨流共享。
//
// 实测 GNU:给了 -A/-B/-C 之后,`--` 也出现在文件边界上 —— 上一组在 f1、新组
// 在 f2(f、stdin 的 `-`、-r 递归同理)时同样补一条。只有"从本调用开始还没输出
// 过任何组"(lastStream == 0)才不打。没给上下文开关时整个分组逻辑不启用
// (contextGiven=false,游标不更新也不会打)。
type grepSep struct {
	contextGiven bool
	lastStream   uint64 // 上一次输出所属的流编号(0 = 还没有任何组输出)
	groupEnd     int    // 上一组覆盖到的最后一行(该流自己的行号:命中行 + after)
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
// binary 这第二个返回值见 noteBinary:它只用于**提示**,不影响输出内容。
//
// 上下文(-A/-B/-C)的发射规则全部按真 grep 实测钉死:
//
//   - 命中行打 `:` 前缀,上下文行打 `-` 前缀(-n 时行号后同样 `:` / `-`);
//   - 两组之间有**没被任何组覆盖**的空档时补一条 `--`(组间隔线,无文件名前缀);
//   - 组相邻/重叠时合并,不打 `--`;**换流必打** —— 实测 GNU 的 `--` 是调用级的,
//     文件列表、stdin 的 `-`、-r 递归的文件边界上同样补 `--`(游标在 grepSep);
//   - 没给过任何 -A/-B/-C 时整个分组逻辑不启用(实测裸的多组命中之间没有 `--`,
//     显式 -A0 才有);
//   - 组的覆盖范围 = [命中行 - before, 命中行 + after],**-o 不打印上下文行,
//     但组覆盖照算** —— 实测 `-o -A1` 在两组隔空时照样给 `--`;
//   - 已作为上一组上下文输出过的行,轮到它命中时按**命中行**重新打印一次
//     (实测 -n -A2 命中 l3、l4 给 `3:l3 / 4:l4 / 5-l5 / 6-l6`);
//   - -c / -l / -q 下不输出行,-A/-B/-C 全部失效(-c 只给计数,与实测一致)。
func grepStream(r io.Reader, m *grepMatcher, opts *grepOpts, label string, showName bool, out io.Writer, sep *grepSep, streamID uint64) (grepResult, bool, error) {
	var res grepResult
	binary := false
	br := bufio.NewReaderSize(SafeStdin(r), 64*1024)
	lineNo := 0

	// 本输入流的发射游标(跨流的组间隔游标在 sep,不在这里)。
	lastEmitted := 0     // 本流内最后一行**实际输出**的行号(-B 去重的依据)
	afterPending := 0    // 命中行之后还欠几行 -A 上下文
	var history [][]byte // -B 的行缓冲(最近 beforeN 行,按行号升序)

	emitLine := func(n int, content []byte, context bool) {
		writeGrepLine(out, label, showName, n, opts, content, context)
		lastEmitted = n
	}
	// 组间隔线:换流必补;同流内,这一组的第一行与上一组覆盖末尾之间有空档
	// (≥1 行谁都没覆盖)时补。第一条组之前(sep.lastStream == 0)不打。
	sepIfNeeded := func(groupStart int) {
		if !opts.contextGiven || sep.lastStream == 0 {
			return
		}
		if sep.lastStream != streamID || groupStart > sep.groupEnd+1 {
			io.WriteString(out, "--\n")
		}
	}

	for {
		raw, err := br.ReadBytes('\n')

		if len(raw) > 0 {
			lineNo++
			content := raw
			if content[len(content)-1] == '\n' {
				content = content[:len(content)-1]
			}

			hit := m.matchLine(content)
			if opts.invert {
				hit = !hit
			}
			if hit && opts.maxCount >= 0 && res.lines >= opts.maxCount {
				// -m 上限已到:GNU 到此停止读入 —— 这一行不选中、不计数、不输出,
				// 连作为 -A 上下文都不出现(实测 -m1 -A1 对 a1,a2,a3 只给 a1:
				// 第二行的 a2 虽命中,却被整个跳过,而不是当作上一组的上下文)。
				break
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

				if !opts.count && !(opts.onlyMatching && opts.invert) {
					groupStart := lineNo - opts.beforeN
					if groupStart < 1 {
						groupStart = 1
					}
					sepIfNeeded(groupStart)
					// -B:先补前导上下文;已输出过、或已滑出窗口的行跳过。
					for k, hc := range history {
						hn := lineNo - len(history) + k
						if hn <= lastEmitted || hn < groupStart {
							continue
						}
						if !opts.onlyMatching {
							emitLine(hn, hc, true)
						}
					}
					if opts.onlyMatching {
						// -o:只输出命中段(按行内位置序、跳过空段),前缀与整行同款。
						// -x -o 的例外:命中段就是整行(实测 GNU -x -o 'abc' 对 abc
						// 给一段整行,不重复、不带行内其他段)。
						if opts.wholeLine {
							writeGrepLine(out, label, showName, lineNo, opts, content, false)
							lastEmitted = lineNo
						} else {
							for _, loc := range m.findAll(content) {
								writeGrepLine(out, label, showName, lineNo, opts, content[loc[0]:loc[1]], false)
								lastEmitted = lineNo
							}
						}
					} else {
						emitLine(lineNo, content, false)
					}
					// -o 与 -v 同给:实测一个字都不出(被选中的行本来就不含模式的命中段,
					// 连组间隔线都没有);exit 仍只看有没有行被选中。
					sep.groupEnd = lineNo + opts.afterN
					sep.lastStream = streamID
				}
				afterPending = opts.afterN
			} else if afterPending > 0 {
				// -A:紧跟上一组;流是连续的,这一行必然与已输出内容相接,不用查间隔线。
				afterPending--
				if !opts.count && !opts.onlyMatching {
					emitLine(lineNo, content, true)
				}
			}

			if opts.maxCount >= 0 && res.lines >= opts.maxCount && afterPending == 0 {
				// -m 上限已到且 -A 的尾随上下文已经吐完:停止读入。实测 GNU 对
				// -m1 -A2 的 a1,x1,a2,x2,a3 只给 a1,x1 —— 第二条上下文行根本不读,
				// 并不是"读完上下文再停"。
				break
			}

			// 维护 -B 缓冲:命中行自身也算后面命中的前文。
			if opts.beforeN > 0 {
				history = append(history, content)
				if len(history) > opts.beforeN {
					history = history[len(history)-opts.beforeN:]
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

	// -c 打计数;-m 0 是唯一的例外:GNU 连计数行都不给(实测 -m 0 -c 无输出、
	// exit 1,不管文件里有没有命中)。
	if opts.count && opts.maxCount != 0 {
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

// writeGrepLine 输出一行。context=true 是 -A/-B/-C 带出的上下文行:实测 GNU 把
// 行前缀的分隔符从 `:` 换成 `-`(文件名与行号两处都是),命中行仍是 `:`。
func writeGrepLine(out io.Writer, label string, showName bool, lineNo int, opts *grepOpts, content []byte, context bool) {
	sep := ":"
	if context {
		sep = "-"
	}
	if showName && label != "" {
		io.WriteString(out, label+sep)
	}
	if opts.lineNumber {
		fmt.Fprintf(out, "%d%s", lineNo, sep)
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
func grepFile(abs string, m *grepMatcher, opts *grepOpts, label string, showName bool, out io.Writer, errw io.Writer, report func(error, string), sep *grepSep, streamID uint64) (matched, ok bool) {
	f, err := os.Open(abs)
	if err != nil {
		report(err, "先确认文件存在,或用 winbash -c 'ls 目录' 看有哪些名字")
		return false, false
	}
	defer f.Close()

	res, binary, err := grepStream(f, m, opts, label, showName, out, sep, streamID)
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
func grepTree(root, display string, m *grepMatcher, opts *grepOpts, out io.Writer, errw io.Writer, report func(error, string), sep *grepSep, streamID *uint64) bool {
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
		*streamID++
		if matched, ok := grepFile(p, m, opts, label, true, out, errw, report, sep, *streamID); ok && matched {
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
