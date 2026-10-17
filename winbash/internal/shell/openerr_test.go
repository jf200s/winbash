// shell 层 open 失败的措辞(重定向 / source)。
//
// 为什么单独一个文件:这批错误由 mvdan/sh 的 **OpenHandler** 产生,不走
// `internal/builtins/oserr.go` 里那套命令内建的措辞层。修之前它原样端出 Go 的错误:
//
//	open C:\Program Files\Git\nonexistent-dir\x: The system cannot find the path specified.
//
// 三处都不对(与 dba69c7 修过的那批同源):泄漏 Win32 API 名、把用户写的
// `/nonexistent-dir/x` 换成绝对路径、Win32 长句而不是 POSIX 措辞。
//
// 真 Git Bash 的措辞(2026-10-18 实测):
//
//	> /nonexistent-dir/x      line 1: /nonexistent-dir/x: No such file or directory
//	> adir                    line 1: adir: Is a directory
//	> /c/Windows/.../__nope   line 1: /c/Windows/.../__nope: Permission denied
//	source nope.sh            line 1: nope.sh: No such file or directory
//
// 我们少 `line N:`(mvdan/sh 的 errf 不带行号),其余逐字对齐。
//
// **这个文件里最关键的是非 fatal 那条**:为了自定义措辞,必须给 mvdan/sh 打一个
// 补丁(把 `switch err.(type) { case *os.PathError }` 换成 errors.As,见
// third_party/mvdan-sh/WINBASH-PATCH.md §3)。若那个改动出错、让错误掉进上游的
// `default: r.exit.fatal(err)` 分支,后果**不是文案变差,而是整个脚本被终止** ——
// 所以必须有测试盯着这个行为,而不只是盯字符串。

package shell

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"winbash/internal/builtins"
)

// mustNotLeak 断言错误文本里没有任何 Win32 / Go 实现细节。
//
// 抽成函数是因为每条用例都要查同一组词,而且这组词本身就是"修这个问题的目的"。
func mustNotLeak(t *testing.T, got string) {
	t.Helper()
	for _, bad := range []string{
		"open C:", // Go 的 *PathError 前缀 + 绝对路径
		"open D:",
		"The system cannot",   // Win32 长句
		"Access is denied",    // Win32 措辞(真 bash 给 Permission denied)
		"Incorrect function",  // Win32:读目录时给
		"GetFileAttributesEx", // Win32 API 名
		"CreateFile",          // 同上
		"is a directory",      // Go 的**小写**形态;真 bash 是大写 Is a directory
	} {
		if strings.Contains(got, bad) {
			t.Errorf("错误文本泄漏了 %q:%q", bad, got)
		}
	}
}

// ★ 输出 / 追加 / 输入重定向失败时,措辞必须是 POSIX 且保留用户写的路径。
//
// 三类一起测,因为它们走同一个 OpenHandler;分开测容易只覆盖一条就以为都好了。
func TestRedirectOpenErrorUsesPosixWording(t *testing.T) {
	// 一个必然不存在的绝对路径。用长名字降低"机器上恰好存在"的概率。
	const missing = "/no-such-dir-for-winbash-test/file.txt"

	cases := []struct {
		name   string
		script string
		want   []string
	}{
		{"输出重定向", "echo x > " + missing,
			[]string{missing + ": No such file or directory"}},
		{"追加重定向", "echo x >> " + missing,
			[]string{missing + ": No such file or directory"}},
		{"输入重定向", "cat < " + missing,
			[]string{missing + ": No such file or directory"}},
		{"stderr 重定向", "echo x 2> " + missing,
			[]string{missing + ": No such file or directory"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := NewSession()
			defer s.Close()

			_, exit, errs := s.Run(context.Background(), c.script, false)
			if exit == 0 {
				t.Fatalf("重定向失败必须非零退出,得到 0")
			}
			got := strings.Join(errs, "\n")
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("期望含 %q(真 bash 的措辞),得到 %q", w, got)
				}
			}
			mustNotLeak(t, got)
			// 用户写的 POSIX 路径要原样出现,不能被换成 C:\...。
			if !strings.Contains(got, missing) {
				t.Errorf("错误里应当保留用户写的路径 %q,得到 %q", missing, got)
			}
		})
	}
}

// ★ 往一个**已存在的目录**上重定向:必须是 `Is a directory`(大写 I),不是 Go 的
// 小写 `is a directory`,更不能是 `I/O error (Win32 error 536870954)`。
//
// 实测依据:Go 在 Windows 上把这种情况映射成 syscall.EISDIR(errno 536870954),
// 而它的 Error() 给的是小写 "is a directory";真 bash 给 `adir: Is a directory`。
//
// 为什么这条单独一个用例:它是唯一**没有**被 fs.ErrNotExist / fs.ErrPermission
// 覆盖的类别,不加专门映射就会掉进兜底,把一句人话变成一串数字。
func TestRedirectOntoDirectoryIsADirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "adir"), 0o755); err != nil {
		t.Fatal(err)
	}

	s := NewSession()
	defer s.Close()
	if code := s.runForSetup(t, `cd "`+dir+`"`); code != 0 {
		t.Fatalf("cd 失败:%d", code)
	}

	_, exit, errs := s.Run(context.Background(), "echo x > adir", false)
	if exit == 0 {
		t.Fatal("往目录上重定向必须非零退出")
	}
	got := strings.Join(errs, "\n")
	if !strings.Contains(got, "adir: Is a directory") {
		t.Errorf("期望 %q(真 bash 措辞),得到 %q", "adir: Is a directory", got)
	}
	mustNotLeak(t, got)
	// 兜底形态单独点出来 —— 它最容易在删掉 EISDIR 映射后出现。
	if strings.Contains(got, "Win32 error") {
		t.Errorf("落进了 errno 兜底(说明 EISDIR 映射丢了):%q", got)
	}
}

