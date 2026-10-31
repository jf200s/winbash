// `ls -t` / `-r` 的行为测试。
//
// 全部期望值取自本机真 Git Bash 的 ls(GNU coreutils 8.32,`usr\bin\ls.exe`),
// 2026-10-31 用 `touch -d` 造出**精确相等**的并列 mtime(三个文件
// `--time-style=full-iso` 下都显示同一纳秒值)逐条实测,不是照 GNU 手册推的。
//
// 两条实测结论,是这套测试的地基:
//
//  1. `-t` 排 ModTime **新→旧**,并列 mtime 的条目按**名字升序**;
//  2. `-r` 逆转的是**整个比较器**,不只是把最终列表倒一遍 —— 所以 `-tr` 的
//     并列条目名字是**倒序**的。`ls -r`(无 -t)恰是默认字母序的逐行取反。
//
// fixture 用相对 now 的偏移而不是绝对日期:文件全都落在 6 个月内,时间列恒为
// 时分形态,测试不会随年份漂移;三个并列文件传入的是**同一个** time.Time 值,
// 就算文件系统粒度粗(FAT 是 2 秒),截断后也依然相等,并列关系不会散。
//
// `-t` 为什么值得单独一个文件:它让短格式也必须取 info(mtime 是排序键),
// 且文件操作数必须攒成一组才能排(`ls -t *.go` 的 glob 展开成 N 个操作数)——
// 这两处都是靠这条文件里的测试钉住的。

package builtins

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// lsSortFixture 造四个不同 mtime + 三个并列 mtime 的文件,返回目录。
//
// 新→旧:new(1h 前)> t_*(并列,10 天前)> mid(20 天前)> old(40 天前)。
// 并列组的名字升序是 t_alpha < t_mid < t_zeta(实测真 ls 就按这个次序)。
func lsSortFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	now := time.Now()
	tie := now.Add(-10 * 24 * time.Hour)
	mtimes := map[string]time.Time{
		"old.txt":     now.Add(-40 * 24 * time.Hour),
		"mid.txt":     now.Add(-20 * 24 * time.Hour),
		"new.txt":     now.Add(-time.Hour),
		"t_zeta.txt":  tie,
		"t_alpha.txt": tie,
		"t_mid.txt":   tie,
	}
	for name, mt := range mtimes {
		f := filepath.Join(dir, name)
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(f, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func lsNames(t *testing.T, dir string, n int, argv ...string) []string {
	t.Helper()
	out, errw, code := lsIn(t, dir, argv...)
	if code != 0 {
		t.Fatalf("ls %v: exit=%d stderr=%q", argv, code, errw)
	}
	fields := strings.Fields(out)
	if len(fields) != n {
		t.Fatalf("ls %v: 应当 %d 个名字,得到 %d 个:%q", argv, n, len(fields), out)
	}
	return fields
}

// ★ 裸 `-t`:ModTime 新→旧,并列 mtime 按名字升序。
//
// 实测真 Git Bash(fixture 同构目录):
//
//	ls -t → new  t_alpha  t_mid  t_zeta  mid  old
func TestLsTimeSortsNewestFirstWithTiedNamesAscending(t *testing.T) {
	dir := lsSortFixture(t)

	got := lsNames(t, dir, 6, "-t")
	want := []string{"new.txt", "t_alpha.txt", "t_mid.txt", "t_zeta.txt", "mid.txt", "old.txt"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ls -t 顺序不对(实测真 ls 为 %q):得到 %q", want, got)
		}
	}
}

// ★ `-tr` 与 `-rt`:-r 逆转整个比较器 → ModTime 旧→新,**并列条目名字倒序**。
//
// 实测真 Git Bash:
//
//	ls -tr → old  mid  t_zeta  t_mid  t_alpha  new
//	ls -rt → (与 -tr 完全相同)
//
// 若 -r 只是把最终列表倒一遍,并列组会错误地给 t_alpha t_mid t_zeta ——
// 这条测试就是钉这个差别的。
func TestLsTimeReverseInvertsWholeComparatorIncludingTies(t *testing.T) {
	dir := lsSortFixture(t)

	want := []string{"old.txt", "mid.txt", "t_zeta.txt", "t_mid.txt", "t_alpha.txt", "new.txt"}
	for _, argv := range [][]string{{"-tr"}, {"-rt"}} {
		got := lsNames(t, dir, 6, argv...)
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("ls %v 顺序不对(实测真 ls 为 %q,注意并列组名字是倒序):得到 %q",
					argv, want, got)
			}
		}
	}
}

