package builtins

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

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
		long, all, human, dirOnly bool
		paths                     []string
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
			remedy := "支持的参数: -l -a -h -1 -d"
			if a == "--recursive" {
				// ls 不做递归是对的(① 层的 ls 只列一层,语义可控),但"不认识的参数"
				// 没说下一步 —— 递归列目录在 bash 里的标准解法是 find,直接给出来。
				remedy = "ls 不递归;要递归列目录用 find: find . -name '*' (只看文件用 find . -type f)"
			}
			fmt.Fprintln(errw, errs.Errorf("ls", "不认识的参数: "+a, remedy))
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
			case 'd':
				// 列**目录本身**而不是它的内容。`ls -d */` 只看子目录名、
				// `ls -ld 某个目录` 只看那个目录的属性,是极常见的写法。
				dirOnly = true
			case '1':
				// 每行一个 —— 这本来就是我们的默认形态,所以接受它。
				// 不接受反而更坏:`ls -1` 是极常见的写法,而它在这里的预期行为
				// 恰好就是现状(给了 -l 时被 -l 压过,与 bash 一致)。
			default:
				remedy := "支持的参数: -l -a -h -1 -d"
				if c == 'R' {
					remedy = "ls 不递归;要递归列目录用 find: find . -name '*' (只看文件用 find . -type f)"
				}
				fmt.Fprintln(errw, errs.Errorf("ls", "不认识的参数: -"+string(c), remedy))
				return errs.ExitUsage
			}
		}
	}

	if len(paths) == 0 {
		paths = []string{"."}
	}

	// 空字符串参数要**响亮拒绝**,不能静默当成当前目录。
	//
	// 根因:ResolvePath 把 `""` 当 cwd(`internal/builtins/fsutil.go` 里
	// `if p == "" { return cwd, nil }`)—— 那个分支是为 `cd` 服务的(`cd ""` 不改目录),
	// 但 ls 沿用它就变成静默做错事。实测(2026-10-18)修之前:
	//
	//	ls ""     → rc=0,列出**当前目录的全部内容**
	//	ls -d ""  → rc=0,输出一个空行
	//	真 bash   → rc=2,`ls: cannot access '': No such file or directory`
	//
	// `ls ""` 那条尤其坏:调用方(典型是 AI)拿到的是一份看起来正常的目录列表,
	// 会以为列的是它传的那个目录。这类"静默给出错值"正是项目底线要防的形态。
	// 不去改 ResolvePath —— 它的空串语义对 cd 是对的,这里只收窄 ls 自己的口径。
	for _, p := range paths {
		if p == "" {
			fmt.Fprintln(errw, errs.Errorf("ls",
				"路径参数是空字符串",
				"空字符串不是当前目录;要列当前目录就写 `ls .` 或干脆不带参数"))
			return errs.ExitUsage
		}
	}

	// `-d`:每个参数**只列它自己**,不展开目录。所以整批参数一起处理、
	// 一起排序、一次输出 —— 而不是像目录展开那样逐路径分组。
	//
	// 语义全部对着真 Git Bash 实测过(2026-10-18):
	//
	//	ls -d            → "."              (默认参数 "." 不展开)
	//	ls -d dirA       → "dirA"
	//	ls -d */         → "dirA/" "dirB/"  (**保留参数里的尾斜杠**)
	//	ls -d *          → "dirA" "dirB" "f1.txt" "f2.txt"   (按名字排序)
	//	ls -d f1.txt dirA dirB → "dirA" "dirB" "f1.txt"      (排序,不按原序)
	//	ls -d dirB dirA dirB   → "dirA" "dirB" "dirB"        (重复参数保留)
	//	ls -l -d */      → 长格式,**没有 total 行**
	//	ls -d nope_xyz   → rc=2 + stderr
	//
	// 两个容易写错的点:
	//  1. **名字要用参数原文**(`p`),不能用 filepath.Base(abs)。这样 `*/` 展开出的
	//     尾斜杠、用户写的 `dirA/`、绝对路径都原样保留,与 bash 行为一致。
	//  2. **不打 total 行**。GNU ls 只在列目录内容时给 total;-d 列的是参数本身。
	if dirOnly {
		items := make([]lsItem, 0, len(paths))
		code := errs.ExitOK
		for _, p := range paths {
			abs, err := ResolvePath(cwd, p)
			if err != nil {
				fmt.Fprintln(errw, errs.Errorf("ls", err.Error(), "检查路径写法: "+p))
				code = errs.ExitError
				continue
			}
			info, serr := os.Stat(abs)
			if serr != nil {
				fmt.Fprintln(errw, errs.Errorf("ls", serr.Error(),
					"先确认路径存在,或用 winbash -c 'ls 上级目录' 看有哪些名字"))
				code = errs.ExitError
				continue
			}
			it := lsItem{name: p, info: info}
			if long {
				it.links = linkCount(abs)
			}
			items = append(items, it)
		}
		// SliceStable:重复参数(如 `ls -d a a`)必须都留着,而排序只需可复现。
		sort.SliceStable(items, func(i, j int) bool { return items[i].name < items[j].name })

		var widths lsWidths
		if long {
			widths = lsMeasure(items, human)
		}
		for _, it := range items {
			writeLsEntry(out, it.name, it, long, human, widths)
		}
		return code
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
			// 单个文件也要按长格式对齐 —— 此时"最大宽度"就是它自己,等价于不补齐。
			// 传一个条目,复用同一套测量逻辑,免得两处格式串分叉。
			//
			// **名字用参数原文 `p`,不用 filepath.Base**。这条一度只在 `-d` 分支做对,
			// 普通分支给的是 Base,于是同一个二进制里出现两种口径(实测 2026-10-18):
			//
			//	ls -d /abs/real.txt  → /abs/real.txt   ✓
			//	ls    /abs/real.txt  → real.txt        ✗(真 bash 给 /abs/real.txt)
			//	ls    ./real.txt     → real.txt        ✗(真 bash 给 ./real.txt)
			//
			// 真 GNU 对**所有**操作数都输出参数原文。同一份 ls 里两种策略本身也是维护
			// 风险 —— 下一个人很容易"统一"成错的那一边。
			it := lsItem{name: p, info: info}
			var widths lsWidths
			if long {
				it.links = linkCount(abs)
				widths = lsMeasure([]lsItem{it}, human)
			}
			writeLsEntry(out, it.name, it, long, human, widths)
			continue
		}

		// 收集条目(名字 + 可选 info),再统一排序。
		//
		// 两个要点,都是踩过才写下来的:
		//
		//  1. `.` 与 `..` 必须**参与同一个排序**,不能先写出去。实测目录含 `!x` 时,
		//     真 bash 给 `!x . .. .hidden ...`(`!` 是 0x21,排在 `.` 的 0x2E 之前)。
		//  2. 真实条目走 `DirEntry.Info()`,**不要**换成 `os.Stat`。Windows 上
		//     os.ReadDir 已经把大小与属性放在 WIN32_FIND_DATA 里带回来了,Info() 直接
		//     取缓存;而 os.Stat 是一次全新的 GetFileAttributesEx —— 每个条目多一次
		//     系统调用(同进程 3000 项微基准下 4574×,端到端在热缓存上看不出来,
		//     但在冷缓存/网络盘上会)。短格式(`ls` 不带 `-l`)**完全不用 info**,
		//     那条路径上连取都不该取。
		items := make([]lsItem, 0, len(entries)+2)
		if all {
			// 只有这两个合成条目需要真去问一次(它们不在 entries 里)。
			for _, n := range []string{".", ".."} {
				if fi, ierr := os.Stat(filepath.Join(abs, n)); ierr == nil {
					it := lsItem{name: n, info: fi}
					if long {
						it.links = linkCount(filepath.Join(abs, n))
					}
					items = append(items, it)
				}
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
			if !long {
				items = append(items, lsItem{name: e.Name()})
				continue
			}
			fi, ierr := e.Info()
			if ierr != nil {
				fmt.Fprintln(errw, errs.Errorf("ls", ierr.Error(), "读不到 "+e.Name()+" 的属性,已跳过"))
				code = errs.ExitError
				continue
			}
			// 链接数在这里**预取一次**,不要在输出/测量阶段各问一次。
			items = append(items, lsItem{
				name:  e.Name(),
				info:  fi,
				links: linkCount(filepath.Join(abs, e.Name())),
			})
		}
		sort.Slice(items, func(i, j int) bool { return items[i].name < items[j].name })

		// total 行。GNU `ls -l` 的第一行永远是它,且**只在列目录时**有
		// (列单个文件时没有 —— 已在上面那条分支直接返回)。
		//
		// 为什么必须有这一行,哪怕数值不精确:脚本常用 `ls -l | tail -n +2` 跳过表头。
		// 少了它,tail 就会把**第一条真实数据**吃掉 —— 而输出照样是一行行文本,
		// 看不出少了什么,只在下游少算一个文件时才暴露。
		if long {
			var blocks int64
			for _, it := range items {
				if it.info != nil {
					// 1024 字节块,向上取整。
					blocks += (it.info.Size() + 1023) / 1024
				}
			}
			writeLsTotal(out, blocks, human)
		}

		// 宽度只在长格式下算:短格式不该为此多走一遍。
		var widths lsWidths
		if long {
			widths = lsMeasure(items, human)
		}

		for _, it := range items {
			writeLsEntry(out, it.name, it, long, human, widths)
		}
	}
	return code
}

