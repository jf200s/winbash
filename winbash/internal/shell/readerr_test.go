// 读侧(打开一个**目录**)的措辞与「不静默」。
//
// 为什么单独一个文件:Windows 上 `os.Open` 一个目录会**成功**(O_RDONLY),
// 失败要等到第一次 Read 才发生,给的是 Win32 的 ERROR_INVALID_FUNCTION。
// 这与 POSIX 不同,而 mvdan/sh 的几处内建都假设"open 成功就能读",于是错误
// 从**读**那里冒出来,原来的形态有三种毛病:
//
//	source adir    → source: read C:\...\adir: Incorrect function.
//	                 ↑ 泄漏 Win32 API 名、绝对路径、Win32 长句
//	mapfile < adir → mapfile: unable to read, read C:\...\adir: Incorrect function.
//	cat < adir     → cannot read standard input: I/O error (Win32 error 1)
//	read < adir    → **完全静默**(只有 rc=1)
//
// 修法是在 OpenHandler 里**打开成功后判一次目录**,是目录就关掉 fd 并返回
// `Is a directory` 的 ShellPathError(见 internal/builtins/dirread.go 的长注释,
// 那里记着为什么不能在"读"那一层包一个 reader —— 会被 io.Copy 吞掉)。
//
// ## 本文件里最要紧的是「正常路径没坏」那一组
//
// 改动落在 OpenHandler 上,那是**每一次**重定向/`source` 都会过的路。
// 若判目录的逻辑写错(比如 Stat 失败也当目录),`cat < 正常文件` 会突然读不了 ——
// 那是比原本的措辞问题严重得多的回归。所以下面必须有正向用例。

package shell

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runIn 在 dir 里跑一段脚本,返回 (stdout, stderr 各行, 退出码)。
func runIn(t *testing.T, dir, script string) (string, string, int) {
	t.Helper()
	s := NewSession()
	defer s.Close()
	if code := s.runForSetup(t, `cd "`+dir+`"`); code != 0 {
		t.Fatalf("cd 失败:%d", code)
	}
	out, exit, errs := s.Run(context.Background(), script, false)
	return out, strings.Join(errs, "\n"), exit
}

