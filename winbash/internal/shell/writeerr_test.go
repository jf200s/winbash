// ★ `>&-`(关闭 stdout)的写错误传播 —— 与真 bash 逐条对齐。
//
// 背景:mvdan/sh 上游把 `>&-` 实现成 `*orig = io.Discard`,于是写"成功"、rc=0、
// 无报错 —— 静默做错事,是项目底线里最坏的形态。真 Git Bash 实测:
//
//	echo x >&-                 rc=1  echo: write error: Bad file descriptor
//	echo a b c >&-             rc=1  同上,但**只报一次**
//	echo x >&-; echo "rc=$?"   rc=0  stdout: rc=1
//	echo x >&-; echo after     rc=0  stdout: after(无 set -e 后续照跑)
//	set -e; echo x >&-; echo after   rc=1(after 不执行)
//	echo x 2>&-                rc=0  stdout: x(只关 stderr 不影响 echo)
//	echo x >&- 2>&-            rc=1  stderr 空(报错也写不出去,rc 仍 1)
//
// 本文件把上面这些行为钉死:关掉 stdout 后写必须**失败**,且失败要变成
// 退出码 1 与一条 `<命令名>: write error: Bad file descriptor`。
package shell

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ★ 关掉 stdout 之后的写必须失败:rc=1、措辞与真 bash 逐字相同。
//
// 「逐字相等」本身就钉住了「只报一次」—— 报两次 stderr 就不等于单行期望了。
// 但仍显式计数一次:防止将来有人把断言放宽成 Contains,把「echo a b c >&- 只
// 报一次」这条最容易漏的行为丢掉(echo 的输出分多段写,首错即停才只报一条)。
func TestCloseStdoutWriteErrorPropagation(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name    string
		script  string
		wantErr string
	}{
		{"echo 写关闭的 stdout", `echo x >&-`, "echo: write error: Bad file descriptor"},
		{"printf 写关闭的 stdout", `printf "x" >&-`, "printf: write error: Bad file descriptor"},
		{"echo 多段输出只报一次", `echo a b c >&-`, "echo: write error: Bad file descriptor"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, errText, exit := runIn(t, dir, c.script)
			if exit != 1 {
				t.Errorf("退出码 %d,期望 1(stderr=%q)", exit, errText)
			}
			if strings.TrimSpace(errText) != c.wantErr {
				t.Errorf("stderr 期望逐字 %q,得到 %q", c.wantErr, errText)
			}
			if n := strings.Count(errText, c.wantErr); n != 1 {
				t.Errorf("写错误应当只报一次(真 bash 如此),得到 %d 次:%q", n, errText)
			}
		})
	}
}

// ★ 写失败对脚本流的影响:$? 可见、无 set -e 后续照跑、set -e 即停。
func TestCloseStdoutKeepsScriptSemantics(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name     string
		script   string
		wantExit int
		wantOut  string
	}{
		{"$? 取到写失败的退出码", `echo x >&-; echo "rc=$?"`, 0, "rc=1\n"},
		{"无 set -e 时后续照跑", `echo x >&-; echo after`, 0, "after\n"},
		{"set -e 遇写失败即停", `set -e; echo x >&-; echo after`, 1, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, errText, exit := runIn(t, dir, c.script)
			if exit != c.wantExit {
				t.Errorf("退出码 %d,期望 %d(stderr=%q)", exit, c.wantExit, errText)
			}
			if out != c.wantOut {
				t.Errorf("stdout 期望逐字 %q,得到 %q", c.wantOut, out)
			}
		})
	}
}

// ★ 连带效应与极限情形。
func TestCloseStdoutSpillover(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "plain.txt"), []byte("a\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("cat 写关闭的 stdout 也失败", func(t *testing.T) {
		// cat 是 winbash ① 层内建,写错误由它自己上报(copyCat 的 write 错误),
		// 措辞是 winbash 的事,这里只钉 rc —— 证明连带效应生效。
		_, _, exit := runIn(t, dir, `cat plain.txt >&-`)
		if exit != 1 {
			t.Errorf("cat 写关闭的 stdout 退出码 %d,期望 1", exit)
		}
	})

	t.Run("stderr 也关时退出码仍是 1", func(t *testing.T) {
		// 报错那行写不出去(必须安静吞掉、不崩、不递归),
		// 但退出码**仍必须是 1** —— 不能因为报错写失败就当成功。
		out, errText, exit := runIn(t, dir, `echo x >&- 2>&-`)
		if exit != 1 {
			t.Errorf("退出码 %d,期望 1(stderr 关闭不改变写失败的事实)", exit)
		}
		if out != "" {
			t.Errorf("stdout 期望为空,得到 %q", out)
		}
		if errText != "" {
			t.Errorf("stderr 已关,报错写不出去,期望为空,得到 %q", errText)
		}
	})
}

