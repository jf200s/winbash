package builtins

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"winbash/internal/errs"
)

// Cat 实现 bash 风格的 cat:把输入原样写到 stdout。
//
// 用 io.Copy 而不是 os.ReadFile + string 转换。后者会经手一次字符串,
// 而 Windows 上的文本文件未必是 UTF-8(GBK 编码的中文文件很常见)——
// 任何一次「这应该是文本」的假设都会静默改写字节。二进制安全不是加分项,
// 它是「值保证」的前置条件:值被改过了,后面所有解析都是在错误的地基上做的。
//
// 输入来源与 grep 对齐(bash 语义):
//   - 给文件名 → 读那些文件
//   - `-`      → 读标准输入;可以出现在文件列表中间,按出现顺序读
//   - 不给参数 → 读标准输入
//
// 开关只实现 -n(给每行编号)。**不认识的开关一律报错**,而不是当成文件名 ——
// 修之前 `cat -n f` 会把 "-n" 当作一个不存在的文件:内容照常打印、退出码却是 1,
// 也就是"看着成功、其实失败",正是本项目最想消灭的形态。
func Cat(ctx context.Context, cwd string, argv []string, in io.Reader, out, errw io.Writer) int {
	numbered, files, perr := parseCatArgs(argv)
	if perr != nil {
		fmt.Fprintln(errw, errs.Errorf("cat", perr.Error(), "支持的参数: -n(给每行编号);`--` 之后一律当文件名"))
		return errs.ExitUsage
	}

	code := errs.ExitOK
	// 行号跨文件**累加**(GNU cat 的语义):`cat -n f1 f2` 里 f2 的第一行接着 f1 的
	// 编号,而不是每个文件从 1 重新开始。计数器原先放在 copyCat 内部,于是多文件时
	// 每段都重置 —— 实测 `1 a 2 b | 1 c 2 d`,真 bash 给 `1 a 2 b | 3 c 4 d`。
	lineNo := 0
	for _, name := range files {
		if name == "-" {
			if cerr := copyCat(out, SafeStdin(in), numbered, &lineNo); cerr != nil {
				fmt.Fprintln(errw, errs.Errorf("cat",
					catCopyProblem("(standard input)", cerr),
					catCopyRemedy(cerr)))
				code = errs.ExitError
			}
			continue
		}

		abs, err := ResolvePath(cwd, name)
		if err != nil {
			fmt.Fprintln(errw, errs.Errorf("cat", err.Error(), "检查路径写法: "+name))
			code = errs.ExitError
			continue
		}

		f, err := os.Open(abs)
		if err != nil {
			// 与 bash 一致:打不开这个就跳过,继续处理后面的,只把退出码置非零。
			// 直接 return 会让 `cat a b` 在 a 缺失时连 b 都读不到。
			fmt.Fprintln(errw, errs.Errorf("cat",
				userPathProblem("", name, err, abs),
				"确认文件存在,或用 winbash -c 'ls 目录' 查看"))
			code = errs.ExitError
			continue
		}

		// 目录必须在这里拦住:**Windows 上 os.Open 一个目录是成功的**,失败要到
		// 第一次 Read 才发生,而且报的是 `Incorrect function.` —— 实测原先它一路
		// 掉进下面那个"写输出时出错"的分支,归因完全错了(实际是读不出来)。
		// 真 bash 对 `cat 某目录` 给的是裸格式 `adir: Is a directory`
		// (实测 2026-10-18:`cat: adir: Is a directory`)。
		if fi, serr := f.Stat(); serr == nil && fi.IsDir() {
			f.Close()
			fmt.Fprintln(errw, errs.Errorf("cat",
				fmt.Sprintf("%s: Is a directory", name),
				"要看目录内容用 ls;要递归就用 find"))
			code = errs.ExitError
			continue
		}

		cerr := copyCat(out, f, numbered, &lineNo)
		f.Close()
		if cerr != nil {
			fmt.Fprintln(errw, errs.Errorf("cat",
				catCopyProblem(name, cerr),
				catCopyRemedy(cerr)))
			code = errs.ExitError
		}
	}
	return code
}

// catCopyError 标明 copyCat 的错误是**读**出来的还是**写**出去的。
//
// 为什么要区分:原先两种情况共用一句"写输出时出错",于是 `cat 目录`(读错误)
// 被报成写失败 —— 实测误导过一次。真 bash 的措辞也分开:`Is a directory` 是读侧,
// 管道断开(Broken pipe)是写侧。
type catCopyError struct {
	op  string // "read" 或 "write"
	err error
}

func (e *catCopyError) Error() string { return e.err.Error() }
func (e *catCopyError) Unwrap() error { return e.err }

