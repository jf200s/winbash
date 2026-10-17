package builtins

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLsDefaultIsOneNamePerLine(t *testing.T) {
	// 默认输出必须与 bash 一致：一行一个名字，无装饰
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("x"), 0o644)

	var out bytes.Buffer
	if code := Ls(context.Background(), dir, nil, &out, io.Discard); code != 0 {
		t.Fatalf("exit code %d", code)
	}
	got := strings.Fields(out.String())
	if len(got) != 2 || got[0] != "a.txt" {
		t.Fatalf("got %q", out.String())
	}
}

func TestLsHandlesChineseAndSpaceInPath(t *testing.T) {
	// Review Focus 第 1 条：这台机器的用户名就含空格，中文路径是日常
	dir := filepath.Join(t.TempDir(), "命令行 目录")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "中文 文件.txt"), []byte("x"), 0o644)

	var out bytes.Buffer
	if code := Ls(context.Background(), dir, nil, &out, io.Discard); code != 0 {
		t.Fatalf("exit code %d", code)
	}
	if !strings.Contains(out.String(), "中文 文件.txt") {
		t.Fatalf("got %q", out.String())
	}
}

func TestLsLongFormatGivesRawByteSizeNotCommaString(t *testing.T) {
	// 值保证：不能出现 "86,488 K" 这类带千分位的字符串
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "f"), bytes.Repeat([]byte("x"), 1234), 0o644)

	var out bytes.Buffer
	Ls(context.Background(), dir, []string{"-l"}, &out, io.Discard)
	if !strings.Contains(out.String(), "1234") {
		t.Fatalf("want raw byte count: %q", out.String())
	}
	if strings.Contains(out.String(), ",") {
		t.Fatalf("must not contain thousands separators: %q", out.String())
	}
}

// TestLsLongFormatHasSevenColumns 钉住 -l 的列结构:权限 链接数 属主 属组 大小 时间 名字。
//
// 修之前只有四列(权限 大小 修改时间 名字)、也没有 total 行 —— 依赖 `ls -l` 的脚本
// (`tail -n +2`、`awk '{print $5, $NF}'`、`ls -l | wc -l`)会整体错位,而错位是
// **静默**的:输出照样是一行行文本,看不出少了三列,只在下游算错值时才暴露。
//
// 这里刻意不按位置断言属主/属组的**具体值**(那依赖运行机器,见
// TestLsLongFormatOwnerIsNotAFixedString),只钉形态:total 行在第一行、数据行
// 至少 7 段、首列是 10 字符权限串、行尾是名字、大小作为独立字段出现。
func TestLsLongFormatHasSevenColumns(t *testing.T) {
	dir := t.TempDir()
	// 5 字节,方便把「大小」与链接数(恒为 1)、组号区分开。
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if code := Ls(context.Background(), dir, []string{"-l"}, &out, io.Discard); code != 0 {
		t.Fatalf("exit code %d", code)
	}

	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) < 2 {
		t.Fatalf("至少要有 total 行 + 一行数据, got %q", out.String())
	}
	// GNU ls 的 total 行必须存在且**在第一位** —— 脚本靠 `tail -n +2` 跳它;
	// 换成"末尾补一行"或"没有",那些脚本就多算/少算一条。
	if !strings.HasPrefix(lines[0], "total ") {
		t.Fatalf("第一行该是 GNU ls 的 total 行(脚本靠 tail -n +2 跳它): %q", out.String())
	}

	data := lines[1]
	fields := strings.Fields(data)
	// 不能用 `len(fields) == 7` 硬等:属主名本身可能含空格(如本机的 `Test User`),
	// 那行会切出 8 段甚至更多。所以是「>= 7」。
	if len(fields) < 7 {
		t.Fatalf("7 列至少切出 7 段(属主名可能含空格,只会更多):%q → %d 段", data, len(fields))
	}
	if len(fields[0]) != 10 || fields[0][0] != '-' {
		t.Fatalf("首列该是 10 字符权限串且以 '-' 开头(普通文件):%q", fields[0])
	}
	if !strings.HasSuffix(data, " a.txt") {
		t.Fatalf("行尾该是名字: %q", data)
	}
	hasSize := false
	for _, f := range fields {
		if f == "5" {
			hasSize = true
			break
		}
	}
	if !hasSize {
		t.Fatalf("大小 5 该作为一个独立字段出现(不能与别列粘在一起):%q", data)
	}
}

