// 各命令的"问题"行**格式**必须各按 GNU 的做法,而不是内部统一成一种。
//
// 为什么单独一个文件(2026-10-18):
// `oserr.go` 起初把 ls / cat / grep 统一成 `cannot access 'x': <原因>`。那是
// **我方的一厢情愿** —— 实测真 GNU coreutils 内部根本不一致:
//
//	ls nope     → ls: cannot access 'nope': No such file or directory
//	cat nope    → cat: nope: No such file or directory
//	grep x nope → grep: nope: No such file or directory
//	wc nope     → wc: nope: No such file or directory
//	head nope   → head: cannot open 'nope' for reading: No such file or directory
//	sed nope    → sed: can't read nope: No such file or directory
//
// 也就是说 `cannot access` 是 **ls 的**措辞,cat / grep / wc 给的是裸的
// `<路径>: <原因>`。README 当时声称三者都"与真 bash 逐字对齐",实际只有 ls 对 ——
// 属于「文档与实现不许对不上」那条底线,所以改实现而不是改声明。
//
// 这组测试钉的是**格式选择本身**:cat / grep 不许带 `cannot access`,ls 必须带。
// 少了它,任何人"为了统一"再把三者改成一种,都不会有测试变红 ——
// 这正是当时那个偏差能溜过去的机制。

package builtins

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ★ cat 的路径错误用**裸格式** `<路径>: <原因>`,不带 `cannot access` / `cannot open`。
//
// 实测真 bash(2026-10-18):
//
//	cat nope.txt → cat: nope.txt: No such file or directory
//	cat adir     → cat: adir: Is a directory
//
// 三行式第一行已经给了 `winbash: cat`,所以"问题"行要对齐的是命令名**之后**那一段。
func TestCatPathProblemUsesBareCoreutilsFormat(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "adir"), 0o755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		argv []string
		want string
	}{
		{"文件不存在", []string{"nope.txt"},
			"nope.txt: No such file or directory"},
		{"一个目录", []string{"adir"},
			"adir: Is a directory"},
		{"深层不存在", []string{"a/b/c.txt"},
			"a/b/c.txt: No such file or directory"},
	}
	for _, c := range cases {
		var errw strings.Builder
		code := Cat(context.Background(), dir, c.argv, nil, io.Discard, &errw)
		if code == 0 {
			t.Errorf("%s: 必须非零退出", c.name)
			continue
		}
		got := errw.String()
		if !strings.Contains(got, c.want) {
			t.Errorf("%s: 期望含 %q(真 bash 的裸格式),得到 %q", c.name, c.want, got)
		}
		// 反面对照:GNU cat 不用 cannot access / cannot open 这两个动词。
		// 少了这条,"加回一个 cannot open 前缀"这种退化不会有任何测试变红。
		for _, bad := range []string{"cannot access", "cannot open", "cannot read"} {
			if strings.Contains(got, bad) {
				t.Errorf("%s: 不该出现 %q —— GNU cat 给的是裸格式 "+
					"(`cat: nope.txt: No such file or directory`),得到 %q",
					c.name, bad, got)
			}
		}
	}
}

// ★ grep 同上:裸格式,不带 `cannot access`。
//
// 实测真 bash(2026-10-18):
//
//	grep x nope.txt → grep: nope.txt: No such file or directory
//	grep x adir     → grep: adir: Is a directory
//
// 第二条是之前漏得更彻底的一处:winbash 曾给中文 `adir 是目录`,与真 bash 完全不搭。
func TestGrepPathProblemUsesBareCoreutilsFormat(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "adir"), 0o755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		argv []string
		want string
	}{
		{"文件不存在", []string{"x", "nope.txt"},
			"nope.txt: No such file or directory"},
		{"一个目录(未加 -r)", []string{"x", "adir"},
			"adir: Is a directory"},
	}
	for _, c := range cases {
		var out, errw bytes.Buffer
		code := Grep(context.Background(), dir, c.argv, nil, &out, &errw)
		if code == 0 {
			t.Errorf("%s: 必须非零退出", c.name)
			continue
		}
		got := errw.String()
		if !strings.Contains(got, c.want) {
			t.Errorf("%s: 期望含 %q(真 bash 的裸格式),得到 %q", c.name, c.want, got)
		}
		if strings.Contains(got, "cannot access") {
			t.Errorf("%s: 不该出现 cannot access —— GNU grep 给的是裸格式,得到 %q", c.name, got)
		}
		// 那条中文措辞是既存偏差,单独点出来免得回潮。
		if strings.Contains(got, "是目录") && !strings.Contains(got, "Is a directory") {
			t.Errorf("%s: 还是中文措辞(%q)—— 真 bash 是 `%s`", c.name, got, c.want)
		}
	}
}

