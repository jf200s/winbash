package builtins

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 本文件钉住 grep 对**二进制内容的提示**。
//
// 背景:真 GNU grep 一旦在任何 buffer 里看到 NUL,就把输出切成二进制模式,只打一行
// `Binary file X matches`。winbash **不那样做** —— 那样会把 `grep -c` 的计数语义
// 一起改掉(实测:同一个文件真 grep `-c` 给 116、`-a -c` 给 87,而 87 才是"匹配行数"),
// 而且那套判定是 libc 的 buffer 策略,依赖它等于给一个"像 GNU 但边界不同"的假预期。
//
// 折中(本轮定的):**输出不动**(规则可解释、计数可复现、`grep -c` 与真 grep 逐字一致),
// 但**不再静默** —— 只要有内容行含 NUL 被输出,就往 stderr 说一句。
//
// 判据是"**被输出的**行含 NUL",不是"文件里有 NUL":文件里有 NUL 但没被输出的行不会
// 污染下游,提示它只会制造噪声。这条边界两侧都要钉。

// writeBin 造一个受控的二进制文件。-1 表示 NUL 字节。
func writeBin(t *testing.T, dir, name string, content []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func runGrep(t *testing.T, dir string, argv ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errw bytes.Buffer
	code = Grep(context.Background(), dir, argv, strings.NewReader(""), &out, &errw)
	return out.String(), errw.String(), code
}

// NUL 出现在**匹配行**里 → 提示;stdout 照旧(内容不再被静默吞掉,但也不被过滤)。
func TestBinaryMatchIsReportedButNotSuppressed(t *testing.T) {
	dir := t.TempDir()
	// 第一行含 NUL,第二行是纯文本
	writeBin(t, dir, "b.bin", []byte{'x', 0, 'y', '\n', 'x', 'z', '\n'})

	stdout, stderr, code := runGrep(t, dir, "x", "b.bin")
	if code != 0 {
		t.Fatalf("exit=%d, stderr=%s", code, stderr)
	}

	// stdout:两行都逐行输出(与真 grep 的 `Binary file` 一行**不同**,这是有意的)
	if n := strings.Count(stdout, "\n"); n != 2 {
		t.Errorf("stdout 应是 2 行逐行输出,得到 %d 行: %q", n, stdout)
	}
	if !strings.Contains(stdout, "\x00") {
		t.Error("stdout 不该被改动:内容原样带出(提示只走 stderr)")
	}

	// stderr:必须有提示,而且要点出原因
	if !strings.Contains(stderr, "NUL") {
		t.Errorf("匹配行含 NUL 时应当提示,stderr = %q", stderr)
	}
	if !strings.Contains(stderr, "b.bin") {
		t.Errorf("提示里应当点名是哪个文件,stderr = %q", stderr)
	}
	// 三行式(项目底线:命令 / 问题 / 怎么办)
	if !strings.Contains(stderr, "怎么办") {
		t.Errorf("提示应当是三行式,stderr = %q", stderr)
	}
}

// **边界**:文件里有 NUL,但没有一行"含 NUL 且被输出" → 不提示。
//
// 这条是判据的核心。若改成"文件里有 NUL 就提示",那么在一个 100MB 的日志里
// 恰好有一行乱码,每次 grep 都要多一句噪声。
func TestNULOutsidePrintedLinesDoesNotWarn(t *testing.T) {
	dir := t.TempDir()
	// 第 1 行纯文本且匹配;第 2 行含 NUL 但**不**匹配
	writeBin(t, dir, "mixed.bin", append([]byte("findme\n"), 0, 'x', 0, '\n'))

	stdout, stderr, code := runGrep(t, dir, "findme", "mixed.bin")
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if stdout != "findme\n" {
		t.Errorf("stdout = %q", stdout)
	}
	if stderr != "" {
		t.Errorf("没有输出含 NUL 的行,不该提示;stderr = %q", stderr)
	}
}

// 纯文本文件不该被误报(回归:提示不能变成"每次 grep 都多一句")。
func TestPlainTextFileDoesNotWarn(t *testing.T) {
	dir := t.TempDir()
	writeBin(t, dir, "t.txt", []byte("hello\nworld\n"))

	_, stderr, _ := runGrep(t, dir, "hello", "t.txt")
	if stderr != "" {
		t.Errorf("纯文本不该提示,stderr = %q", stderr)
	}
}

// `-v` 反转之后,含 NUL 的行**成为**输出 → 仍然要提示。
//
// 判据是"被输出的行含 NUL",不是"匹配的行含 NUL" —— 这个区别在 -v 下才会显形。
func TestInvertedMatchStillWarnsWhenNULIsPrinted(t *testing.T) {
	dir := t.TempDir()
	writeBin(t, dir, "b.bin", append([]byte("keep\n"), 'x', 0, 'y', '\n'))

	stdout, stderr, code := runGrep(t, dir, "-v", "keep", "b.bin")
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if !strings.Contains(stdout, "\x00") {
		t.Errorf("反转后含 NUL 的那行应当被输出,stdout = %q", stdout)
	}
	if !strings.Contains(stderr, "NUL") {
		t.Errorf("输出了含 NUL 的行就该提示,stderr = %q", stderr)
	}
}

// `-l` 与 `-q` 不吐内容,所以**不该**提示。
//
// 它们的输出只有"文件名"或"什么都没有",NUL 到不了下游 —— 提示只会干扰
// `if grep -q x f; then` 这类写法(它们靠退出码与 stdout,不该被 stderr 噪声打扰)。
func TestListingAndQuietDoNotWarn(t *testing.T) {
	dir := t.TempDir()
	writeBin(t, dir, "b.bin", []byte{'x', 0, 'y', '\n'}) // 匹配行本身就含 NUL

	t.Run("-l", func(t *testing.T) {
		stdout, stderr, code := runGrep(t, dir, "-l", "x", "b.bin")
		if code != 0 {
			t.Fatalf("exit=%d", code)
		}
		if strings.TrimSpace(stdout) != "b.bin" {
			t.Errorf("-l 应当只打文件名,stdout = %q", stdout)
		}
		if stderr != "" {
			t.Errorf("-l 不吐内容,不该提示;stderr = %q", stderr)
		}
	})

	t.Run("-q", func(t *testing.T) {
		stdout, stderr, code := runGrep(t, dir, "-q", "x", "b.bin")
		if code != 0 {
			t.Fatalf("-q 命中应当 exit 0,得到 %d", code)
		}
		if stdout != "" || stderr != "" {
			t.Errorf("-q 应当全静默,stdout=%q stderr=%q", stdout, stderr)
		}
	})
}

// `-c` 的计数语义**不受影响**:这是选"只提示、不改写输出"这条路的直接理由 ——
// 真 grep 切二进制模式后 `-c` 的数字会变(实测 116 vs 87),而那是个更难解释的差异。
func TestCountIsUnchangedByBinaryContent(t *testing.T) {
	dir := t.TempDir()
	// 3 行,每行都匹配,每行都含 NUL
	writeBin(t, dir, "b.bin", []byte{'x', 0, '\n', 'x', 0, '\n', 'x', 0, '\n'})

	stdout, stderr, code := runGrep(t, dir, "-c", "x", "b.bin")
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if strings.TrimSpace(stdout) != "3" {
		t.Errorf("-c 应当数出 3 行,stdout = %q(真 grep 在二进制模式下会给另一个数)", stdout)
	}
	if !strings.Contains(stderr, "NUL") {
		t.Errorf("-c 只是不吐内容,不是不提示;stderr = %q", stderr)
	}
}
