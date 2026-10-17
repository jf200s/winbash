// 路径错误**措辞**的测试。
//
// 为什么值得单独一个文件:错误文案是「AI 能不能自己修」的关键输入。修之前它在
// winbash 里长这样 ——
//
//	问题: GetFileAttributesEx C:\Users\...\nope.txt: The system cannot find the file specified.
//
// 三个问题:泄漏 Win32 API 名、把用户写的相对路径换成绝对路径、用 Win32 长句而不是
// POSIX 措辞。真 bash 给的是 `cannot access 'nope.txt': No such file or directory`。
//
// 下面每条断言都对着一份真 Git Bash 实测输出(2026-10-18),见各条注释。

package builtins

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// lsStderr 跑一次 ls,只回 stderr(路径错误全在 stderr)。
func lsStderr(t *testing.T, dir string, argv ...string) string {
	t.Helper()
	_, errw, _ := lsIn(t, dir, argv...)
	return errw
}

// ★ 五类路径错误都要给出**与真 bash 一致的 POSIX 措辞**。
//
// 实测对照(左 winbash 修前 / 右真 bash):
//
//	ls nope.txt           GetFileAttributesEx C:\...\nope.txt: The system cannot
//	                      find the file specified.
//	                      → cannot access 'nope.txt': No such file or directory
//	ls plain.txt/sub      → cannot access 'plain.txt/sub': Not a directory
//	ls "a<b>.txt"         → cannot access 'a<b>.txt': No such file or directory
//	cat 受保护文件        → cannot open '...': Permission denied
//	cat 某目录            → cannot read 'adir': Is a directory
func TestPathErrUsesPosixWording(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "plain.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	adir := filepath.Join(dir, "adir")
	if err := os.Mkdir(adir, 0o755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		argv []string
		want string
		why  string
	}{
		{"文件不存在", []string{"nope.txt"},
			"cannot access 'nope.txt': No such file or directory",
			"真 bash: ls: cannot access 'nope.txt': No such file or directory"},
		{"目录不存在(带尾斜杠)", []string{"nope_dir/"},
			"cannot access 'nope_dir/': No such file or directory",
			"尾斜杠要原样保留 —— 那是用户写的东西"},
		{"深层都不存在", []string{"a/b/c/d.txt"},
			"cannot access 'a/b/c/d.txt': No such file or directory",
			"Windows 在这里给 errno 3(PATH_NOT_FOUND),仍是 No such file or directory"},
		{"中间段是文件", []string{"plain.txt/sub"},
			"cannot access 'plain.txt/sub': Not a directory",
			"真 bash: Not a directory。Windows 把它与「深层不存在」压成同一个 errno 3," +
				"必须靠结构检查区分"},
		{"非法字符", []string{"a<b>.txt"},
			"cannot access 'a<b>.txt': No such file or directory",
			"Win32 给 errno 123(Illegal name),真 bash 仍给 No such file or directory"},
	}
	for _, c := range cases {
		got := lsStderr(t, dir, c.argv...)
		if !strings.Contains(got, c.want) {
			t.Errorf("%s: 期望含 %q,得到 %q\n  依据: %s", c.name, c.want, got, c.why)
		}
	}
}

