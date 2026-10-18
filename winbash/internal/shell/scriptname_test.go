package shell

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"winbash/internal/errs"
)

// 脚本运行时报错的前缀要与真 bash 一致:用**用户写的原串**,不是解析出的绝对路径。
//
// 现象(实测):winbash 打 `C:/Users/x/proj/script.sh: line 1: foo: command not found`,
// 真 bash 打 `./script.sh: line 1: foo: command not found`。
//
// 根因**不是** mvdan/sh(README 曾这样归因,已一并改正):那条 `line N:` 报错是 MSYS 的
// bash **子进程**生成的,bash 的运行时报错前缀用 $0,而 $0 就是 runExternal 传给
// env.exe 的脚本参数。所以这里钉的是那个参数:用户写 `./script.sh`,报错就得说
// `./script.sh`。
func TestScriptErrorPrefixIsUserWrittenName(t *testing.T) {
	requireGitBash(t)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "script.sh"), []byte("#!/bin/sh\nfoo\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	s := NewSession()
	_, exit, errsOut := s.Run(context.Background(),
		"cd '"+filepath.ToSlash(dir)+"' && ./script.sh", false)
	if exit == 0 {
		t.Fatalf("脚本里的 foo 不存在,不该成功: errs=%v", errsOut)
	}

	joined := strings.Join(errsOut, "\n")
	// 行号是 **2** 不是 1:脚本第一行是 `#!/bin/sh`,`foo` 在第二行。
	// (实测真 bash 也是 line 2。)
	const want = "./script.sh: line 2: foo: command not found"
	if !strings.Contains(joined, want) {
		t.Fatalf("报错前缀应当是用户写的原串,got:\n%s\nwant 一行含:%q\ndir=%s", joined, want, dir)
	}
	// 绝对路径一个字都不该混进这条报错。
	if strings.Contains(joined, filepath.ToSlash(dir)) || strings.Contains(joined, dir) {
		t.Errorf("报错不该带解析出来的绝对路径,got:\n%s", joined)
	}
}

// POSIX 挂载路径(`/tmp/x.sh` 这类)是**刻意保留的边界**:winbash 的合成挂载与
// bash 自己的 MSYS 挂载表不一定一致,所以那种写法仍把解析出的绝对路径传给
// env.exe —— 原串两种解释可能分叉,传原串反而更坏。这里钉住「不把边界改坏」:
// /tmp 下的脚本照常能跑。
func TestPOSIXMountScriptPathStillRuns(t *testing.T) {
	requireGitBash(t)

	s := NewSession()
	out, exit, errsOut := s.Run(context.Background(), "cd /tmp && pwd", false)
	if exit != 0 {
		t.Fatalf("cd /tmp && pwd 失败(挂载点没了?): exit=%d errs=%v", exit, errsOut)
	}
	tmpDir := strings.TrimSpace(out)
	if tmpDir == "" {
		t.Fatal("pwd 没给目录")
	}

	f, err := os.CreateTemp(tmpDir, "winbash-scriptname-*.sh")
	if err != nil {
		t.Fatalf("在 %s 建临时脚本: %v", tmpDir, err)
	}
	name := filepath.Base(f.Name())
	f.Close()
	defer os.Remove(f.Name())
	if err := os.WriteFile(f.Name(), []byte("#!/bin/sh\necho from-mysys-mount\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	out, exit, errsOut = s.Run(context.Background(), "/tmp/"+name, false)
	if exit != 0 {
		t.Fatalf("/tmp/%s 应当能跑: exit=%d errs=%v", name, exit, errsOut)
	}
	if got := strings.TrimSpace(out); got != "from-mysys-mount" {
		t.Errorf("got %q want %q", got, "from-mysys-mount")
	}
}

// 脚本**不存在**时的报错同样用用户写的原串(winbash 自己的三行式第一行),
// 不能把解析出来的绝对路径当命令名说出去。
func TestMissingScriptErrorUsesUserWrittenName(t *testing.T) {
	dir := t.TempDir()

	s := NewSession()
	_, exit, errsOut := s.Run(context.Background(),
		"cd '"+filepath.ToSlash(dir)+"' && ./no-such-script.sh", false)
	if exit != errs.ExitNotFound {
		t.Fatalf("exit=%d want %d(errs=%v)", exit, errs.ExitNotFound, errsOut)
	}

	joined := strings.Join(errsOut, "\n")
	const want = "winbash: ./no-such-script.sh"
	if !strings.Contains(joined, want) {
		t.Fatalf("三行式第一行应当用用户写的原串,got:\n%s\nwant 含:%q", joined, want)
	}
}