// ★ ls 反过来**必须**带 `cannot access` —— 别在"统一成裸格式"时把它也改掉。
//
// 实测真 bash(2026-10-18):
//
//	ls nope.txt    → ls: cannot access 'nope.txt': No such file or directory
//	ls -d nope.txt → ls: cannot access 'nope.txt': No such file or directory
//
// 这条与上面两条成对:只钉住 cat/grep 该裸、不钉住 ls 该带动词,
// 那"全部改成裸格式"就只会有 cat/grep 的测试变绿,没人挡得住。
//
// **`-d` 分支必须单独测**(这处是变异验证逼出来的):ls 的普通分支与 `-d` 分支各
// 有一处 `userPathProblem("cannot access", …)`,只测普通分支的话,把 `-d` 那处
// 改成裸格式不会有任何测试变红 —— 实测过,确实漏了。
func TestLsPathProblemKeepsCannotAccess(t *testing.T) {
	dir := t.TempDir()

	want := "cannot access 'nope.txt': No such file or directory"
	bare := "nope.txt: No such file or directory"

	for _, c := range []struct {
		what string
		argv []string
	}{
		{"普通分支", []string{"nope.txt"}},
		{"-d 分支", []string{"-d", "nope.txt"}},
		{"-ld 分支", []string{"-ld", "nope.txt"}},
	} {
		got := lsStderr(t, dir, c.argv...)
		if !strings.Contains(got, want) {
			t.Errorf("%s: ls 必须用 cannot access 格式(真 bash: %s),得到 %q",
				c.what, want, got)
			continue
		}
		// 裸格式(没有 cannot access)是错的 —— 那会把 ls 与 cat/grep 混为一谈。
		if strings.Contains(got, bare) && !strings.Contains(got, "cannot access") {
			t.Errorf("%s: ls 变成了裸格式(%q)—— 真 bash 的 ls 用 cannot access",
				c.what, got)
		}
	}
}

// ★ 写侧与 stdin 那两条**有意不照搬 GNU**,理由必须说明白并钉住。
//
// 真 bash:
//   - `cat 大文件 | head` 被 SIGPIPE **直接杀掉**,rc=0、stderr 什么也没有;
//   - `cat f > /bad/x` 由 **shell 层**拦截,cat 根本没启动。
//
// winbash 不会因 SIGPIPE 被杀(它自己管着输出端),所以那个错误必须自己报 ——
// 报出来比照抄一个"什么都没有"更有用。这条测试防止有人"为了对齐 GNU"
// 把这两条也删成静默。
func TestCatWriteAndStdinProblemsStayExplicit(t *testing.T) {
	// 写侧:读一个正常文件,但 out 必然写失败。
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("payload\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var errw strings.Builder
	Cat(context.Background(), dir, []string{"a.txt"}, nil, failingWriter{}, &errw)
	got := errw.String()
	if !strings.Contains(got, "cannot write output") {
		t.Errorf("写侧失败必须报成写侧(真 bash 那里被 SIGPIPE 杀成静默,我们不会,所以要自己报),得到 %q", got)
	}

	// stdin:直接调 catCopyProblem 验口径(不依赖真能构造出 stdin 读失败)。
	if p := catCopyProblem("(standard input)", os.ErrClosed); !strings.HasPrefix(p, "cannot read standard input:") {
		t.Errorf("stdin 失败要给显式说明,得到 %q", p)
	}
}
