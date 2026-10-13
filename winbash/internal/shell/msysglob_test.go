package shell

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Cygwin/MSYS2 的 runtime 有一个"好心"的补偿:当调用方**不是** Cygwin 程序时,
// 它会自己把 argv 里的通配符展开一遍(因为 Windows 的 argv 里没有引号语义,
// runtime 怕调用方漏了展开)。
//
// 我们从原生 Windows 进程启动 `/usr/bin/find`,于是引号本该保护的模式被它展开掉了:
//
//	/usr/bin/find: paths must precede expression: `classify_test.go'
//	/usr/bin/find: possible unquoted pattern after predicate `-name'?
//
// 同样的命令在 Git Bash 里是好的 —— 因为调用方也是 Cygwin 进程,runtime 不重复展开。
// 这一条就是钉住那个修复:`MSYS=noglob` 关掉这一层,让 MSYS 工具看到我们展开好的 argv,
// 与它们在真 bash 下的行为一致。
func TestQuotedGlobIsNotReExpandedByMsysTools(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"aaa.go", "bbb.go", "ccc.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	s := NewSession()
	if code := s.runForSetup(t, `cd "`+dir+`"`); code != 0 {
		t.Fatalf("cd 失败:%d", code)
	}

	out, exit, errsOut := s.Run(context.Background(), `find . -maxdepth 1 -name '*.go' | sort`, false)
	if exit != 0 {
		t.Fatalf("exit=%d errs=%v", exit, errsOut)
	}
	if len(errsOut) != 0 {
		t.Fatalf("不该有错误输出(说明 MSYS 又展开了一遍):%v", errsOut)
	}

	got := strings.Split(strings.TrimSpace(out), "\n")
	if len(got) != 2 || !strings.Contains(got[0], "aaa.go") || !strings.Contains(got[1], "bbb.go") {
		t.Fatalf("引号内的 '*.go' 应当由我们的 shell 原样交给 find,得到两个 .go 文件: got=%q", out)
	}
}

// 未加引号的通配符**应当**由我们的 shell 展开(这是 shell 的职责,不是 MSYS 的)。
func TestUnquotedGlobIsExpandedByOurShellNotByMsys(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"one.go", "two.go", "three.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	s := NewSession()
	if code := s.runForSetup(t, `cd "`+dir+`"`); code != 0 {
		t.Fatalf("cd 失败:%d", code)
	}

	// 裸 glob 由 winbash 展开成两个参数;find 收到的是两个具体文件名。
	out, exit, errsOut := s.Run(context.Background(), `printf '[%s]' *.go`, false)
	if exit != 0 {
		t.Fatalf("exit=%d errs=%v", exit, errsOut)
	}
	if strings.TrimSpace(out) != "[one.go][two.go]" {
		t.Fatalf("got %q", out)
	}
}

// runForSetup 跑一条准备性命令(只关心退出码)。
func (s *Session) runForSetup(t *testing.T, script string) int {
	t.Helper()
	_, exit, _ := s.Run(context.Background(), script, false)
	return exit
}
