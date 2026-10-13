package builtins

import (
	"context"
	"fmt"
	"io"
	"os"

	"winbash/internal/errs"
)

// Cat 实现 bash 风格的 cat:把文件内容原样写到 stdout。
//
// 用 io.Copy 而不是 os.ReadFile + string 转换。后者会经手一次字符串,
// 而 Windows 上的文本文件未必是 UTF-8(GBK 编码的中文文件很常见)——
// 任何一次「这应该是文本」的假设都会静默改写字节。二进制安全不是加分项,
// 它是「值保证」的前置条件:值被改过了,后面所有解析都是在错误的地基上做的。
func Cat(ctx context.Context, cwd string, argv []string, out, errw io.Writer) int {
	if len(argv) == 0 {
		fmt.Fprintln(errw, errs.Errorf("cat", "没有指定文件", "用法: cat 文件 [文件...]"))
		return errs.ExitUsage
	}

	code := errs.ExitOK
	for _, name := range argv {
		if name == "-" {
			// bash 惯例里 "-" 表示 stdin。① 层的 cat 不接 stdin,
			// 明确报错好过把它当成一个名叫 "-" 的文件去打开。
			fmt.Fprintln(errw, errs.Errorf("cat", "暂不支持从 stdin 读(-)", "直接用文件名: cat 文件"))
			code = errs.ExitUsage
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

		_, cerr := io.Copy(out, f)
		f.Close()
		if cerr != nil {
			fmt.Fprintln(errw, errs.Errorf("cat", cerr.Error(), "写输出时出错"))
			code = errs.ExitError
		}
	}
	return code
}
