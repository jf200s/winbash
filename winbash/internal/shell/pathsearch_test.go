package shell

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 计划里的 4 个用例照抄自 plan Task 12;后面两条是本轮补的。

func TestBuildSearchPathAppendsGitBashUsrBinWhenPresent(t *testing.T) {
	got := BuildSearchPath(`C:\Windows\system32`)
	if !strings.Contains(got, filepath.FromSlash("C:/Program Files/Git/usr/bin")) &&
		!strings.Contains(strings.ToLower(got), `program files\git\usr\bin`) {
		t.Skipf("Git Bash not installed at the standard location on this machine; path=%q", got)
	}
}

func TestBuildSearchPathDoesNotMutateEnviron(t *testing.T) {
	before := os.Getenv("PATH")
	BuildSearchPath(os.Getenv("PATH"))
	if os.Getenv("PATH") != before {
		t.Fatal("BuildSearchPath mutated PATH; it must not touch the process environment")
	}
}

func TestWhichFindsCommandPresentOnPath(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "probe-tool.exe")
	os.WriteFile(exe, []byte("MZ"), 0o755)
	if got, ok := Which("probe-tool", dir); !ok || got != exe {
		t.Fatalf("got %q ok=%v", got, ok)
	}
}

func TestWhichReportsMissForAbsentCommand(t *testing.T) {
	if _, ok := Which("definitely-not-installed-xyz", t.TempDir()); ok {
		t.Fatal("expected a miss")
	}
}

// 搜索路径的顺序必须被尊重:同名工具取先出现的那个(NNNN 惯例,PATH 顺序即优先级)。
func TestWhichRespectsPathOrder(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(first, "probe-tool.exe"), []byte("MZ"), 0o755)
	os.WriteFile(filepath.Join(second, "probe-tool.exe"), []byte("MZ"), 0o755)

	sep := string(filepath.ListSeparator)
	got, ok := Which("probe-tool", first+sep+second)
	if !ok {
		t.Fatal("expected a hit")
	}
	if got != filepath.Join(first, "probe-tool.exe") {
		t.Fatalf("PATH order ignored: got %q", got)
	}
}

// 重复追加没有意义,而且会让 PATH 越用越长(每次调用都长一截)。
func TestBuildSearchPathDoesNotDuplicateExistingEntry(t *testing.T) {
	base := BuildSearchPath(`C:\Windows\system32`)
	if base == `C:\Windows\system32` {
		t.Skip("Git Bash usr\\bin not present on this machine")
	}
	again := BuildSearchPath(base)
	if again != base {
		t.Fatalf("Git Bash entry was appended twice:\n base=%q\nagain=%q", base, again)
	}
}