// ★ 无权限位置 -> `Permission denied`(不是 Win32 的 `Access is denied.`)。
func TestRedirectPermissionDeniedWording(t *testing.T) {
	// 受保护的系统目录下的一个不存在文件。若这台机器上它居然可写,跳过而不是假绿。
	const target = `/c/Windows/System32/drivers/etc/__winbash_no_such_file`

	s := NewSession()
	defer s.Close()
	_, exit, errs := s.Run(context.Background(), "echo x > "+target, false)
	got := strings.Join(errs, "\n")

	mustNotLeak(t, got)
	if exit == 0 {
		// 说明这台机器真能写那里 —— 那不是我们能断言的环境,跳过并说明原因。
		t.Skipf("这台机器可以写 %s,无法验证 Permission denied:%q", target, got)
	}
	if !strings.Contains(got, "Permission denied") {
		t.Errorf("期望 %q(POSIX 措辞),得到 %q", "Permission denied", got)
	}
}

// ★ `source` 找不到文件也用同一套措辞(它走同一个 OpenHandler)。
func TestSourceMissingFileUsesPosixWording(t *testing.T) {
	s := NewSession()
	defer s.Close()

	_, exit, errs := s.Run(context.Background(), "source no-such-script.sh", false)
	if exit == 0 {
		t.Fatal("source 不存在的文件必须非零退出")
	}
	got := strings.Join(errs, "\n")
	if !strings.Contains(got, "no-such-script.sh: No such file or directory") {
		t.Errorf("期望含 %q,得到 %q", "no-such-script.sh: No such file or directory", got)
	}
	mustNotLeak(t, got)
}

// ★★★ 最要紧的一条:重定向失败**不能让脚本终止**。
//
// 这是给 mvdan/sh 打补丁(类型断言 -> errors.As)的**核心风险点**。上游的 r.open 是:
//
//	switch err.(type) {
//	case nil:            return f, nil
//	case *os.PathError:  if print { r.errf(...) }        // 非 fatal
//	default:             r.exit.fatal(err)               // fatal:终止整个脚本
//	}
//
// 我们自定义的包装类型若不被 errors.As 认出,就会掉进 default —— 那时错误文案可能
// 看着还行,但**语义已经变了**:一条重定向失败会把整个脚本打断。真 bash 不是这样:
// 那条命令失败、脚本继续。
//
// 所以这条测试断言的是**控制流**,不是字符串。它必须能独立变红,不依赖上面任何一条。
func TestRedirectFailureDoesNotAbortScript(t *testing.T) {
	const missing = "/no-such-dir-for-winbash-test/x"

	s := NewSession()
	defer s.Close()

	out, exit, errs := s.Run(context.Background(),
		"echo x > "+missing+"; echo \"rc=$?\"; echo AFTER-SCRIPT-CONTINUES", false)

	// 1) 后续命令必须真的执行了。
	if !strings.Contains(out, "AFTER-SCRIPT-CONTINUES") {
		t.Fatalf("重定向失败把整个脚本终止了(stdout 里没有后续命令的输出)。\n"+
			"  这条意味着 mvdan/sh 的 errors.As 补丁失效、错误掉进了上游的 fatal 分支。\n"+
			"  stdout=%q exit=%d errs=%v", out, exit, errs)
	}
	// 2) 重定向那条本身要算失败(rc=1),但脚本整体继续跑完。
	//    注意 `$?` 必须紧跟在重定向那条后面 —— 中间夹一条 echo 就是 echo 的退出码了
	//    (我第一版这么写错过,得到 rc=0 还以为产品有问题)。
	if !strings.Contains(out, "rc=1") {
		t.Errorf("重定向失败那条的 $? 应当是 1(与真 bash 一致),stdout=%q", out)
	}
	// 3) 错误仍然要报出来(不因为非 fatal 就沉默)。
	if len(errs) == 0 {
		t.Error("重定向失败必须往 stderr 报一行,不能静默")
	}
}

// ★ 包装类型必须保住 errors.Is 的判定能力(Unwrap 不能省)。
//
// `ShellPathError` 的 Unwrap 返回内层 *os.PathError,所以 `errors.Is(err, fs.ErrNotExist)`
// 这类判定沿链仍然成立。少了 Unwrap 就只有文案看得过去、判定全失效 ——
// 那会影响依赖错误类别的上层逻辑。
//
// 这里刻意用 errors.Is 而不是 os.IsNotExist:后者只做有限展开(文档明说它不处理
// 任意 Unwrap),拿它测这条会得到 false 而误判成"Unwrap 坏了"。
func TestShellPathErrorKeepsUnwrapChain(t *testing.T) {
	_, statErr := os.Stat("/no-such-dir-for-winbash-test/x")
	if statErr == nil {
		t.Skip("这个路径居然存在,跳过")
	}

	wrapped := builtins.ShellPathError("/x", statErr, `C:\x`)

	// 文案是自定义的(不带 Go 的 open 前缀)。
	if strings.Contains(wrapped.Error(), "open ") {
		t.Errorf("包装后的文案不该带 Go 的 open 前缀:%q", wrapped.Error())
	}
	// 但底层判定仍然穿透。
	if !errors.Is(wrapped, fs.ErrNotExist) {
		t.Errorf("errors.Is(..., fs.ErrNotExist) 应当沿 Unwrap 成立,得到 false:%q",
			wrapped.Error())
	}
}
