package shell

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 计划里的 4 个用例来自 plan Task 12,其中第一条的名称与断言按下面的理由改过;
// 另外两条是本轮补的。

// 这条原名 TestBuildSearchPathAppendsGitBashUsrBinWhenPresent(断言"追加")。
// 语料扫描实测:追加会让 Windows System32 里的 sort.exe/find.exe 抢先,而它们与
// Unix 同名工具语义完全不同(`find . -name` 直接报参数格式不正确)。
// 改为断言**前置** —— 依据是 Git Bash 自己的解析顺序也是 usr\bin 优先。
func TestBuildSearchPathPutsGitBashUsrBinFirst(t *testing.T) {
	got := BuildSearchPath(`C:\Windows\system32`)
	usrBin := gitBashUsrBin()
	if usrBin == "" {
		t.Skipf("这台机器上没有标准位置的 Git Bash;path=%q", got)
	}
	parts := filepath.SplitList(got)
	if len(parts) == 0 || !strings.EqualFold(filepath.Clean(parts[0]), filepath.Clean(usrBin)) {
		t.Fatalf("Git Bash 的 usr\\bin 必须在最前:got=%q want first=%q", got, usrBin)
	}
}

func TestBuildSearchPathDoesNotMutateEnviron(t *testing.T) {
	before := os.Getenv("PATH")
	BuildSearchPath(os.Getenv("PATH"))
	if os.Getenv("PATH") != before {
		t.Fatal("BuildSearchPath mutated PATH; it must not touch the process environment")
	}
}

// 无论输入里有没有那个目录,结果都必须是「usr\bin 恰好出现一次、而且在最前」。
// (旧实现是"已有就不追加",现在是从输入里摘掉再前置 —— 两种情况下都不能出现两份。)
func TestBuildSearchPathKeepsUsrBinExactlyOnceAtFront(t *testing.T) {
	usrBin := gitBashUsrBin()
	if usrBin == "" {
		t.Skip("这台机器上没有标准位置的 Git Bash")
	}

	sep := string(filepath.ListSeparator)
	for _, in := range []string{
		`C:\Windows\system32`,
		`C:\Windows\system32` + sep + usrBin,
		usrBin + sep + `C:\Windows\system32`,
	} {
		got := BuildSearchPath(in)
		n := 0
		for _, p := range filepath.SplitList(got) {
			if strings.EqualFold(filepath.Clean(p), filepath.Clean(usrBin)) {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("usr\\bin 出现了 %d 次(输入 %q → %q)", n, in, got)
		}
		if parts := filepath.SplitList(got); !strings.EqualFold(filepath.Clean(parts[0]), filepath.Clean(usrBin)) {
			t.Fatalf("usr\\bin 不在最前(输入 %q → %q)", in, got)
		}
	}
}

// 这一条钉住整个顺序改动的**主张**:Git Bash 的 usr\bin 必须在 System32 之前,
// 否则 `sort`/`find` 会被 Windows 的同名工具抢走。
func TestBuildSearchPathPrefersGitBashOverWindowsSystem32(t *testing.T) {
	usrBin := gitBashUsrBin()
	if usrBin == "" {
		t.Skip("这台机器上没有标准位置的 Git Bash")
	}
	got := BuildSearchPath(`C:\Windows\system32`)
	lower := strings.ToLower(got)

	iUsr := strings.Index(lower, strings.ToLower(usrBin))
	iSys := strings.Index(lower, `c:\windows\system32`)
	if iUsr < 0 || iSys < 0 {
		t.Fatalf("两个目录都该在结果里: %q", got)
	}
	if iUsr > iSys {
		t.Fatalf("Git Bash 的 usr\\bin 排在 System32 之后 —— 这会让 find/sort 被 Windows 版抢走: %q", got)
	}
}

// 本机实测:usr\bin 里确实有 sort 与 find;没有它们就说明这台机器的情况不同,
// 那么上面的顺序主张也就不适用于它。
func TestGitBashUsrBinCarriesTheConflictingTools(t *testing.T) {
	usrBin := gitBashUsrBin()
	if usrBin == "" {
		t.Skip("这台机器上没有标准位置的 Git Bash")
	}
	for _, name := range []string{"sort", "find", "sed", "awk"} {
		if _, err := os.Stat(filepath.Join(usrBin, name+".exe")); err != nil {
			t.Skipf("%s 不在 %s 里,跳过(这台机器的 Git Bash 布局不同)", name, usrBin)
		}
	}
}
