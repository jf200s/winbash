// `ls -l` 权限位列的测试。
//
// 为什么单独一个文件:这一列原先直接用 Go 的 `info.Mode().String()`,在 Windows 上
// 给 `-rw-rw-rw-` / `drwxrwxrwx` —— 与真 Git Bash 差得远,而且会误导人以为
// group/other 都能写。改成复刻 MSYS 算法后,规则有 5 个分支 + 3 条可执行判据,
// 每一条都值得单独钉住。
//
// 全部断言都来自**真 Git Bash 实测**(2026-10-18),实测值写在每条注释里。

package builtins

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// writeProbe 写一个内容指定的文件,可选设 Windows 只读属性。
func writeProbe(t *testing.T, dir, name string, data []byte, readonly bool) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if readonly {
		// Windows 只有只读位;Go 的 Chmod 用"去掉所有 w 位"表达它。
		if err := os.Chmod(p, 0o444); err != nil {
			t.Fatal(err)
		}
	}
}

// lsPerm 取参数**本身**的权限串(用 `-ld`,不展开目录)。
//
// 为什么必须带 `-d`:不带它时 `ls -l subdir` 列的是 subdir 里的**内容**,
// 首行还是 `total N`。这正是上一轮补 `-d` 的用处。
func lsPerm(t *testing.T, dir, name string) string {
	t.Helper()
	out, errw, code := lsIn(t, dir, "-ld", name)
	if code != 0 {
		t.Fatalf("ls -ld %s: code=%d errw=%q", name, code, errw)
	}
	fields := splitFields(out)
	if len(fields) == 0 {
		t.Fatalf("ls -ld %s 没有输出", name)
	}
	return fields[0]
}

// splitFields 是 strings.Fields 的薄封装(避免测试文件里到处 import strings)。
func splitFields(s string) []string {
	var out []string
	cur := ""
	for _, c := range s {
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			continue
		}
		cur += string(c)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// ★ 权限串的 5 个分支全部要对着真 bash。
//
// 实测(真 Git Bash,umask=022):
//
//	              可写            只读
//	目录          drwxr-xr-x      drwxr-xr-x     ← 只读属性对目录**不生效**
//	文件·可执行   -rwxr-xr-x      -r-xr-xr-x
//	文件·普通     -rw-r--r--      -r--r--r--
//
// 修之前这四种文件情况全是 `-rw-rw-rw-`,两种目录情况全是 `drwxrwxrwx`。
func TestLsModeMatchesMsysRules(t *testing.T) {
	dir := t.TempDir()

	// 目录两个:普通目录、被设成只读的目录。
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "rodir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "rodir"), 0o444); err != nil {
		t.Fatal(err)
	}

	// 文件四个:可写普通 / 只读普通 / 可写可执行 / 只读可执行。
	writeProbe(t, dir, "plain.txt", []byte("x"), false)
	writeProbe(t, dir, "ro.txt", []byte("x"), true)
	writeProbe(t, dir, "prog.exe", []byte("MZ"), false)
	writeProbe(t, dir, "ro_prog.exe", []byte("MZ"), true)

	cases := []struct {
		name string
		want string
		why  string
	}{
		{"subdir", "drwxr-xr-x", "普通目录:0777 &^ 022"},
		{"rodir", "drwxr-xr-x", "只读目录:仍给 drwxr-xr-x —— 只读属性对目录不生效"},
		{"plain.txt", "-rw-r--r--", "可写普通文件:0666 &^ 022"},
		{"ro.txt", "-r--r--r--", "只读普通文件:0444 &^ 022(去掉所有 w 位)"},
		{"prog.exe", "-rwxr-xr-x", "可写可执行:0777 &^ 022"},
		{"ro_prog.exe", "-r-xr-xr-x", "只读可执行:0555 &^ 022"},
	}
	for _, c := range cases {
		if got := lsPerm(t, dir, c.name); got != c.want {
			t.Errorf("%s: 得到 %q,期望 %q(%s)", c.name, got, c.want, c.why)
		}
	}
}

// ★ 权限串必须恒为 10 个字符 —— 脚本靠 `substr($1,1,1)` 判目录。
//
// 这条看着琐碎,但它是权限列唯一的**结构性**不变量:少了/多了字符,下游
// `case "${line:0:1}" in d)` 之类的判断就会静默走错分支。
func TestLsModeIsAlwaysTenChars(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeProbe(t, dir, "f.txt", []byte("x"), false)
	writeProbe(t, dir, "g.exe", []byte("MZ"), false)

	for _, name := range []string{"d", "f.txt", "g.exe"} {
		perm := lsPerm(t, dir, name)
		if len(perm) != 10 {
			t.Errorf("%s: 权限串应当是 10 字符,得到 %d 字符 %q", name, len(perm), perm)
		}
	}
	// 首字符区分目录与文件 —— 这是下游最常用的一位。
	if got := lsPerm(t, dir, "d"); got[0] != 'd' {
		t.Errorf("目录的权限串首字符应当 'd',得到 %q", got)
	}
	if got := lsPerm(t, dir, "f.txt"); got[0] != '-' {
		t.Errorf("普通文件的首字符应当 '-',得到 %q", got)
	}
}