// TestLsLongFormatHasNoTotalForSingleFile 钉住「列单个文件时不打 total 行」。
//
// 真 bash 实测:`ls -l a.txt` 只有一行、没有 total —— total 是「目录里各条目占的
// 块数之和」,列单个文件时没有这个汇总语境。改成 7 列后最容易犯的错是在
// writeLsEntry 外面无条件先打一行 total,于是 `ls -l f | wc -l` 从 1 变 2;
// 那种偏差在下游只表现为「多读了一行」,极难归因。
func TestLsLongFormatHasNoTotalForSingleFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if code := Ls(context.Background(), dir, []string{"-l", "a.txt"}, &out, io.Discard); code != 0 {
		t.Fatalf("exit code %d", code)
	}
	got := out.String()
	if strings.Contains(got, "total") {
		t.Fatalf("列单个文件不该有 total 行: %q", got)
	}
	if lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n"); len(lines) != 1 {
		t.Fatalf("该只有一行, got %d 行: %q", len(lines), got)
	}
}

// TestLsShortFormatStaysOneNamePerLine 防的是「补列时把短格式一起改了」。
//
// 短格式(`ls` 不带 -l)是 ① 层最重要的兼容点:一行一个纯文件名,无表头、无权限串、
// 无 total —— AI 对它的解析预期极强(常常 `ls` 完就直接拼文件名)。
// 长格式从 4 列改 7 列时,最容易把「对齐/列宽/表头」那套逻辑提到两条路径共用的地方,
// 于是短格式也开始带装饰,而短格式的错法同样是静默的(名字里多出几个字符)。
func TestLsShortFormatStaysOneNamePerLine(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("x"), 0o644)

	var out bytes.Buffer
	if code := Ls(context.Background(), dir, nil, &out, io.Discard); code != 0 {
		t.Fatalf("exit code %d", code)
	}
	got := out.String()
	if strings.Contains(got, "total") || strings.Contains(got, "rw") {
		t.Fatalf("短格式不该带任何装饰(total/权限串都不行): %q", got)
	}
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("该正好两行, got %d 行: %q", len(lines), got)
	}
	// 逐行**整行相等**:多一个前导空格或尾随空格都算破坏兼容。
	for i, want := range []string{"a.txt", "b.txt"} {
		if lines[i] != want {
			t.Fatalf("第 %d 行该是纯文件名 %q, got %q", i+1, want, lines[i])
		}
	}
}

// TestLsLongFormatOwnerIsNotAFixedString 钉住「属主 + 属组两列确实存在,且是文本」。
//
// 这里**刻意不断言具体的账户名/组号**:它们依赖运行机器(本机是 `Test User` / `197121`),
// 换台机器(英文用户名、域账户、CI runner)就会红 —— 一条「谁跑都红」的测试最后
// 只会被加 skip 或被删掉,等于没钉住。所以改为钉结构:权限列与名字列之间至少有
// 4 段(链接数 属主 属组 大小),也就是属主、属组各自至少占一段非空文本。
//
// 属主名可能含空格(`Test User`),所以这里只按**段数**宽松断言,绝不按固定下标取字段
// —— `awk '{print $3}'` 那种写法在本项目自己的 README 里就被点名为静默错位陷阱。
func TestLsLongFormatOwnerIsNotAFixedString(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if code := Ls(context.Background(), dir, []string{"-l"}, &out, io.Discard); code != 0 {
		t.Fatalf("exit code %d", code)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) < 2 {
		t.Fatalf("至少要有 total 行 + 一行数据, got %q", out.String())
	}
	fields := strings.Fields(lines[1])
	if len(fields) < 7 {
		t.Fatalf("段数该 >= 7(属主名含空格时更多), got %d: %q", len(fields), lines[1])
	}
	// fields[0] 是权限、最后一段是名字;中间至少有「链接数 属主 属组 大小」4 段
	// (时间本身还会再切出 2~3 段),所以属主与属组一定在,且都不是空串。
	if middle := fields[1 : len(fields)-1]; len(middle) < 4 {
		t.Fatalf("权限与名字之间该至少有 链接数/属主/属组/大小 4 段, got %d: %q", len(middle), lines[1])
	}
}