// ★ 裸 `-r`(不配 -t):恰是默认字母序的**逐行取反**。
//
// 实测真 Git Bash:
//
//	ls     → mid new old t_alpha t_mid t_zeta
//	ls -r  → t_zeta t_mid t_alpha old new mid
func TestLsReverseIsExactReverseOfDefaultOrder(t *testing.T) {
	dir := lsSortFixture(t)

	fwd := lsNames(t, dir, 6)
	rev := lsNames(t, dir, 6, "-r")
	if len(fwd) != len(rev) {
		t.Fatalf("两个方向条目数不同:%q vs %q", fwd, rev)
	}
	for i := range fwd {
		if fwd[i] != rev[len(rev)-1-i] {
			t.Fatalf("ls -r 应当是默认序的逐行取反:%q vs reverse(%q)", rev, fwd)
		}
	}
}

// ★ `-lt` / `-ltr`:长格式下同样按 ModTime 排,列结构与 `ls -l` 一致
// (total 行在前、数据行行尾是名字)。
//
// 实测真 Git Bash(-lt 的名字次序与裸 -t 完全一致;-ltr 整体取反):
//
//	ls -lt  → new t_alpha t_mid t_zeta mid old
//	ls -ltr → old mid t_zeta t_mid t_alpha new
//
// `ls -lt | head` 是「看最近改了哪些文件」的标准写法,次序错了是静默错值。
func TestLsLongTimeOrderMatchesRealBash(t *testing.T) {
	dir := lsSortFixture(t)

	// 长格式一行切出的字段比 7 多(属主名/时间含空格),名字恒是行尾,
	// 所以按「最后一列」取名字,与 ls_test.go 处理属主空格的思路一致。
	nameOf := func(out string) []string {
		var names []string
		for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
			if strings.HasPrefix(line, "total ") {
				continue
			}
			f := strings.Fields(line)
			names = append(names, f[len(f)-1])
		}
		return names
	}

	out, _, code := lsIn(t, dir, "-lt")
	if code != 0 {
		t.Fatalf("-lt: exit=%d", code)
	}
	got := nameOf(out)
	want := []string{"new.txt", "t_alpha.txt", "t_mid.txt", "t_zeta.txt", "mid.txt", "old.txt"}
	if len(got) != len(want) {
		t.Fatalf("-lt 应当 6 行数据,得到 %d 行:%q", len(got), out)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ls -lt 次序不对(实测真 ls 为 %q):得到 %q", want, got)
		}
	}

	out, _, code = lsIn(t, dir, "-ltr")
	if code != 0 {
		t.Fatalf("-ltr: exit=%d", code)
	}
	got = nameOf(out)
	wantRev := []string{"old.txt", "mid.txt", "t_zeta.txt", "t_mid.txt", "t_alpha.txt", "new.txt"}
	for i := range wantRev {
		if got[i] != wantRev[i] {
			t.Fatalf("ls -ltr 次序不对(实测真 ls 为 %q):得到 %q", wantRev, got)
		}
	}
}

// ★ 文件操作数要攒成**一组**排序:这是 `ls -t *.go` 能用的前提。
//
// glob 展开后每个 .go 都是独立操作数;GNU 把所有非目录操作数当一组、
// 排序后输出(且排在目录前面)。实测真 Git Bash(操作数故意按"新的在后"给):
//
//	ls zfirst.txt alast.txt     → alast.txt zfirst.txt   (默认:按名字)
//	ls -t zfirst.txt alast.txt  → alast.txt zfirst.txt   (-t:alast 更新,在前)
//
// 修之前文件操作数按参数原序逐个写出,-t 被静默吞掉。默认序也顺带从
// "原序"对齐到了真 ls 的"按名字排序"。
func TestLsTimeSortsFileOperandsAsOneGroup(t *testing.T) {
	dir := lsSortFixture(t)
	now := time.Now()
	// zfirst 名字在前但更旧;alast 名字在后但更新 —— 两种排序给不同答案,
	// 一个用例同时区分"按名"与"按时间"。
	for name, mt := range map[string]time.Time{
		"zfirst.txt": now.Add(-5 * 24 * time.Hour),
		"alast.txt":  now.Add(-2 * time.Hour),
	} {
		f := filepath.Join(dir, name)
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(f, mt, mt); err != nil {
			t.Fatal(err)
		}
	}

	// -t:新的(alast)在前。
	got := lsNames(t, dir, 2, "-t", "zfirst.txt", "alast.txt")
	if got[0] != "alast.txt" || got[1] != "zfirst.txt" {
		t.Fatalf("ls -t 文件操作数应当按 mtime 排(alast 更新在前),得到 %q", got)
	}

	// 默认:按名字(alast 在前)—— 与真 ls 一致,不再是参数原序。
	got = lsNames(t, dir, 2, "zfirst.txt", "alast.txt")
	if got[0] != "alast.txt" || got[1] != "zfirst.txt" {
		t.Fatalf("ls 默认对文件操作数按名字排序(实测真 ls 如此),得到 %q", got)
	}

	// -rt:整个比较器逆转 → 旧的(zfirst)在前。
	got = lsNames(t, dir, 2, "-rt", "zfirst.txt", "alast.txt")
	if got[0] != "zfirst.txt" || got[1] != "alast.txt" {
		t.Fatalf("ls -rt 文件操作数应当整体逆转(旧的在前),得到 %q", got)
	}
}