// ★ 「可执行」的判定规则:`.exe` 扩展名 / MZ 头 / `#!` 头,三者之一即可。
//
// 实测(造了 28 种扩展名 + 10 种文件头的矩阵,逐条对照真 Git Bash):
//
//	扩展名 .exe            → -rwxr-xr-x   (唯一靠扩展名就够的)
//	MZ 头(任意扩展名)     → -rwxr-xr-x
//	#! 头(任意扩展名)     → -rwxr-xr-x
//	.com / .dll 内容是文本 → -rw-r--r--   (不是靠扩展名,靠头)
//	.bat / .cmd / .ps1     → -rw-r--r--   (**不可执行**)
//	.sh / .py 无 #! 头     → -rw-r--r--
//	ELF / PNG / ZIP 头     → -rw-r--r--
//
// 为什么要这么较真:「`ls -l` 能不能用来判可执行」完全取决于这一列。错判会让
// AI 以为一个脚本能跑(或反过来以为不能),而 Git Bash 的 usr\bin 里有 44 个
// 无扩展名的 shebang 脚本 —— 只看扩展名会把它们全标成不可执行。
func TestLsModeExecutableDetection(t *testing.T) {
	dir := t.TempDir()

	type probe struct {
		name string
		data []byte
		want string
		why  string
	}
	probes := []probe{
		// 靠扩展名。
		{"a.exe", []byte("x"), "-rwxr-xr-x", "`.exe` 扩展名即可执行,内容不必是 MZ"},
		// 靠文件头(扩展名无关)。
		{"b.com", []byte("MZ\x00\x00"), "-rwxr-xr-x", "MZ 头 → 可执行(.com 扩展名不是关键)"},
		{"c.dll", []byte("MZ\x00\x00"), "-rwxr-xr-x", "MZ 头 → 可执行(.dll 也可执行)"},
		{"d.txt", []byte("MZ\x00\x00"), "-rwxr-xr-x", "MZ 头 → 可执行,哪怕扩展名是 .txt"},
		{"e_noext", []byte("MZ\x00\x00"), "-rwxr-xr-x", "MZ 头 + 无扩展名 → 可执行"},
		{"f_shebang", []byte("#!/bin/sh\n"), "-rwxr-xr-x", "`#!` 头 → 可执行"},
		{"g_shebang.txt", []byte("#!/bin/sh\n"), "-rwxr-xr-x", "`#!` 头 + .txt 扩展名 → 可执行"},
		// 不可执行的几类(每类都是容易猜错的)。
		{"h.bat", []byte("@echo off\r\n"), "-rw-r--r--", ".bat **不可执行**(与 Cygwin 不同)"},
		{"i.cmd", []byte("@echo off\r\n"), "-rw-r--r--", ".cmd **不可执行**"},
		{"j.ps1", []byte("Write-Host hi\n"), "-rw-r--r--", ".ps1 **不可执行**"},
		{"k.sh", []byte("echo hi\n"), "-rw-r--r--", ".sh 但**没有 #! 头** → 不可执行"},
		{"l.py", []byte("print(1)\n"), "-rw-r--r--", ".py 但**没有 #! 头** → 不可执行"},
		{"m.com_txt", []byte("x"), "-rw-r--r--", ".com 内容是文本 → 不可执行(所以不是靠扩展名)"},
		{"n.dll_txt", []byte("x"), "-rw-r--r--", ".dll 内容是文本 → 不可执行"},
		{"o_elf", []byte("\x7fELF\x00\x00"), "-rw-r--r--", "ELF 头**不可执行**"},
		{"p_png", []byte("\x89PNG\r\n\x1a\n"), "-rw-r--r--", "PNG 头不可执行"},
		{"q_zip", []byte("PK\x03\x04"), "-rw-r--r--", "ZIP 头不可执行"},
		{"r_empty", []byte(""), "-rw-r--r--", "空文件不可执行"},
		{"s_text", []byte("just text\n"), "-rw-r--r--", "纯文本不可执行"},
	}
	for _, p := range probes {
		writeProbe(t, dir, p.name, p.data, false)
	}
	for _, p := range probes {
		if got := lsPerm(t, dir, p.name); got != p.want {
			t.Errorf("%s: 得到 %q,期望 %q(%s)", p.name, got, p.want, p.why)
		}
	}
}