// ★ 只关 stderr(`echo x 2>&-`)不影响 stdout:真 bash 是 rc=0、stdout 照常有 x。
//
// 这条守的是「关 stderr」与「关 stdout」两半边不能搅在一起 —— 记账字段如果
// 被 errf 的失败污染,这里会红。
func TestCloseStderrKeepsStdoutWorking(t *testing.T) {
	dir := t.TempDir()
	out, errText, exit := runIn(t, dir, `echo x 2>&-`)
	if exit != 0 {
		t.Errorf("退出码 %d,期望 0(只关 stderr 不该让 echo 失败,stderr=%q)", exit, errText)
	}
	if out != "x\n" {
		t.Errorf("stdout 期望逐字 %q,得到 %q", "x\n", out)
	}
}

// ★ 反向回归:不关 fd 时,echo / printf / cat 的输出与退出码一字不改。
//
// 写错误的传播不能伤及正常写路径 —— 记账(首错即停)如果误触发,最先受害的
// 就是这些最常见的命令。
func TestWriteErrorChangeKeepsNormalOutputIntact(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "plain.txt"), []byte("a\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		script  string
		wantOut string
	}{
		{"echo", `echo x`, "x\n"},
		{"echo 多段", `echo a b c`, "a b c\n"},
		{"printf", `printf "a%s\n" b`, "ab\n"},
		{"cat", `cat plain.txt`, "a\nb\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, errText, exit := runIn(t, dir, c.script)
			if exit != 0 {
				t.Errorf("退出码 %d,期望 0(errs=%q)", exit, errText)
			}
			if out != c.wantOut {
				t.Errorf("stdout 期望逐字 %q,得到 %q", c.wantOut, out)
			}
			if errText != "" {
				t.Errorf("不该有 stderr,得到 %q", errText)
			}
		})
	}
}

// ★ `pwd` 的输出仍是工作目录,rc=0、无 stderr。
//
// 路径按 filepath.Clean 归一后比(盘符大小写、尾分隔符这类平台差异不该
// 让用例红),但目录本身必须一字不差。
func TestPwdStillPrintsWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	out, errText, exit := runIn(t, dir, `pwd`)
	if exit != 0 {
		t.Fatalf("退出码 %d,期望 0(errs=%q)", exit, errText)
	}
	if errText != "" {
		t.Errorf("不该有 stderr,得到 %q", errText)
	}
	got := strings.TrimSpace(out)
	if filepath.Clean(got) != filepath.Clean(dir) {
		t.Errorf("pwd 期望 %q,得到 %q", dir, got)
	}
}

// ★ **管道 broken pipe 必须静默** —— 与 `>&-` 严格区分。
//
// 这条是回归点,踩过一次才写下来。两种场景对 `Write` 来说都返回 error,
// 但真 bash 的语义完全不同(实测 2026-10-18):
//
//	echo foo >&-            → rc=1 + `echo: write error: Bad file descriptor`
//	echo foo | false | true → **完全静默**、rc=0
//
// 前者是使用者明确关掉了 fd,必须响亮;后者是管道读端先走(broken pipe),
// bash 按 SIGPIPE 语义静默处理。不区分的话后者会多出一行
// `echo: write error: write |1: The pipe is being closed.`,把一个正常的
// 管道场景变成噪音 —— 上游 interp 的 4 条用例(TestRunnerRun/#502、#568、
// #569、TestRunnerOpts/#21)就是因此变红的。
func TestBrokenPipeIsSilentUnlikeClosedFd(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name   string
		script string
	}{
		{"管道中间段即退", `echo foo | false | true`},
		{"管道尾段失败", `echo foo | true | false`},
		{"多行写入遇到早退的读端", `for i in 1 2 3 4 5; do echo line$i; done | head -1`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, errText, _ := runIn(t, dir, c.script)
			// 判据只有一个:stderr 里**不许**出现 write error。
			if strings.Contains(errText, "write error") {
				t.Errorf("broken pipe 必须静默,却报了 %q", errText)
			}
		})
	}
}

// ★ 命令**自身已经失败**时,不追加 write error 行(取舍,与真 bash 对齐)。
//
// 实测真 bash:
//
//	type nope >&-   rc=1  stderr: type: nope: not found      ← 只有这一行
//	false >&-       rc=1  stderr: (空)
//
// `type nope` 找不到目标时根本不写 stdout,所以没有写错误可报;`false` 同理。
// 我们的实现是「exit.code != 0 就不结算 writeErr」,在这两条上结果一致。
//
// 这条钉的是**不要多报**:如果将来有人把结算条件放宽成「只要有 writeErr 就报」,
// `type nope` 一类的失败就会多出一行 write error —— 凭空增加的噪音。
// (判据只看「不含 write error」,不断言 type 自己的措辞 —— 那是另一件事。)
func TestFailedCommandDoesNotAppendWriteError(t *testing.T) {
	dir := t.TempDir()
	_, errText, exit := runIn(t, dir, `type nope`)
	if exit == 0 {
		t.Fatal("type 找不到目标必须非零退出")
	}
	if strings.Contains(errText, "write error") {
		t.Errorf("type nope 不该追加 write error 行,得到 %q", errText)
	}
	if strings.TrimSpace(errText) == "" {
		t.Errorf("type nope 至少该报找不到,得到空 stderr")
	}
}