// dirFixture 造一个含子目录与普通文件的目录。
func dirFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "adir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plain.txt"), []byte("a\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// ★ `source` 一个目录:措辞要干净,不能泄漏 Win32。
//
// 真 bash:`source: adir: is a directory`(小写)。我们给 `Is a directory`(大写)——
// 与项目此前那批措辞保持一致的形态,且大小写差异不影响理解。
//
// **绝对路径那条用例是必需的**(变异验证逼出来的):`ResolveShellPath` 对**相对**
// 路径原样返回,所以 `adir` 这种情况下"用户原文"与"解析后的路径"恰好相同 ——
// 把实现改成用解析后的路径也不会有任何差异。只有 POSIX 绝对路径(`/c/Windows`)
// 才会让两者分开(`/c/Windows` vs `C:\Windows`),从而钉住"必须用用户原文"。
func TestSourceDirectoryReportsCleanError(t *testing.T) {
	dir := dirFixture(t)

	cases := []struct {
		script string
		want   string
	}{
		{"source adir", "adir: Is a directory"},
		{". adir", "adir: Is a directory"},
		// 绝对路径:用户写 `/c/Windows`,错误里也必须是 `/c/Windows`。
		{"source /c/Windows", "/c/Windows: Is a directory"},
		{"cat < /c/Windows", "/c/Windows: Is a directory"},
	}
	for _, c := range cases {
		t.Run(c.script, func(t *testing.T) {
			_, errText, exit := runIn(t, dir, c.script)
			if exit == 0 {
				t.Fatalf("%s 必须非零退出", c.script)
			}
			if !strings.Contains(errText, c.want) {
				t.Errorf("期望含 %q,得到 %q", c.want, errText)
			}
			mustNotLeak(t, errText)
			// 反向:绝不能出现 Windows 绝对路径形态。
			// 这条是"必须用用户原文"的直接断言 —— 少了它,把实现改成传解析后的
			// 路径不会有任何测试变红(相对路径下两者同值)。
			if strings.Contains(errText, `C:\Windows`) || strings.Contains(errText, `C:/Windows`) {
				t.Errorf("%s: 错误里出现了 Windows 绝对路径,应当保留用户写的 POSIX 形式:%q",
					c.script, errText)
			}
		})
	}
}

// ★ 从目录重定向输入:`cat < adir` 要被拦住并给干净措辞。
//
// 这条曾经有两版错误实现,都记在 dirread.go 的注释里:
//   - 原版:让错误从 read 冒出来 → `I/O error (Win32 error 1)`;
//   - 我第一版修法(包一层 reader)→ 非 *os.File 导致 mvdan/sh 用 goroutine
//     `io.Copy` 抽干,而 io.Copy 的返回值没人检查 → `cat < adir` 变成
//     **rc=0、无输出、无报错**(比修之前更坏)。
//
// 所以这条断言里 rc 与「有错误输出」两者都要查 —— 只看措辞会漏掉第二种退化。
func TestInputRedirectFromDirectoryIsRejected(t *testing.T) {
	dir := dirFixture(t)

	out, errText, exit := runIn(t, dir, "cat < adir")
	if exit == 0 {
		t.Fatalf("从目录重定向输入必须非零退出(stdout=%q) —— "+
			"若这里是 0,说明错误在 open 之后被吞掉了(见 dirread.go 的注释)", out)
	}
	if errText == "" {
		t.Fatal("必须往 stderr 报错,不能静默给空输入")
	}
	if !strings.Contains(errText, "adir: Is a directory") {
		t.Errorf("期望含 %q,得到 %q", "adir: Is a directory", errText)
	}
	mustNotLeak(t, errText)
	if strings.TrimSpace(out) != "" {
		t.Errorf("不该有 stdout 输出,得到 %q", out)
	}
}

// ★ `read` / `mapfile` 从目录读:不能**静默**。
//
// 这条是项目底线的直接体现。修之前:
//
//	read    → 只有 rc=1,一个字节的提示都没有
//	mapfile → 只有 rc=1,一个字节的提示都没有
//
// 而 `read` 静默尤其糟:AI 写 `while read x; do ...; done < 文件` 时,
// 若那条重定向落到了目录上,循环体一次都不执行、退出码 1、**没有任何线索**,
// 看起来就像"文件是空的"。
//
// 真 bash 在这两条上的行为不同(`read` 报错、`mapfile` 静默),我们不照搬后者:
// 按「不静默做错事」,响亮是更好的那一边。
func TestReadAndMapfileFromDirectoryAreNotSilent(t *testing.T) {
	dir := dirFixture(t)

	cases := []struct {
		name   string
		script string
	}{
		{"read", `read -r x < adir; echo "rc=$?"`},
		{"while read", `while read -r l; do echo "L=$l"; done < adir; echo "rc=$?"`},
		{"mapfile", `mapfile -t arr < adir; echo "rc=$? n=${#arr[@]}"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, errText, _ := runIn(t, dir, c.script)
			if errText == "" {
				t.Fatalf("%s 从目录读必须报错,不能静默(脚本输出见上)", c.name)
			}
			if !strings.Contains(errText, "Is a directory") {
				t.Errorf("期望含 %q,得到 %q", "Is a directory", errText)
			}
			mustNotLeak(t, errText)
		})
	}
}

// ★★★ 正常路径不能坏 —— 改动落在 OpenHandler 上,那是每一次重定向都会过的路。
//
// 若"判目录"的逻辑写错(比如 Stat 失败也当目录、或把非目录的 fd 也关掉),
// `cat < 正常文件` 会突然读不了。那是比措辞问题严重得多的回归,
// 而且原测试集里**未必**有覆盖(既有用例大多不经过 `cat < 文件` 这种形态)。
func TestNormalFileRedirectsStillWork(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "plain.txt"), []byte("a\nb\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "lib.sh"), []byte("GVAR=from-source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "empty.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		script string
		want   string // stdout 必须包含
	}{
		{"输入重定向", `cat < plain.txt`, "a"},
		{"输入重定向读到完整内容", `wc -l < plain.txt`, "3"},
		{"read 从正常文件", `read -r x < plain.txt; echo "x=$x"`, "x=a"},
		{"while read 正常文件", `n=0; while read -r _; do n=$((n+1)); done < plain.txt; echo "n=$n"`, "n=3"},
		{"mapfile 正常文件", `mapfile -t arr < plain.txt; echo "n=${#arr[@]}"`, "n=3"},
		{"空文件不报错", `cat < empty.txt; echo "rc=$?"`, "rc=0"},
		{"source 正常脚本", `source lib.sh; echo "G=$GVAR"`, "G=from-source"},
		{"输出重定向仍正常", `echo hi > out.txt; cat out.txt`, "hi"},
		{"追加重定向仍正常", `echo a > o2.txt; echo b >> o2.txt; wc -l < o2.txt`, "2"},
		{"重定向进子目录", `echo x > sub/f.txt; cat sub/f.txt`, "x"},
		{"管道仍正常", `echo hi | cat`, "hi"},
		{"here-doc 仍正常", "cat <<EOF\nhd\nEOF", "hd"},
		{"here-string 仍正常", `cat <<< "hs"`, "hs"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, errText, exit := runIn(t, dir, c.script)
			if exit != 0 {
				t.Errorf("%s: 退出码 %d(errs=%q)", c.name, exit, errText)
			}
			if !strings.Contains(out, c.want) {
				t.Errorf("%s: stdout 期望含 %q,得到 %q(errs=%q)",
					c.name, c.want, out, errText)
			}
		})
	}
}

// ★ 目录判定的边界:`/dev/null` 与 FIFO 不是目录,不能被拦。
//
// `/dev/null` 在 winbash 里走 NUL 特例(OpenHandler 之前的 ResolveShellPath 就
// 放行了),但这条用例的价值是**防止判目录逻辑把某些特殊文件误判成目录** ——
// 一旦误判,`> /dev/null` 会全线失败,而那是极常见的写法。
func TestDevNullAndSpecialTargetsAreNotTreatedAsDirectories(t *testing.T) {
	dir := t.TempDir()

	cases := []struct {
		name   string
		script string
		want   string
	}{
		{"重定向到 /dev/null", `echo x > /dev/null; echo "rc=$?"`, "rc=0"},
		{"从 /dev/null 读", `cat < /dev/null; echo "rc=$?"`, "rc=0"},
		{"追加到 /dev/null", `echo x >> /dev/null; echo "rc=$?"`, "rc=0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, errText, exit := runIn(t, dir, c.script)
			if exit != 0 {
				t.Errorf("%s: 退出码 %d(errs=%q)", c.name, exit, errText)
			}
			if !strings.Contains(out, c.want) {
				t.Errorf("%s: stdout 期望含 %q,得到 %q(errs=%q)",
					c.name, c.want, out, errText)
			}
		})
	}
}