// ★ `.exe` 扩展名要**大小写不敏感** —— Windows 上 `FOO.EXE` 一样是可执行文件。
func TestLsModeExeExtensionIsCaseInsensitive(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"upper.EXE", "mixed.Exe", "lower.exe"} {
		writeProbe(t, dir, n, []byte("x"), false)
	}
	for _, n := range []string{"upper.EXE", "mixed.Exe", "lower.exe"} {
		if got := lsPerm(t, dir, n); got != "-rwxr-xr-x" {
			t.Errorf("%s: 得到 %q,期望 -rwxr-xr-x(扩展名判定要大小写不敏感)", n, got)
		}
	}
}

// ★ umask 固定按 022 算(winbash 没有 umask 内建,见 lsmode.go 的说明)。
//
// 这条断言的意义不是"验证 umask 正确",而是**钉住那个常量**:哪天有人把它改成
// 077 或 0,所有权限串都会变,必须有人明确知道。
//
// 顺带钉住等价关系:0755 / 0644 / 0444 就是真 bash 在 umask=022 下的那三档。
func TestLsModeUsesDefaultUmask(t *testing.T) {
	if defaultUmask != 0o022 {
		t.Fatalf("defaultUmask 应当是 022(Git Bash 的默认值),现在是 %#o —— "+
			"改它会让所有权限串都变,README 的说明也要跟着改", defaultUmask)
	}

	dir := t.TempDir()
	writeProbe(t, dir, "f.txt", []byte("x"), false)
	// 022 的效果:group/other 的 w 位被掩掉。若 umask 变成 0,这里会是 -rw-rw-rw-。
	if got := lsPerm(t, dir, "f.txt"); got != "-rw-r--r--" {
		t.Fatalf("umask 022 下普通文件应当是 -rw-r--r--,得到 %q", got)
	}
}

// ★ 权限位不能再泄漏 Windows 的 `-rw-rw-rw-`。
//
// 这条是**回归护栏**:修之前的实现是 `info.Mode().String()`,它在 Windows 上给
// `-rw-rw-rw-` / `drwxrwxrwx`。若哪天有人图省事改回去,这条会红,而且错得很明显。
func TestLsModeDoesNotLeakWindowsMode(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeProbe(t, dir, "f.txt", []byte("x"), false)

	for _, name := range []string{"d", "f.txt"} {
		perm := lsPerm(t, dir, name)
		if perm == "-rw-rw-rw-" || perm == "drwxrwxrwx" {
			t.Fatalf("%s: 权限串是 %q —— 这是 Go 的 info.Mode().String() 在 Windows 上的"+
				"原始输出,不是 MSYS 的算法(见 lsmode.go)", name, perm)
		}
	}
}

// ★ 「可执行」与「只读」组合的正确性(两个位互相独立)。
//
// 实测:只读 + 可执行 → `-r-xr-xr-x`(不是 -r--r--r--,也不是 -rwxr-xr-x)。
// 这条容易被写成"只读就全清 w 和 x"或"可执行就不看只读"。
func TestLsModeReadonlyAndExecCombine(t *testing.T) {
	dir := t.TempDir()
	writeProbe(t, dir, "ro_shebang", []byte("#!/bin/sh\n"), true)
	writeProbe(t, dir, "ro_text.txt", []byte("plain"), true)

	if got := lsPerm(t, dir, "ro_shebang"); got != "-r-xr-xr-x" {
		t.Fatalf("只读 + 可执行 应当是 -r-xr-xr-x,得到 %q —— "+
			"只读只清 w 位,x 位要保留", got)
	}
	if got := lsPerm(t, dir, "ro_text.txt"); got != "-r--r--r--" {
		t.Fatalf("只读 + 不可执行 应当是 -r--r--r--,得到 %q", got)
	}
}

