package shell

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// pwsh 被 ② 层接管("透明覆盖")。
//
// 为什么值得单独钉一条:这不是一个普通命令,而是**覆盖**了一个真实存在的 exe。
// 走错层的后果很隐蔽 —— 输出照样对,只是每次都白付 ~350ms 的启动钱,
// 而"慢"不会让人怀疑路由。所以断言不只看输出,还看路由本身:
// ③ 层只是 exec 一个新进程,永远不会启动常驻引擎;引擎被启动过,就说明走了 ② 层。
func TestPwshIsTakenOverByLayerTwo(t *testing.T) {
	s := NewSession()
	defer s.Close()

	out, exit, errsOut := s.Run(context.Background(), `pwsh -c 'Write-Output routed'`, false)
	if exit != 0 {
		t.Fatalf("exit=%d errs=%v", exit, errsOut)
	}
	if strings.TrimSpace(out) != "routed" {
		t.Fatalf("out=%q(want routed)", out)
	}
	if strings.HasPrefix(s.EngineStatus(), "pwsh=not-started") {
		t.Fatalf("pwsh 没被 ② 层接管(引擎没启动),走了 ③ 层: %s", s.EngineStatus())
	}
}

// 反向保证:③ 层的命令**不惊动**常驻引擎。
// 少了这条,上面那条可以被"反正每条命令都会启动引擎"糊弄过去。
func TestLayerThreeCommandsDoNotStartTheEngine(t *testing.T) {
	s := NewSession()
	defer s.Close()

	_, _, _ = s.Run(context.Background(), "git --version", false)
	if !strings.HasPrefix(s.EngineStatus(), "pwsh=not-started") {
		t.Fatalf("③ 层的命令不该启动 ② 层的引擎: %s", s.EngineStatus())
	}
}

// ② 层的 env/dir ctx 值现在只对 `pwsh` 子命令构造(shell.go 的 layerWinmgmt 分支
// 按命令名收进了 if)。这条从 ① 层会话一路走到常驻引擎,钉住「pwsh 拿得到
// export 的环境变量、以及会话 cd 过的目录」:分支收错了对象(比如漏掉 pwsh),
// 这里会红。svc/reg/ps/sys 不读这两个值,由 winmgmt 包自己的测试覆盖。
func TestPwshSubcommandSeesSessionEnvAndDir(t *testing.T) {
	s := NewSession()
	defer s.Close()

	dir := t.TempDir()
	// 同一条脚本里先 export、再 cd、最后 pwsh —— 三件事都在常驻引擎动手**之前**
	// 发生,引擎启动那一刻继承的 env/cwd 必然是旧的,只有 ctx 值被真正带上才会对。
	script := "export WINBASH_PWSH_PROBE=7; cd '" + filepath.ToSlash(dir) + "'; " +
		`pwsh -c 'Write-Output $env:WINBASH_PWSH_PROBE; Write-Output (Get-Location).Path'`
	out, exit, errsOut := s.Run(context.Background(), script, false)
	if exit != 0 {
		t.Fatalf("exit=%d errs=%v", exit, errsOut)
	}

	wantBase := strings.ToLower(filepath.Base(filepath.ToSlash(dir)))
	var sawEnv, sawDir bool
	for _, ln := range strings.Split(strings.TrimRight(out, "\r\n"), "\n") {
		switch strings.TrimSpace(ln) {
		case "7":
			sawEnv = true
		default:
			if strings.ToLower(filepath.ToSlash(strings.TrimSpace(ln))) == wantBase ||
				strings.HasSuffix(strings.ToLower(filepath.ToSlash(strings.TrimSpace(ln))), wantBase) {
				sawDir = true
			}
		}
	}
	if !sawEnv {
		t.Fatalf("pwsh 没拿到会话 export 的环境变量(out=%q errs=%v)—— env ctx 值没有带上", out, errsOut)
	}
	if !sawDir {
		t.Fatalf("pwsh 的 (Get-Location) 不在会话目录 %s 下(out=%q)—— dir ctx 值没有带上", dir, out)
	}
}