// lsItem 是待输出的一个条目。短格式时 info 为 nil(那时不该去取它)。
//
// links 在这里**预取一次**而不是等输出时再问:linkCount 要 os.Open,
// 而输出前还要先扫一遍算列宽。若两处各问一次,`ls -l` 一个大目录就会把
// 每个文件的 open/close 做两遍 —— 那是本层最不该有的开销(同一目录下
// 3000 项的微基准里,多一遍 open 就是多一遍)。
type lsItem struct {
	name  string
	info  os.FileInfo // 短格式时为 nil,writeLsEntry 不会碰它
	links int         // 长格式时预取的硬链接数;短格式为 0,不会被用到
}

// lsWidths 是长格式各**数值列**的宽度,用来右对齐。
//
// 为什么要专门算一遍再输出:GNU `ls -l`(以及 Git Bash 里的实现)对数值列做右对齐,
// 实测真输出里大小是 `   6` / `5000` / `   0` —— 同一个列表里按最大宽度补齐。
// 不做对齐时字段数不变、`$NF` 仍拿得到名字,但人的肉眼读起来会错行,而且与真输出
// 的空白数量不同,「逐字节可比」这个目标就打了折。
//
// 属主列**不参与**对齐:Windows 账户名可能含空格(实测本机是 `Test User`),按字符宽度
// 补齐只会让这一列看起来更乱;真 bash 的属主名在一个列表里恒定(MSYS 一律显示当前用户),
// 原样输出就等于"对齐"。
type lsWidths struct {
	links int
	group int
	size  int
}

