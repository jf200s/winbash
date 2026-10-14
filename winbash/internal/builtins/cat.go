package builtins

import (
	"bufio"
	"context"
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
	for _, name := range files {
		if name == "-" {
			if cerr := copyCat(out, SafeStdin(in), numbered); cerr != nil {
				fmt.Fprintln(errw, errs.Errorf("cat", cerr.Error(), "读标准输入时出错"))
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
			fmt.Fprintln(errw, errs.Errorf("cat", err.Error(), "确认文件存在,或用 winbash -c 'ls 目录' 查看"))
			code = errs.ExitError
			continue
		}

		cerr := copyCat(out, f, numbered)
		f.Close()
		if cerr != nil {
			fmt.Fprintln(errw, errs.Errorf("cat", cerr.Error(), "写输出时出错"))
			code = errs.ExitError
		}
	}
	return code
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
// 逐行读而不是给每行补 \n:最后一行没有换行符时也要原样输出,不能凭空加一个。
func copyCat(out io.Writer, r io.Reader, numbered bool) error {
	if !numbered {
		_, err := io.Copy(out, r)
		return err
	}
	br := bufio.NewReaderSize(SafeStdin(r), 64*1024)
	n := 0
	for {
		raw, err := br.ReadBytes('\n')
		if len(raw) > 0 {
			n++
			if _, werr := fmt.Fprintf(out, "%6d\t", n); werr != nil {
				return werr
			}
			if _, werr := out.Write(raw); werr != nil {
				return werr
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}