// ★ `-t`/`-r` 同样作用于 `-d` 的操作数:实测真 Git Bash 用同一套比较器。
//
//	ls -d dirB dirA mid.txt zfirst.txt   → dirA dirB mid.txt zfirst.txt(按名)
//	ls -dt 〃                             → zfirst dirB mid dirA(按 mtime 新→旧)
//	ls -drt 〃                            → dirA mid dirB zfirst(整体逆转)
func TestLsTimeAppliesToDashDOperands(t *testing.T) {
	dir := lsSortFixture(t)
	if err := os.Mkdir(filepath.Join(dir, "dirA"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "dirB"), 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	// mtime:zfirst(-2h)> dirB(-3d)> mid.txt(fixture,-20d)> dirA(-30d)。
	zf := filepath.Join(dir, "zfirst.txt")
	if err := os.WriteFile(zf, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, mt := range map[string]time.Time{
		"dirB":       now.Add(-3 * 24 * time.Hour),
		"dirA":       now.Add(-30 * 24 * time.Hour),
		"zfirst.txt": now.Add(-2 * time.Hour),
	} {
		if err := os.Chtimes(filepath.Join(dir, name), mt, mt); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		argv []string
		want []string
	}{
		{[]string{"-d", "dirB", "dirA", "mid.txt", "zfirst.txt"},
			[]string{"dirA", "dirB", "mid.txt", "zfirst.txt"}},
		{[]string{"-dt", "dirB", "dirA", "mid.txt", "zfirst.txt"},
			[]string{"zfirst.txt", "dirB", "mid.txt", "dirA"}},
		{[]string{"-drt", "dirB", "dirA", "mid.txt", "zfirst.txt"},
			[]string{"dirA", "mid.txt", "dirB", "zfirst.txt"}},
	}
	for _, c := range cases {
		out, errw, code := lsIn(t, dir, c.argv...)
		if code != 0 {
			t.Fatalf("ls %v: exit=%d stderr=%q", c.argv, code, errw)
		}
		got := strings.Fields(out)
		if len(got) != len(c.want) {
			t.Fatalf("ls %v: 得到 %q,期望 %q", c.argv, got, c.want)
		}
		for i := range c.want {
			if got[i] != c.want[i] {
				t.Fatalf("ls %v 次序不对(实测真 ls 为 %q):得到 %q", c.argv, c.want, got)
			}
		}
	}
}

// ★ -t 与 -l 组合时,输出仍是**无装饰的 7 列**长格式(total 行在,
// 权限串在首列)—— 排序只改次序,不许把格式改坏。
func TestLsLongTimeKeepsSevenColumnShape(t *testing.T) {
	dir := lsSortFixture(t)

	out, _, code := lsIn(t, dir, "-lt")
	if code != 0 {
		t.Fatalf("-lt: exit=%d", code)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if !strings.HasPrefix(lines[0], "total ") {
		t.Fatalf("第一行该是 total 行:%q", out)
	}
	if len(lines) != 7 {
		t.Fatalf("total + 6 个文件 = 7 行,得到 %d 行:%q", len(lines), out)
	}
	for _, line := range lines[1:] {
		perm := strings.Fields(line)[0]
		if len(perm) != 10 || perm[0] != '-' {
			t.Fatalf("数据行首列该是 10 字符权限串:%q", line)
		}
	}
}

// ★ 短格式在 -t 下仍然**零装饰**:排序要 info,不代表输出要变样。
// `ls -t` 的输出与 `ls` 只该差次序,一行一个纯文件名。
func TestLsTimeKeepsShortFormatPlain(t *testing.T) {
	dir := lsSortFixture(t)

	out, _, code := lsIn(t, dir, "-t")
	if code != 0 {
		t.Fatalf("-t: exit=%d", code)
	}
	if strings.Contains(out, "total") || strings.Contains(out, "rw") {
		t.Fatalf("短格式不该因 -t 带任何装饰:%q", out)
	}
	if lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n"); len(lines) != 6 {
		t.Fatalf("该正好 6 行,得到 %d 行:%q", len(lines), out)
	}
}