// lsMeasure 扫一遍条目,算出各数值列的最大宽度。
//
// 只在长格式下调用;短格式连 info 都没有,不该走这里。
// 链接数从 it.links 读(已在收集时预取),这里不再 open 文件。
func lsMeasure(items []lsItem, human bool) lsWidths {
	// 两个数值列的宽度都至少 1 位,免得零条目时格式串退化成 `%0d`(会输出空串)。
	w := lsWidths{links: 1, size: 1}
	if _, gid := ownerIDs(); len(strconv.Itoa(gid)) > w.group {
		w.group = len(strconv.Itoa(gid))
	}
	for _, it := range items {
		if it.info == nil {
			continue
		}
		if n := len(strconv.Itoa(it.links)); n > w.links {
			w.links = n
		}
		if n := len(output.FormatBytes(it.info.Size(), human)); n > w.size {
			w.size = n
		}
	}
	return w
}

// writeLsEntry 输出一个条目。
//
// 长格式 7 列,与真 Git Bash 对齐:`权限 链接数 属主 属组 大小 时间 名字`,单空格分隔。
// 实测真输出(注意属主名 `Test User` 本身带空格,这是真的):
//
//	-rw-r--r-- 1 Test User 197121    6 Oct 18 00:22 a.txt
//	-rw-r--r-- 1 Test User 197121 5000 Oct 18 00:22 big.bin
//
// 数值列(链接数/属组/大小)按 lsWidths 右对齐,其余原样 —— 属主列不对齐的理由见 lsWidths。
//
// 大小走 output.FormatBytes(…, human),默认是纯字节数:
// "86,488 K" 那种字符串看着像数字却不是数字,只给人眼好看,给解析器添乱。
//
// 性能约束(别破坏):短格式必须**零额外系统调用**。linkCount 会 os.Open,
// 所以它只能在 `long` 为真的路径上调用 —— 见函数体里第一段的提前 return。
func writeLsEntry(w io.Writer, name string, it lsItem, long, human bool, widths lsWidths) {
	if !long {
		fmt.Fprintln(w, name)
		return
	}
	// 只取 gid:uid 不单独占一列 —— GNU `ls -l` 的第 3 列是**属主**(名字)、
	// 第 4 列是**属组**(数字)。uid 只在内部由 ownerIDs 算出来,用不上。
	_, gid := ownerIDs()
	fmt.Fprintf(w, "%s %*d %s %*d %*s %s %s\n",
		it.info.Mode().String(),
		widths.links, it.links,
		ownerName(),
		widths.group, gid,
		widths.size, output.FormatBytes(it.info.Size(), human),
		lsTime(it.info.ModTime()),
		name,
	)
}

