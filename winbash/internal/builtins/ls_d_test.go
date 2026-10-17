// `ls -d` 的行为测试。
//
// 每一条断言都对应一次**真 Git Bash 实测**(2026-10-18),实测值抄在注释里 ——
// 不是照 GNU 手册推的。`-d` 在 winbash 里曾经是"不认识的参数",而它是极常见的
// 写法(`ls -d */` 只列子目录名、`ls -ld 目录` 只看那个目录的属性)。
//
// 为什么 `-d` 值得单独一个文件:它的语义与"不带 -d"**根本不同** ——
// 不带 -d 时参数是"要展开的目录",带 -d 时参数是"要列出的对象本身"。
// 两条路径共用同一套输出函数,但收集逻辑不能共用。

package builtins

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// lsIn 在 dir 里跑一次 ls,返回 (stdout, stderr, 退出码)。
func lsIn(t *testing.T, dir string, argv ...string) (string, string, int) {
	t.Helper()
	var out, errw bytes.Buffer
	code := Ls(context.Background(), dir, argv, &out, &errw)
	return out.String(), errw.String(), code
}

// lsFixture 造一个固定的目录结构:两个子目录 + 两个普通文件 + 一个点文件。
func lsFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, d := range []string{"dirA", "dirB"} {
		if err := os.Mkdir(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"f1.txt", "f2.txt", ".hidden"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// ★ `-d` 的核心语义:参数**只列它自己**,不展开目录内容。
//
// 实测:
//
//	ls -d            → "."                       (默认参数 "." 也不展开)
//	ls -d dirA       → "dirA"
//	ls -d dirA dirB  → "dirA" "dirB"
//	ls               → "dirA" "dirB" "f1.txt" "f2.txt"   (不带 -d:展开目录)
func TestLsDashDListsTheArgumentNotItsContents(t *testing.T) {
	dir := lsFixture(t)

	out, errw, code := lsIn(t, dir, "-d")
	if code != 0 {
		t.Fatalf("-d 无参数: exit=%d stderr=%q", code, errw)
	}
	if got := strings.TrimSpace(out); got != "." {
		t.Fatalf("ls -d 无参数应当是 %q(默认参数 \".\" 不展开),得到 %q", ".", got)
	}

	out, _, code = lsIn(t, dir, "-d", "dirA")
	if code != 0 {
		t.Fatalf("-d dirA: exit=%d", code)
	}
	if got := strings.TrimSpace(out); got != "dirA" {
		t.Fatalf("ls -d dirA 应当是 %q(不展开成 dirA 里的内容),得到 %q", "dirA", got)
	}

	out, _, code = lsIn(t, dir, "-d", "dirA", "dirB")
	if code != 0 {
		t.Fatalf("-d dirA dirB: exit=%d", code)
	}
	if got := strings.Fields(out); len(got) != 2 || got[0] != "dirA" || got[1] != "dirB" {
		t.Fatalf("ls -d dirA dirB 应当是 [dirA dirB],得到 %q", out)
	}

	// 对照组:不带 -d 时 dirA 要展开(dirA 是空目录 → 没有输出)。
	out, _, code = lsIn(t, dir, "dirA")
	if code != 0 {
		t.Fatalf("ls dirA: exit=%d", code)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("ls dirA(空目录)应当没有输出,得到 %q —— "+
			"若这里有内容,说明 -d 与不带 -d 的行为搞混了", out)
	}
}

// ★ `-d` 输出的名字是**参数原文**,不是 basename。
//
// 实测:
//
//	ls -d dirA/  → "dirA/"      (用户写的尾斜杠保留)
//	ls -d ..     → ".."
//
// 为什么这条重要:`ls -d */` 在真 bash 里靠的就是这个 —— glob 展开出的参数自带
// 尾斜杠,输出也就带着。若实现里换成 filepath.Base,`ls -d */` 会退化成
// `ls` 的效果(丢失"这是目录"的视觉线索),而且绝对路径参数会被截断成裸名字。
func TestLsDashDKeepsArgumentTextVerbatim(t *testing.T) {
	dir := lsFixture(t)

	out, _, code := lsIn(t, dir, "-d", "dirA/")
	if code != 0 {
		t.Fatalf("-d dirA/: exit=%d", code)
	}
	if got := strings.TrimSpace(out); got != "dirA/" {
		t.Fatalf("ls -d dirA/ 应当保留尾斜杠给 %q,得到 %q —— "+
			"用 filepath.Base 会把它截成 dirA", "dirA/", got)
	}

	out, _, code = lsIn(t, dir, "-d", "..")
	if code != 0 {
		t.Fatalf("-d ..: exit=%d", code)
	}
	if got := strings.TrimSpace(out); got != ".." {
		t.Fatalf("ls -d .. 应当是 %q,得到 %q", "..", got)
	}
}

// ★ **反斜杠**形态必须原样保留 —— 这才是 winbash glob 真实产出的形态。
//
// 这条是补上的一个**假绿洞**(2026-10-18 由一次独立审查发现,主代理亲手复验):
//
// winbash 的 glob 用反斜杠分隔,`ls -d */` 的**参数实际是 `dirA\`**(见 README
// 「glob 给 `.\x`」那条)。但本文件原先只喂过 `dirA/`(POSIX 斜杠),于是:
//
//	name: strings.TrimSuffix(p, "\\")   ← 这个变异**全绿通过**
//
// 而它一旦混进代码,真实场景 `ls -d */` 就会退化成裸名字 —— 正是 ls.go 注释里
// 拼命要防的那件事。测试没钉住自己最强调的不变量,是"看着绿但抓不住"的典型。
//
// 用 `${d%[/\\]}` 那类写法的使用者也会依赖这个行为:尾部反斜杠在,才知道是不是目录。
func TestLsDashDKeepsTrailingBackslashFromGlob(t *testing.T) {
	dir := lsFixture(t)

	out, errw, code := lsIn(t, dir, "-d", `dirA\`)
	if code != 0 {
		t.Fatalf(`-d "dirA\\": exit=%d errw=%q`, code, errw)
	}
	if got := strings.TrimSpace(out); got != `dirA\` {
		t.Fatalf(`ls -d "dirA\\" 应当原样输出 %q,得到 %q ——`+
			`这是 winbash glob 的真实产出形态(echo */ 给 "dirA\\");`+
			`若这里被去掉反斜杠,真实场景 ls -d */ 就会丢掉"这是目录"的线索`,
			`dirA\`, got)
	}

	// 成对:POSIX 斜杠也要保留(两种都要,别为了让一种过而清掉另一种)。
	out, _, code = lsIn(t, dir, "-d", "dirA/")
	if code != 0 {
		t.Fatalf("-d dirA/: exit=%d", code)
	}
	if got := strings.TrimSpace(out); got != "dirA/" {
		t.Fatalf("ls -d dirA/ 应当保留正斜杠,得到 %q", got)
	}
}

// ★ 多参数时的排序结果要**逐项**核对,不能只数个数。
//
// 只断言 `len(got) == 3` 的话,「排序后把同一个名字吐两次」这种错法照样绿
// (`dirA dirA dirB` 也是 3 个)。这里整切片比较,零成本闭合。
func TestLsDashDMultiArgOrderExact(t *testing.T) {
	dir := lsFixture(t)

	out, _, code := lsIn(t, dir, "-d", "dirB", "dirA", "dirB")
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	got := strings.Fields(out)
	want := []string{"dirA", "dirB", "dirB"}
	if len(got) != len(want) {
		t.Fatalf("条目数不对:得到 %q,期望 %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("顺序/内容不对:得到 %q,期望 %q(排序 + 重复参数保留)", got, want)
		}
	}
}

// ★ `-d` 的参数要**按名字排序**,且重复参数要**都留着**。
//
// 实测:
//
//	ls -d f1.txt dirA dirB → "dirA" "dirB" "f1.txt"   (排序,不按原序)
//	ls -d dirB dirA dirB   → "dirA" "dirB" "dirB"     (重复保留)
//
// 排序用 SliceStable:重复项的相对次序要可复现,否则同一目录两次调用可能不同序。
func TestLsDashDSortsButKeepsDuplicates(t *testing.T) {
	dir := lsFixture(t)

	out, _, code := lsIn(t, dir, "-d", "f1.txt", "dirA", "dirB")
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	got := strings.Fields(out)
	want := []string{"dirA", "dirB", "f1.txt"}
	if len(got) != len(want) {
		t.Fatalf("条目数不对:得到 %q,期望 %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("按名字排序不对:得到 %q,期望 %q(实测真 bash 会排序)", got, want)
		}
	}

	out, _, code = lsIn(t, dir, "-d", "dirB", "dirA", "dirB")
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if got := strings.Fields(out); len(got) != 3 {
		t.Fatalf("重复参数应当都留着(实测真 bash 给 dirA dirB dirB),得到 %q", out)
	}
}

// ★ `ls -l -d` 有长格式但**没有 total 行**。
//
// 实测:`ls -ld dirA` → `drwx... dirA`,**没有** `total N` 开头;
// 而 `ls -l`(列目录内容)第一行必然是 `total N`。
//
// 为什么这条必须钉:脚本常用 `ls -l | tail -n +2` 跳过 total 行。若 `-d` 也打
// total,`ls -ld */ | tail -n +2` 就会吃掉第一条真实数据 —— 而输出照样一行行文本,
// 看不出少了什么。
func TestLsDashDLongFormatHasNoTotalLine(t *testing.T) {
	dir := lsFixture(t)

	out, _, code := lsIn(t, dir, "-l", "-d", "dirA")
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("ls -ld dirA 应当恰好 1 行,得到 %d 行:%q", len(lines), out)
	}
	if strings.HasPrefix(lines[0], "total ") {
		t.Fatalf("ls -ld 不该有 total 行(那是列目录内容时才有的):%q", out)
	}
	// 长格式的列仍然要在。
	//
	// 这里不能用 `== 7`:属主名本身可能含空格(本机就是 `Test User`),时间又是
	// `Oct 18 02:11` 这种带空格的形态,所以一行会切出 10 段。判据是「至少 7 段」——
	// 与 ls_test.go 的 TestLsLongFormatHasSevenColumns 同一处理方式。
	if n := len(strings.Fields(lines[0])); n < 7 {
		t.Fatalf("ls -ld 至少该切出 7 段(属主/时间含空格只会更多),得到 %d 段:%q",
			n, lines[0])
	}
	// 首列必须是 10 字符权限串(目录以 d 开头)—— 这才是"长格式真的生效了"的证据。
	if perm := strings.Fields(lines[0])[0]; len(perm) != 10 || perm[0] != 'd' {
		t.Fatalf("首列该是 10 字符权限串且以 'd' 开头(目录):%q", perm)
	}

	// 对照组:不带 -d 列目录内容时,total 行必须在(这是既有测试钉着的语义)。
	out, _, code = lsIn(t, dir, "-l")
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if !strings.HasPrefix(out, "total ") {
		t.Fatalf("ls -l(列目录)第一行必须是 total,得到 %q", firstLine(out))
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// ★ 参数不存在时必须**响亮失败**,不能静默跳过。
//
// 实测真 bash:`ls -d nope_xyz` → rc=2 + `ls: cannot access 'nope_xyz'`。
// winbash 的退出码约定是 `1`(一般错误),不是 bash 的 2(那是用法错误)——
// 这个差异是既有的、已记录的(`ls 不存在` 历来给 1)。这里断言的是**非零 + 有错误行**,
// 而不是死抠 1 还是 2:底线是"不静默"。
//
// 混合参数时:存在的照常列出,不存在的报错,退出码非零 —— 实测真 bash 就是这么做的
// (`ls -d nope_xyz dirA` → 打印 dirA,rc=2)。
func TestLsDashDMissingPathIsReportedNotSilent(t *testing.T) {
	dir := lsFixture(t)

	out, errw, code := lsIn(t, dir, "-d", "nope_xyz")
	if code == 0 {
		t.Fatalf("参数不存在时退出码必须非零(静默跳过会让脚本以为列成功了)")
	}
	if strings.TrimSpace(errw) == "" {
		t.Fatal("参数不存在时必须往 stderr 写错误行,不能一声不吭")
	}
	if !strings.Contains(errw, "nope_xyz") {
		t.Fatalf("错误行要点名那个找不到的路径: %q", errw)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("不存在的参数不该产生 stdout 输出,得到 %q", out)
	}

	// 混合:存在的照常列出,失败不吞掉成功项。
	out, errw, code = lsIn(t, dir, "-d", "nope_xyz", "dirA")
	if code == 0 {
		t.Fatal("有一个参数不存在,退出码必须非零")
	}
	if got := strings.TrimSpace(out); got != "dirA" {
		t.Fatalf("存在的那个仍要列出来,得到 %q", got)
	}
	if !strings.Contains(errw, "nope_xyz") {
		t.Fatalf("错误行要点名找不到的那个: %q", errw)
	}
}

// ★ `-d` 与 `-a` 组合:`-da` 等价于 `-d`(点文件在 -d 下没有"隐藏"这回事)。
//
// 实测:`ls -da` → "."、`ls -da .` → "." —— 与不带 -a 的 `ls -d` 输出相同。
// 连字符顺序无关(`-dl` 与 `-ld` 一样),组合写法必须能解析。
func TestLsDashDAcceptsCombinedFlags(t *testing.T) {
	dir := lsFixture(t)

	// -da 与 -d 同输出。
	outD, _, code := lsIn(t, dir, "-d")
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	outDA, _, code := lsIn(t, dir, "-da")
	if code != 0 {
		t.Fatalf("-da: exit=%d", code)
	}
	if strings.TrimSpace(outD) != strings.TrimSpace(outDA) {
		t.Fatalf("-da 应当与 -d 同输出(实测真 bash 都只给 \".\"),得到 %q vs %q", outDA, outD)
	}
	// 而且**那个共同值必须是 "."**。少了这一句,`outD == outDA` 只证明了"a 能被解析",
	// 对 -d 的语义零区分度 —— 两个都是错的(比如都列出目录内容)也照样相等。
	if got := strings.TrimSpace(outD); got != "." {
		t.Fatalf("ls -d 无参数应当给 %q,得到 %q", ".", got)
	}

	// -dl 与 -ld 同输出(组合顺序无关)。
	outDL, _, code := lsIn(t, dir, "-dl", "dirA")
	if code != 0 {
		t.Fatalf("-dl: exit=%d", code)
	}
	outLD, _, code := lsIn(t, dir, "-ld", "dirA")
	if code != 0 {
		t.Fatalf("-ld: exit=%d", code)
	}
	if outDL != outLD {
		t.Fatalf("-dl 与 -ld 输出应当一致,得到 %q vs %q", outDL, outLD)
	}
	// 且必须是长格式(带权限位),不是短格式。
	if !strings.HasPrefix(strings.TrimSpace(outDL), "d") {
		t.Fatalf("-dl 应当给长格式(目录的权限位以 d 开头),得到 %q", outDL)
	}
}

// ★ `-d` 对**文件**参数与不带 `-d` 相同(文件本来就不展开)。
//
// 实测:`ls -d f1.txt` → "f1.txt"、`ls -d f*.txt` → 两行。
// 这条是区分度的对照:若 -d 分支实现成"只处理目录",文件参数就会被漏掉。
func TestLsDashDWorksForPlainFiles(t *testing.T) {
	dir := lsFixture(t)

	out, _, code := lsIn(t, dir, "-d", "f1.txt")
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if got := strings.TrimSpace(out); got != "f1.txt" {
		t.Fatalf("ls -d f1.txt 应当是 %q,得到 %q", "f1.txt", got)
	}

	// 多个文件(glob 已展开后的形态)。
	out, _, code = lsIn(t, dir, "-d", "f1.txt", "f2.txt")
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if got := strings.Fields(out); len(got) != 2 {
		t.Fatalf("ls -d f1.txt f2.txt 应当给两个名字,得到 %q", out)
	}

	// 长格式对文件也要有 total 之外的 7 列(仍然没有 total 行)。
	out, _, code = lsIn(t, dir, "-ld", "f1.txt")
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	line := strings.TrimSpace(out)
	if strings.HasPrefix(line, "total ") {
		t.Fatalf("ls -ld 文件也不该有 total 行: %q", out)
	}
	// 与上面同一条理由:属主/时间含空格,判据是「至少 7 段」+ 首列是 10 字符权限串。
	fields := strings.Fields(line)
	if len(fields) < 7 {
		t.Fatalf("ls -ld 文件至少该切出 7 段,得到 %d 段:%q", len(fields), line)
	}
	if perm := fields[0]; len(perm) != 10 || perm[0] != '-' {
		t.Fatalf("普通文件的首列该是以 '-' 开头的 10 字符权限串:%q", perm)
	}
}
