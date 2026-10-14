package builtins

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"winbash/internal/errs"
	"winbash/internal/output"
)

// Ls 实现 bash 风格的 ls。
//
// 默认输出与 bash 完全一致:一行一个名字,无装饰、无表头、无颜色。
// 这是 ① 层最重要的兼容点 —— AI 对 ls 的输出有极强预期,任何"贴心"的表头
// (`Mode LastWriteTime Length Name`)都会让它的解析当场失效。
// 额外信息一律通过显式的 -l 索取,或通过 --json 拿结构化形式。
func Ls(ctx context.Context, cwd string, argv []string, out, errw io.Writer) int {
	var (
		long, all, human bool
		paths            []string
	)

	// 逐字符解析,支持 -la 这类组合写法(bash 惯例)。
	// 不认识的开关一律报错 —— 静默忽略一个 flag,使用者会以为它生效了。
	for _, a := range argv {
		if a == "-" || !strings.HasPrefix(a, "-") {
			paths = append(paths, a)
			continue
		}
		// 长开关整个报出来。拆成单字符会把 "--nope" 报成 "--",
		// 使用者拿着这个提示没法定位问题。
		if strings.HasPrefix(a, "--") {
			fmt.Fprintln(errw, errs.Errorf("ls", "不认识的参数: "+a, "支持的参数: -l -a -h -1"))
			return errs.ExitUsage
		}
		for _, c := range a[1:] {
			switch c {
			case 'l':
				long = true
			case 'a':
				all = true
			case 'h':
				human = true
			case '1':
				// 每行一个 —— 这本来就是我们的默认形态,所以接受它。
				// 不接受反而更坏:`ls -1` 是极常见的写法,而它在这里的预期行为
				// 恰好就是现状(给了 -l 时被 -l 压过,与 bash 一致)。
			default:
				fmt.Fprintln(errw, errs.Errorf("ls", "不认识的参数: -"+string(c), "支持的参数: -l -a -h -1"))
				return errs.ExitUsage
			}
		}
	}

	if len(paths) == 0 {
		paths = []string{"."}
	}

	code := errs.ExitOK
	for _, p := range paths {
		abs, err := ResolvePath(cwd, p)
		if err != nil {
			fmt.Fprintln(errw, errs.Errorf("ls", err.Error(), "检查路径写法: "+p))
			code = errs.ExitError
			continue
		}

		entries, err := os.ReadDir(abs)
		if err != nil {
			// 不是目录 —— 按 bash 惯例把它当单个文件列出,而不是直接失败。
			info, serr := os.Stat(abs)
			if serr != nil {
				fmt.Fprintln(errw, errs.Errorf("ls", serr.Error(), "先确认路径存在,或用 winbash -c 'ls 上级目录' 看有哪些名字"))
				code = errs.ExitError
				continue
			}
			writeLsEntry(out, filepath.Base(abs), info, long, human)
			continue
		}

		if all {
			// bash 的 -a 会列出 . 与 ..。Windows 上 os.ReadDir 本来就返回隐藏文件,
			// 所以这里唯一需要补的就是这两个特殊条目。
			for _, name := range []string{".", ".."} {
				info, ierr := os.Stat(filepath.Join(abs, name))
				if ierr != nil {
					continue
				}
				writeLsEntry(out, name, info, long, human)
			}
		}

		for _, e := range entries {
			// 点文件默认不列。这是 **POSIX 语义**,与 Windows 的"隐藏属性"无关 ——
			// 真 bash 的 `ls` 也看不到 .hidden,而 AI 对 ls 的输出有极强预期
			// (常常 `ls` 完就去拼文件名)。原先这里只有一句"少不了 . 和 .."的注释,
			// 把"列不列点文件"当成了无需处理的细节,于是多出一个静默的差异。
			if !all && strings.HasPrefix(e.Name(), ".") {
				continue
			}
			info, ierr := e.Info()
			if ierr != nil {
				fmt.Fprintln(errw, errs.Errorf("ls", ierr.Error(), "读不到 "+e.Name()+" 的属性,已跳过"))
				code = errs.ExitError
				continue
			}
			writeLsEntry(out, e.Name(), info, long, human)
		}
	}
	return code
}

// writeLsEntry 输出一个条目,长格式为「权限 大小 修改时间 名字」。
//
// 大小走 output.FormatBytes(…, human),默认是纯字节数:
// "86,488 K" 那种字符串看着像数字却不是数字,只给人眼好看,给解析器添乱。
func writeLsEntry(w io.Writer, name string, info os.FileInfo, long, human bool) {
	if !long {
		fmt.Fprintln(w, name)
		return
	}
	fmt.Fprintf(w, "%s %s %s %s\n",
		info.Mode().String(),
		output.FormatBytes(info.Size(), human),
		info.ModTime().Format("2006-01-02 15:04:05"),
		name,
	)
}