// writeLsTotal 输出 GNU `ls -l` 的 `total` 表头行。
//
// 数值是**近似**:真 bash 的 total 是文件系统块分配数,实测 1B→1、513B→1、
// 1000B→4、5000B→8、200000B→196 —— 它依赖 NTFS 簇大小(4096)与 MFT 小文件驻留,
// 不同盘/不同簇大小下都不一样,**不可跨盘复现**。我们按 1024 字节块向上取整求和:
// 行在、位置对、`tail -n +2` 正确跳过,但具体数字可能与真 bash 不同。
//
// 这一取舍是刻意的:精确复刻要拿簇大小 + 压缩后分配大小,代码重且依旧在不同文件系统上
// 走样;而 total 是**给人看的汇总**,没有脚本会依赖它的确切值(依赖的是"这一行存在")。
func writeLsTotal(w io.Writer, blocks int64, human bool) {
	if human && blocks > 0 {
		// 真 bash `ls -lh` 对总和 2 块打的是 `total 2.0K`,而不是 `total 2`。
		fmt.Fprintf(w, "total %s\n", output.FormatBytes(blocks*1024, true))
		return
	}
	// blocks == 0 时 `-h` 也打 `total 0`(实测空目录的 `ls -lh` 不给 `0B`)。
	fmt.Fprintf(w, "total %d\n", blocks)
}

// lsTime 按 bash 的两种形态格式化修改时间。
//
// 实测真 Git Bash(Git Bash 用的是 GNU coreutils 的 ls):
//   - `t` 落在 **[现在-6个月, 现在]** 区间内 → `Oct 18 00:15`(布局 "Jan _2 15:04")
//   - 其它(6 个月前的旧文件,**以及任何未来时间**)→ `Mar  5  2020` / `Jun  1  2027`
//     (布局 "Jan _2  2006",年份前多一个空格)
//
// 「任何未来时间都给年份」这一条是**实测**出来的,两个错误都在这里被纠正过:
//  1. 直觉写法 `time.Since(t) > 180*24*time.Hour` 对未来返回**负数**,
//     比较为假 → 落到时分分支,与真 bash 不一致(这是修之前的 bug);
//  2. 我随后以为是「GNU 容忍 1 小时内的未来」,写成 `t.After(now.Add(time.Hour))` ——
//     但实测把 mtime 设成 **30 分钟后**,真 bash 给的是 `Oct 18  2026`,照样带年份。
//     所以容差是 **0**:只要 t > now 就走年份分支。
//
// 阈值 6 个月用 180 天近似(GNU 算的是真正的"6 个日历月",在月末附近会差一两天)。
// 这点近似写在注释里,免得下一个人以为我们已经逐字复刻了 GNU 的全部时间规则。
// 为什么不用 Go 的 "2006-01-02 15:04:05":那是 ISO 风格,而 `ls -l` 的消费者
// (人工阅读、`awk '{print $6,$7,$8}'`、以及大量 shell 片段)都按 ls 的老格式来切。
func lsTime(t time.Time) string {
	now := time.Now()
	// GNU ls:落在 [6 个月前, 现在] 之外就显示年份 —— 注意未来侧无容差。
	if t.Before(now.Add(-180*24*time.Hour)) || t.After(now) {
		return t.Format("Jan _2  2006")
	}
	return t.Format("Jan _2 15:04")
}