// TestLsHumanFlagFormatIsDocumentedNotGNU 钉住 `-h` 的大小列**仍与 GNU 不同**。
//
// 这条不是"验收新功能",而是把一条**已知差异**钉住,免得两件事发生:
//  1. 有人以为 `-lh` 已经与真 bash 一致(README 那条里说明了它是例外);
//  2. `output.FormatBytes` 哪天被改成 GNU 规则(小于 1K 不带单位、≥10 取整),
//     这条测试会红,提醒去同步 README —— 反过来也一样。
//
// 实测真 Git Bash `ls -lh`:
//
//	-rw-r--r-- 1 Test User 197121    5 Oct 18 00:30 a5.txt     ← 没有 B
//	-rw-r--r-- 1 Test User 197121 196K Oct 18 00:30 big.bin    ← 取整,不是 195.3K
//
// 我们给的是 `5B` / `195.3K`。字段数与顺序不受影响,只是文本形态。
func TestLsHumanFlagFormatIsDocumentedNotGNU(t *testing.T) {
	dir := t.TempDir()
	// 5 字节:小于 1K,用来暴露单位后缀差异。
	if err := os.WriteFile(filepath.Join(dir, "small.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if code := Ls(context.Background(), dir, []string{"-lh"}, &out, io.Discard); code != 0 {
		t.Fatalf("exit code %d", code)
	}
	got := out.String()
	// 断言的是**当前已知行为**,不是"理想行为":小于 1K 带 B。
	if !strings.Contains(got, "5B") {
		t.Fatalf("`-lh` 对小于 1K 的文件当前会给单位后缀(5B);"+
			"若这里改成不带单位的 5,说明 FormatBytes 变了 —— 请同步 README 的 ls -l 那条: %q", got)
	}
	// 同时确认它仍然是 7 列 + total 行,没有因为 -h 把结构弄坏。
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if len(lines) < 2 || !strings.HasPrefix(lines[0], "total ") {
		t.Fatalf("`-lh` 也必须有 total 行且在前: %q", got)
	}
	if n := len(strings.Fields(lines[1])); n < 7 {
		t.Fatalf("`-lh` 的数据行也该 >= 7 段, got %d: %q", n, lines[1])
	}
}

// TestLsTimeHandlesFutureAndOldFiles 钉住 lsTime 三种分支的边界。
//
// 修之前的写法是 `if time.Since(t) > 180*24*time.Hour { 带年份 }`。
// 这对**未来时间**返回的是负数(负的 Duration),比较结果为假,于是落到
// 时分分支 —— 而真 bash 对**任何**未来时间都给年份。
//
// 边界值是实测出来的,两个错误都在这里被纠正过(所以这条测试才值得存在):
//  1. 旧实现:`time.Since` 对未来给负数 → 未来文件错给时分;
//  2. 我第一版"修法"以为 GNU 容忍 1 小时内的未来,写了 `After(now.Add(time.Hour))` ——
//     但实测把 mtime 设成 **30 分钟后**,真 bash 给的是 `Oct 18  2026`,照样带年份。
//     容差是 **0**。
//
// 这条测试直接测 lsTime(不通过 Ls),因为要精确控制时间值 —— 走 Ls 就得造文件
// 再改 mtime,既慢又受文件系统时间精度影响。
func TestLsTimeHandlesFutureAndOldFiles(t *testing.T) {
	now := time.Now()

	cases := []struct {
		name string
		t    time.Time
		want string // Go 的时间布局,便于读
		desc string
	}{
		{
			name: "刚刚",
			t:    now.Add(-time.Minute),
			want: "Jan _2 15:04",
			desc: "近期文件给时分",
		},
		{
			name: "五个月前",
			t:    now.Add(-150 * 24 * time.Hour),
			want: "Jan _2 15:04",
			desc: "6 个月内仍给时分",
		},
		{
			name: "七个月前",
			t:    now.Add(-210 * 24 * time.Hour),
			want: "Jan _2  2006",
			desc: "超过 6 个月给年份",
		},
		{
			name: "三十分钟后",
			t:    now.Add(30 * time.Minute),
			want: "Jan _2  2006",
			desc: "未来时间**无容差** —— 实测真 bash 对 30 分钟后的文件也给年份",
		},
		{
			name: "一年后",
			t:    now.Add(365 * 24 * time.Hour),
			want: "Jan _2  2006",
			desc: "更远的未来同样给年份",
		},
	}

	for _, c := range cases {
		got := lsTime(c.t)
		want := c.t.Format(c.want)
		if got != want {
			t.Errorf("%s(%s): lsTime 给 %q, 期望 %q(%s)",
				c.name, c.desc, got, want, c.want)
		}
	}
}

// TestOwnerIDsAreNonZero 钉住「uid/gid 真的取到了」。
//
// 这条测试是补出来的,因为一次真实回归溜过了所有其它用例:
// 我给 tokenSIDCopy 加上界校验时,把 TokenUser 的头部大小 16 **硬编码**给了
// TokenPrimaryGroup 用 —— 而后者只有一个 PSID(8 字节头)。于是 primaryGroup
// 的校验恒为「越界」、返回 nil,gid 变成 0。所有 ls 测试都绿,因为
// TestLsLongFormatOwnerIsNotAFixedString **刻意不对齐具体的组号**(那依赖机器)。
//
// 而端到端对比才暴露出来:真 bash 给 197121,winbash 给 0。
//
// 所以这里断言的是**与机器无关的不变量**:MSYS 的 uid/gid 是 0x30000 + RID,
// 而 RID 是个 uint32 的非负值,0x30000+0 = 196608 > 0 —— 所以取到了就一定 > 0,
// 取不到才会是 0(loadOwner 的零值)。这条在任何机器上都成立,不会红。
func TestOwnerIDsAreNonZero(t *testing.T) {
	uid, gid := ownerIDs()
	if uid <= 0 {
		t.Errorf("uid 取到 %d —— tokenSIDCopy(TokenUser) 失败了;"+
			"0 是 ownerUID 的零值,说明令牌读取没成功", uid)
	}
	if gid <= 0 {
		t.Errorf("gid 取到 %d —— tokenSIDCopy(TokenPrimaryGroup) 失败了。"+
			"注意这条路的结构头只有 8 字节(单个 PSID),与 TokenUser 的 16 字节不同,"+
			"上界校验不能用同一个常数", gid)
	}
	if name := ownerName(); name == "" || name == "unknown" {
		t.Errorf("ownerName 取到 %q —— LookupAccountSidW 或令牌读取失败", name)
	}
}

// TestOwnerIsProcessWideConstant 钉住那个反直觉的语义:属主是**进程级**常量。
//
// 实测依据:MSYS 的 ls 不读文件的属主 SID —— 对 C:\Windows\notepad.exe
// (属主是 TrustedInstaller)真 Git Bash 也显示 `Test User`。所以同一个进程里
// 问多少次、问哪个文件,答案都一样。
//
// 这条测试钉的是「两次调用返回同一结果」,防的是有人把它改成"逐文件查 ACL"
// 那套 —— 那不但每个条目多两次 Win32 调用,结果还会与真 bash 不一致。
func TestOwnerIsProcessWideConstant(t *testing.T) {
	n1 := ownerName()
	u1, g1 := ownerIDs()
	n2 := ownerName()
	u2, g2 := ownerIDs()
	if n1 != n2 || u1 != u2 || g1 != g2 {
		t.Fatalf("两次调用结果不同(%q/%d/%d vs %q/%d/%d)—— "+
			"属主应当是进程级常量、由 sync.Once 缓存", n1, u1, g1, n2, u2, g2)
	}
}

func TestLsRejectsUnknownFlag(t *testing.T) {
	// Global Constraint：不认识的 flag 必须报错，不得静默忽略
	var errw bytes.Buffer
	code := Ls(context.Background(), t.TempDir(), []string{"--definitely-not-a-flag"}, io.Discard, &errw)
	if code == 0 {
		t.Fatal("expected non-zero exit for unknown flag")
	}
	if errw.Len() == 0 {
		t.Fatal("expected an error message")
	}
}

// ls 不做递归是对的(① 层只列一层,语义可控),但错误信息要指路。
//
// "不认识的参数: -R" 只说了不行、没说下一步 —— AI 与人都得再花一轮去问"那递归
// 该怎么写"。bash 里的标准解法是 find,直接给出来。
func TestLsRecursivePointsToFind(t *testing.T) {
	for _, arg := range []string{"-R", "--recursive"} {
		var errw bytes.Buffer
		code := Ls(context.Background(), t.TempDir(), []string{arg}, io.Discard, &errw)
		if code == 0 {
			t.Errorf("%s: code=0,应当报用法错误", arg)
		}
		if !strings.Contains(errw.String(), "find") {
			t.Errorf("%s: 该指路到 find,got %q", arg, errw.String())
		}
	}
}

func TestResolvePathAcceptsBothPOSIXAndWindowsForms(t *testing.T) {
	if p, err := ResolvePath(`C:\work`, "/c/Users/x"); err != nil || p != `C:\Users\x` {
		t.Fatalf("posix form: %q %v", p, err)
	}
	if p, err := ResolvePath(`C:\work`, `D:\other`); err != nil || p != `D:\other` {
		t.Fatalf("windows form: %q %v", p, err)
	}
}

func TestResolvePathJoinsRelativeAgainstCwd(t *testing.T) {
	got, err := ResolvePath(`C:\work`, filepath.Join("sub", "f.txt"))
	if err != nil || got != `C:\work\sub\f.txt` {
		t.Fatalf("got %q %v", got, err)
	}
}

func TestLsMissingPathIsAnErrorNotSilence(t *testing.T) {
	// 找不到就报错并给建议，不得静默返回 0 —— 静默是「翻车」的主要形态
	var errw bytes.Buffer
	code := Ls(context.Background(), t.TempDir(), []string{"no-such-entry"}, io.Discard, &errw)
	if code == 0 || errw.Len() == 0 {
		t.Fatalf("code=%d err=%q", code, errw.String())
	}
}

// ★ 单个文件参数的名字必须是**参数原文**,不是 basename。
//
// 真 GNU 对**所有**操作数都输出参数原文(实测真 Git Bash 2026-10-18):
//
//	ls /abs/real.txt  → "/abs/real.txt"
//	ls ./real.txt     → "./real.txt"
//
// 这条一度只在 `ls -d` 分支做对,普通分支给的是 `filepath.Base`,于是同一个二进制里
// 有两种口径。它危害的实际形态:AI 用 `ls <绝对路径>` 拿到裸文件名,再拿它去拼别的
// 路径时就会丢目录。
//
// 为什么这条测试以前没有:既有的 `TestLsDefaultIsOneNamePerLine` 等都用 `t.TempDir()`
// 当 cwd、只传裸名字,所以"Base 与原文相同",测不出区别 —— 是结构性盲区。
func TestLsFileArgumentKeepsVerbatimText(t *testing.T) {
	dir := t.TempDir()
	abs := filepath.Join(dir, "real.txt")
	if err := os.WriteFile(abs, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 绝对路径:必须原样输出,**不能**截成 real.txt。
	out, errw, code := lsIn(t, dir, abs)
	if code != 0 {
		t.Fatalf("绝对路径: code=%d errw=%q", code, errw)
	}
	if got := strings.TrimSpace(out); got != abs {
		t.Fatalf("绝对路径参数应当原样输出 %q(真 bash 就是这么做的),"+
			"得到 %q —— 用 filepath.Base 会截成裸文件名", abs, got)
	}

	// `./` 前缀:必须保留。
	out, _, code = lsIn(t, dir, "./real.txt")
	if code != 0 {
		t.Fatalf("./rel: code=%d", code)
	}
	if got := strings.TrimSpace(out); got != "./real.txt" {
		t.Fatalf("`./real.txt` 应当原样输出,得到 %q —— "+
			"丢了 `./` 之后,下游若直接拼接就会落到别的目录", got)
	}

	// 反向:名字得是**这个**文件的,不能把整个目录都列出来。
	// 判据取 TrimSpace 之后的行数 —— 行尾那个换行不算多行。
	if lines := strings.Split(strings.TrimSpace(out), "\n"); len(lines) != 1 {
		t.Fatalf("传一个文件只该输出一行,得到 %d 行:%q", len(lines), out)
	}
}

// ★ 空字符串路径参数必须**响亮拒绝**,不能静默当成当前目录。
//
// 修之前(实测 2026-10-18)的两个坏形态:
//
//	ls ""     → rc=0,列出**当前目录的全部内容**
//	ls -d ""  → rc=0,输出一个空行
//	真 bash   → rc=2,`ls: cannot access '': No such file or directory`
//
// `ls ""` 那条最坏:调用方(典型是 AI)拿到的是一份看起来完全正常的目录列表,
// 于是会以为列的就是它传的那个目录 —— 静默给出错值,正是项目底线要防的形态。
//
// 根因是 ResolvePath 把 `""` 当 cwd(那个分支是为 `cd ""` 不改目录服务的)。
// 修法是**只收窄 ls 自己的口径**,不动 ResolvePath —— 它对 cd 是对的。
func TestLsRejectsEmptyPathArgument(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, argv := range [][]string{
		{""},
		{"-d", ""},
		{"-l", ""},
		{"a.txt", ""}, // 混在合法参数里也要拒绝,不能只处理第一个
	} {
		out, errw, code := lsIn(t, dir, argv...)
		if code == 0 {
			t.Fatalf("参数 %q: 空字符串路径必须非零退出,否则会静默列出当前目录 "+
				"(out=%q)", argv, out)
		}
		if code != 2 {
			t.Fatalf("参数 %q: 这是用法错误,应当 exit=2,得到 %d", argv, code)
		}
		if !strings.Contains(errw, "空字符串") {
			t.Fatalf("参数 %q: 错误行要点明是空字符串的问题,得到 %q", argv, errw)
		}
		// 绝不能有 stdout —— 一份"看起来正常"的列表正是最坏的结果。
		if strings.TrimSpace(out) != "" {
			t.Fatalf("参数 %q: 被拒时不该有任何 stdout 输出,得到 %q", argv, out)
		}
	}
}