// catCopyProblem 生成 copyCat 失败时的"问题"行。
//
// 三种情况的措辞口径不同,都对着真 Git Bash 实测过(2026-10-18):
//
//	读一个文件失败   → `adir: Is a directory`        (裸格式,与 GNU cat 一致)
//	读 stdin 失败    → 保留显式说明(`standard input`),真 bash 在那里给的是 `-`
//	写侧失败         → 保留显式说明
//
// 写侧与 stdin 两条**没有**照搬 GNU:真 bash 里 `cat 大文件 | head` 是被 SIGPIPE
// 直接杀掉(rc=0、stderr 空),`cat f > /bad/x` 则由 **shell 层**拦截、cat 根本没
// 启动。winbash 不会因 SIGPIPE 被杀(它自己管着输出),所以那个错误必须自己报 ——
// 报出来比照抄一个"什么都没有"更有用。
func catCopyProblem(subject string, err error) string {
	var ce *catCopyError
	if errors.As(err, &ce) && ce.op == "write" {
		// 写侧失败没有文件名可言:对象是管道/终端,不是被读的那个文件。
		return "cannot write output: " + posixErrText(ce.err, "")
	}
	if subject == "(standard input)" {
		return "cannot read standard input: " + posixErrText(err, "")
	}
	return userPathProblem("", subject, err, "")
}

// catCopyRemedy 给出对应的下一步。
func catCopyRemedy(err error) string {
	var ce *catCopyError
	if errors.As(err, &ce) && ce.op == "write" {
		return "输出端已关闭(常见于 `cat 大文件 | head`);这不影响文件本身"
	}
	return "确认这个文件可读(权限、是否被占用),或换一个文件试"
}

// parseCatArgs 拆出开关与文件名。
//
// 只认 -n。不认识的开关**报错**而不是当文件名(理由见 Cat 的文档)。
// `-` 单独出现仍表示标准输入,`--` 之后一律当文件名。
// bash 那批控制字符可视化开关(-A/-b/-e/-E/-s/-t/-T/-u/-v)在 Windows 上没有
// 对应语义,给一条明确的"未实现"比装作支持好。
func parseCatArgs(argv []string) (numbered bool, files []string, err error) {
	i := 0
	for ; i < len(argv); i++ {
		a := argv[i]
		if a == "-" || !strings.HasPrefix(a, "-") {
			break
		}
		if a == "--" {
			i++
			break
		}
		if strings.HasPrefix(a, "--") {
			return false, nil, fmt.Errorf("不认识的参数: %s", a)
		}
		for _, c := range a[1:] {
			switch c {
			case 'n':
				numbered = true
			case 'A', 'b', 'e', 'E', 's', 't', 'T', 'u', 'v':
				return false, nil, fmt.Errorf("参数 -%c 未实现", c)
			default:
				return false, nil, fmt.Errorf("不认识的参数: -%c", c)
			}
		}
	}
	files = argv[i:]
	if len(files) == 0 {
		files = []string{"-"}
	}
	return numbered, files, nil
}

// copyCat 搬运字节;-n 时给每行加 "     1\t" 前缀(与 bash 的 cat -n 一致:
// 宽度 6、右对齐、后跟一个制表符)。
//
// 行号由调用方持有(*lineNo),这样多个文件之间是**累加**的 —— 与 GNU cat 一致。
// 逐行读而不是给每行补 \n:最后一行没有换行符时也要原样输出,不能凭空加一个。
func copyCat(out io.Writer, r io.Reader, numbered bool, lineNo *int) error {
	if !numbered {
		// 显式读写循环而不是 io.Copy:io.Copy 不告诉我们错误来自读侧还是写侧,
		// 而调用方要据此选措辞(`cannot read 'x'` vs `cannot write output`)。
		// 实测过 `cat 目录`:Windows 上 open 目录成功、第一次 Read 才失败,
		// 若两侧混为一谈就会报成"写输出时出错" —— 归因完全反了。
		buf := make([]byte, 64*1024)
		for {
			n, rerr := r.Read(buf)
			if n > 0 {
				if _, werr := out.Write(buf[:n]); werr != nil {
					return &catCopyError{op: "write", err: werr}
				}
			}
			if rerr != nil {
				if rerr == io.EOF {
					return nil
				}
				return &catCopyError{op: "read", err: rerr}
			}
		}
	}
	br := bufio.NewReaderSize(r, 64*1024)
	for {
		raw, err := br.ReadBytes('\n')
		if len(raw) > 0 {
			*lineNo++
			if _, werr := fmt.Fprintf(out, "%6d\t", *lineNo); werr != nil {
				return &catCopyError{op: "write", err: werr}
			}
			if _, werr := out.Write(raw); werr != nil {
				return &catCopyError{op: "write", err: werr}
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return &catCopyError{op: "read", err: err}
		}
	}
}