// ★ 目录的 size 列恒为 0,而 `total` 行仍按真实块数累加 —— 两处必须分开。
//
// 实测真 Git Bash(2026-10-18):
//
//	withfiles 目录(内含 30 个文件)  → size 0     (winbash 曾给 4096)
//	/c/Windows                        → size 0     (曾给 28672)
//	`..`(TEMP 根)                    → size 0     (曾给 65536)
//	`ls -la` 的 total                 → 85,与 winbash **一致**
//
// 也就是说:size 列要收成 0,但 total 不能跟着收 —— 一起改会把已经对齐的 total
// 弄坏(temp 根 65536 一去掉,total 会掉六十多块)。
//
// **这条测试返工过一次**(变异验证抓出来的,记下来):
// 第一版用 `t.TempDir()` 建一个**空**子目录来验 size=0,结果把"目录不归零"
// 那个变异放过了 —— Windows 上空目录的 info.Size() 恰好就是 0,归零与否看不出
// 区别。所以这里必须让目录里**有文件**(实测含 30 个文件的目录 size 是 4096)。
func TestLsDirSizeIsZeroButTotalStillCounts(t *testing.T) {
	dir := t.TempDir()

	// 子目录里放足够多的文件,让它自己的 info.Size() 明显非零
	// (NTFS 目录索引随条目增长;空目录是 0,这一点是返工的直接原因)。
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		name := filepath.Join(sub, "f"+strconv.Itoa(i)+".txt")
		if err := os.WriteFile(name, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// 当前目录也放些文件,保证 `.` 的真实大小非零(后面验 total 要用)。
	if err := os.WriteFile(filepath.Join(dir, "big.bin"), make([]byte, 5000), 0o644); err != nil {
		t.Fatal(err)
	}

	// 先确认这个前提成立:子目录的真实 info.Size() 确实非零。
	// 若哪天在某台机器上它就是 0(比如 ReFS),这条测试会失去鉴别力 ——
	// 与其静默变成恒真,不如直接跳过并说清楚。
	fi, err := os.Stat(sub)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() == 0 {
		t.Skipf("本机 NTFS 上含 40 个文件的目录 info.Size() 仍是 0," +
			"这条测试无法区分「归零」与「不归零」,跳过")
	}

	// ① 目录的 size 列必须是 0。
	out, errw, code := lsIn(t, dir, "-ld", "sub")
	if code != 0 {
		t.Fatalf("-ld sub: code=%d errw=%q", code, errw)
	}
	fields := splitFields(out)
	if len(fields) < 5 {
		t.Fatalf("-ld sub 输出列数不对: %q", out)
	}
	// **不能按位置取** `fields[4]` —— 属主名 `Test User` 本身含空格,会把这段撑成两位,
	// 于是 size 落到第 6 位而不是第 5 位。这是 README 里专门警告过的陷阱
	// (「不要用 awk '{print $3}' 按位置取字段」),写这条测试时自己踩了一次。
	//
	// 从**行尾**倒数才稳:最后 1 段是名字,它前面固定是 3 段时间(`Oct 18 02:45`
	// 或 `Oct 18  2026`),再前面就是 size。
	if size := fields[len(fields)-5]; size != "0" {
		t.Fatalf("目录的 size 列应当是 0(真 bash 恒给 0),得到 %q —— "+
			"它的真实 info.Size() 是 %d,所以这条能区分「归零」与「不归零」。完整行: %q",
			size, fi.Size(), out)
	}

	// ② total 行必须仍存在、且**不因 size 列归零而退化**。
	//
	// **这里返工过一次,把错误的判据与理由记下来:**
	// 我原本想用「`ls -la` 的 total 应当大于 `ls -l` 的 total」来区分 total 用的是
	// info.Size() 还是 displaySize —— 以为 `.` 与 `..` 两个目录会贡献块。实测发现
	// **这个前提是错的**:真 Git Bash 里两者相等(`ls -l` 与 `ls -la` 都是 `total 5`)。
	// `.` 和 `..` 在 MSYS 的 total 里贡献 0 块。
	//
	// 而且在非 `-a` 场景下两个来源**本来就相等**:条目走 `DirEntry.Info()`,而它在
	// Windows 上对目录给的 size 就是 0(GetFileAttributesEx 才给 4096)。所以
	// 「total 改用 displaySize」是个**功能等价变异**,抓不到不是测试缺口 —— 实测确认过。
	// (`-a` 场景下 `.` 与 `..` 走 os.Stat 会拿到非零 size,那时改用 displaySize 确实会
	//  改变 total 数值;但**那个数值本来就与真 bash 不一致**(真 bash 让 . 与 .. 贡献 0 块),
	//  属既存差异,不适合拿来当断言锚点。)
	//
	// 所以这里只断言真正可断言的部分:total 行在、位置对、且**非零**(有文件在)。
	// total 的精确数值本来就已知是近似值(见 writeLsTotal 的注释与 README)。
	out, errw, code = lsIn(t, dir, "-l")
	if code != 0 {
		t.Fatalf("-l: code=%d errw=%q", code, errw)
	}
	if got := totalBlocks(t, out); got == 0 {
		t.Fatalf("`ls -l` 的 total 不该是 0 —— 目录里有 5000 字节的文件。"+
			"若目录的 size 归零被误用到了 total 累加上,就会变成这样。完整输出: %q", out)
	}
}

// totalBlocks 从 `ls -l` 输出里取 total 行的数值。
func totalBlocks(t *testing.T, out string) int64 {
	t.Helper()
	first := firstLine(out)
	if !strings.HasPrefix(first, "total ") {
		t.Fatalf("第一行该是 total,得到 %q(完整输出 %q)", first, out)
	}
	n, err := strconv.ParseInt(strings.TrimPrefix(first, "total "), 10, 64)
	if err != nil {
		t.Fatalf("total 行的数值解析失败: %q (%v)", first, err)
	}
	return n
}