// ★ 错误文本里**不能**出现 Win32 API 名,也不能出现绝对路径。
//
// 这两条是修这个问题的**根本目的**,所以要正面钉住,而不是只靠"措辞对上了"间接保证。
//
// 为什么独立一条:措辞改对了,将来仍可能有人为了"信息更全"把原始错误拼回去 ——
// 那样 `GetFileAttributesEx` 与临时目录全路径就又回来了。
func TestPathErrLeaksNoWin32Detail(t *testing.T) {
	dir := t.TempDir()

	got := lsStderr(t, dir, "definitely-not-here.txt")
	if got == "" {
		t.Fatal("居然没有错误输出")
	}

	// ① 不能泄漏 Win32 API 名。
	for _, api := range []string{
		"GetFileAttributesEx", "CreateFile", "FindFirstFile", "GetFileInformationByHandle",
	} {
		if strings.Contains(got, api) {
			t.Errorf("错误文本泄漏了 Win32 API 名 %q:%q\n"+
				"  那是实现细节,使用者拿它无从下手", api, got)
		}
	}
	// ② 不能泄漏 Windows 原文措辞(用英文句点结尾的长句)。
	for _, w := range []string{
		"The system cannot find", "The filename, directory name",
		"Access is denied",
	} {
		if strings.Contains(got, w) {
			t.Errorf("错误文本泄漏了 Win32 措辞 %q:%q\n"+
				"  真 bash 给的是 POSIX 说法(No such file or directory)", w, got)
		}
	}
	// ③ 不能把用户写的相对路径替换成绝对路径。
	//
	// 判据是「出现了临时目录的绝对路径」—— 用户写的是 `definitely-not-here.txt`,
	// 输出里不该冒出 `C:\Users\...`。这一条同时保证了用户看到的还是自己写的那串。
	if strings.Contains(got, dir) {
		t.Errorf("错误文本泄漏了绝对路径 %q:%q\n"+
			"  用户写的是相对路径,回显就该是相对路径", dir, got)
	}
	if strings.Contains(got, `:\`) {
		t.Errorf("错误文本里出现了 Windows 绝对路径(candidates):%q", got)
	}
}

// ★ `Not a directory` 与 `No such file or directory` 必须分得开。
//
// 这两条在 Windows 上是**同一个 errno(3)**,只能靠"路径里有没有把文件当目录用"
// 来区分。真 bash 分得很清,而这直接影响 AI 的下一步:前者要它改路径结构,
// 后者要它找文件。
func TestPathErrDistinguishesNotADirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 把文件当目录用 → Not a directory。
	if got := lsStderr(t, dir, "file.txt/inner"); !strings.Contains(got, "Not a directory") {
		t.Errorf("file.txt/inner 应当报 Not a directory(真 bash 如此),得到 %q", got)
	}
	// 纯粹不存在 → No such file or directory(不能误报成 Not a directory)。
	if got := lsStderr(t, dir, "nosuch/inner"); !strings.Contains(got, "No such file or directory") {
		t.Errorf("nosuch/inner 应当报 No such file or directory,得到 %q", got)
	}
	// 两层都不存在 → 同上。
	if got := lsStderr(t, dir, "a/b/c"); !strings.Contains(got, "No such file or directory") {
		t.Errorf("a/b/c 应当报 No such file or directory,得到 %q", got)
	}
}

// ★ `Is a directory`:Windows 上 open 目录会**成功**,失败要到 Read 才发生。
//
// 修之前实测的两个错:
//  1. `cat adir` 报 `Incorrect function.`(Win32 的 ERROR_INVALID_FUNCTION);
//  2. 而且落进了"怎么办: 写输出时出错"那个分支 —— **归因反了**(实际读不出来)。
//
// 真 bash 给 `cat: adir: Is a directory`。现在在 open 之后显式判目录再拦。
func TestCatDirIsADirectoryNotAWriteError(t *testing.T) {
	dir := t.TempDir()
	adir := filepath.Join(dir, "adir")
	if err := os.Mkdir(adir, 0o755); err != nil {
		t.Fatal(err)
	}

	var errw strings.Builder
	code := Cat(context.Background(), dir, []string{"adir"}, nil, io.Discard, &errw)
	if code == 0 {
		t.Fatal("cat 一个目录必须非零退出")
	}
	got := errw.String()
	if !strings.Contains(got, "Is a directory") {
		t.Errorf("应当是 Is a directory(真 bash 的措辞),得到 %q", got)
	}
	// 反向:不能再出现那两句错的。
	if strings.Contains(got, "Incorrect function") {
		t.Errorf("泄漏了 Win32 的 Incorrect function:%q", got)
	}
	if strings.Contains(got, "写输出") {
		t.Errorf("归因错了 —— 这是**读**失败,不是写失败:%q", got)
	}
}

// ★ 写侧错误仍要报成写侧(不能因为修了读侧就全归成读)。
//
// 构造:把 out 换成一个必然写失败的 Writer,读一个正常文件。
// 此时错误来自写侧,措辞应当是 `cannot write output`。
type failingWriter struct{}

func (failingWriter) Write(p []byte) (int, error) {
	return 0, os.ErrClosed // 任何非 nil 错误都行,重点是它来自写侧
}

func TestCatWriteErrorStaysWriteError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}

	var errw strings.Builder
	code := Cat(context.Background(), dir, []string{"a.txt"}, nil, failingWriter{}, &errw)
	if code == 0 {
		t.Fatal("写失败必须非零退出")
	}
	got := errw.String()
	if !strings.Contains(got, "cannot write output") {
		t.Errorf("写侧失败应当报 cannot write output,得到 %q", got)
	}
	// 不该被误报成读失败 —— 那会让人去查文件权限,而问题在输出端。
	if strings.Contains(got, "cannot read") {
		t.Errorf("归因错了 —— 这是**写**失败,不该报 cannot read:%q", got)
	}
}

// ★ 措辞里不该重复命令名。
//
// 三行式的外壳已经把命令名放在第一行(`winbash: ls`),所以"问题"行不该再带
// `ls: ` 前缀 —— 那会变成 `问题: ls: cannot access …`,与真 bash 的
// `ls: cannot access …` 差一层。这条看着琐碎,但它决定了输出像不像 bash。
func TestPathErrDoesNotRepeatCommandName(t *testing.T) {
	dir := t.TempDir()
	got := lsStderr(t, dir, "nope.txt")

	// 只看"问题:"那一行。
	var problem string
	for _, line := range strings.Split(got, "\n") {
		if i := strings.Index(line, "问题: "); i >= 0 {
			problem = line[i+len("问题: "):]
			break
		}
	}
	if problem == "" {
		t.Fatalf("没找到「问题:」行:%q", got)
	}
	if strings.HasPrefix(problem, "ls: ") {
		t.Errorf("「问题」行重复了命令名(外壳第一行已经有了):%q", problem)
	}
	// 正确形态应当以 cannot 开头。
	if !strings.HasPrefix(problem, "cannot ") {
		t.Errorf("「问题」行应当以 cannot 开头(与真 bash 的措辞一致),得到 %q", problem)
	}
}

// readFailer 是一个必定读失败的 Reader,用来测 copyCat 的**读侧**分类。
type readFailer struct{}

func (readFailer) Read(p []byte) (int, error) { return 0, os.ErrClosed }

// failAfterN 前 n 次 Write 成功,之后失败。
//
// 为什么要它:`cat -n` 那段有**两个独立的写点** —— 先 `Fprintf(out, "%6d\t", …)`
// 写行号,再 `out.Write(raw)` 写内容。用一个"必定失败"的 Writer 只会打到第一个
// 写点,第二处就永远测不到(变异验证实测:把第二处标成 read 也全绿)。
// 给 n=1 才能精确落到第二处。
type failAfterN struct{ left int }

func (w *failAfterN) Write(p []byte) (int, error) {
	if w.left <= 0 {
		return 0, os.ErrClosed
	}
	w.left--
	return len(p), nil
}

// ★ copyCat 必须能分辨读侧与写侧的错误。
//
// 为什么这条必须单独存在(变异验证逼出来的):`cat 目录` 现在被 Cat 里的
// **显式判目录**拦住了,根本走不到 copyCat —— 于是「copyCat 把读错误标成写错误」
// 那个变异,靠 TestCatDirIsADirectoryNotAWriteError 抓不到。两条路要分开测:
// 显式判目录管"常见情形",copyCat 的分类管"读了一半才失败"(设备出错、
// 网络盘断开、文件被截断)。
func TestCopyCatClassifiesReadAndWriteErrors(t *testing.T) {
	var lineNo int

	// 读侧。
	err := copyCat(io.Discard, readFailer{}, false, &lineNo)
	if err == nil {
		t.Fatal("读失败应当返回错误")
	}
	var ce *catCopyError
	if !errors.As(err, &ce) {
		t.Fatalf("错误应当是 *catCopyError,得到 %T", err)
	}
	if ce.op != "read" {
		t.Fatalf("读侧失败应当标成 read,得到 %q —— 标成 write 会让使用者去查输出端", ce.op)
	}

	// 写侧。
	err = copyCat(failingWriter{}, strings.NewReader("payload"), false, &lineNo)
	if err == nil {
		t.Fatal("写失败应当返回错误")
	}
	if !errors.As(err, &ce) {
		t.Fatalf("错误应当是 *catCopyError,得到 %T", err)
	}
	if ce.op != "write" {
		t.Fatalf("写侧失败应当标成 write,得到 %q", ce.op)
	}

	// 带 -n 的分支是另一段代码,同样要分类。
	err = copyCat(io.Discard, readFailer{}, true, &lineNo)
	if err == nil {
		t.Fatal("-n 读失败应当返回错误")
	}
	if !errors.As(err, &ce) || ce.op != "read" {
		t.Fatalf("-n 分支的读失败也要标成 read,得到 %+v", ce)
	}
	err = copyCat(failingWriter{}, strings.NewReader("a\n"), true, &lineNo)
	if err == nil {
		t.Fatal("-n 写失败应当返回错误")
	}
	if !errors.As(err, &ce) || ce.op != "write" {
		t.Fatalf("-n 分支的写失败也要标成 write,得到 %+v", ce)
	}

	// `-n` 分支的**第二个写点**(写内容那一步)。上面那条只会打到第一个写点
	// (写行号),所以这里用 failAfterN(1) 让它过掉行号、卡在内容上。
	lineNo = 0
	err = copyCat(&failAfterN{left: 1}, strings.NewReader("a\n"), true, &lineNo)
	if err == nil {
		t.Fatal("-n 写内容那一步失败应当返回错误")
	}
	if !errors.As(err, &ce) || ce.op != "write" {
		t.Fatalf("-n 分支写内容失败也要标成 write,得到 %+v", ce)
	}
}

// ★ 不认识的 Win32 errno 不能原样端出去,也不能瞎猜成某个 POSIX 措辞。
//
// 这条钉的是一处**取舍**:未知错误给 `I/O error (Win32 error N)` —— 错误号比
// 消息更有用(能搜到官方文档),而且明确告诉使用者"这是 Win32 层的东西"。
//
// 背景:我一开始把 ERROR_INVALID_FUNCTION(1) 映射成 `Is a directory`,因为读目录
// 时 Windows 给的就是它。但那是个**通用错误码**,当专属含义用是猜。变异验证发现:
// 写了那条映射之后,「去掉 cat 的显式判目录」就抓不到了 —— 投机映射在替正确实现
// 打掩护。所以删掉它,只留这条可定位的兜底。
func TestUnknownWin32ErrnoIsNotGuessed(t *testing.T) {
	got := posixErrText(syscall.Errno(1), "")
	if got == "Is a directory" {
		t.Fatalf("把通用错误码 1 猜成了 Is a directory —— 别的场景会误报。"+
			"判目录要靠调用方 stat,不能靠错误号。得到 %q", got)
	}
	if !strings.Contains(got, "Win32 error 1") {
		t.Fatalf("未知 errno 应当给出可定位的 Win32 错误号,得到 %q", got)
	}
	if strings.Contains(got, "Incorrect function") {
		t.Fatalf("未知 errno 泄漏了 Win32 原文:%q", got)
	}
}
