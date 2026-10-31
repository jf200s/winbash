package shell

// 会话内 `export PATH` 的语义:重派生 searchPath(决议 2026-10-31,见
// docs/superpowers/specs/2026-10-31-winbash-session-semantics-design.md §2)。
//
// 每条测试对应方案里的一条钉子:
//   - 原复现(审计里 127 的那条)转绿          → TestSessionExportPATHMakesNewToolRunnable
//   - 负缓存不挡新 PATH(键 = searchPath 串)  → TestSessionExportPATHInvalidatesNegativeCache
//   - 子进程 env 拿到新 searchPath(usr\bin 前置)→ TestSessionExportPATHReachesChildEnv
//   - unset PATH 的保守行为                    → TestSessionUnsetPATHKeepsSearchPathUsable
//   - export 收紧后 usr\bin 仍然可定位          → TestSessionExportPATHStillFindsGitBashTools
//   - syncSearchPath 本体(不变不重派生)       → TestSyncSearchPathOnlyRebuildsOnRealChange

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mvdan.cc/sh/v3/expand"
)

// writeProbeTool 在 dir 里建一个无扩展名 shebang 脚本(usr\bin 里 c_rehash 那一类,
// findExecutable 的 isScript 分支认它,runExternal 交给 env.exe 执行)。
func writeProbeTool(t *testing.T, dir, name, greeting string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "#!/bin/sh\necho " + greeting + "\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// 原复现(审计 2026-10-31):会话里建工具 + export PATH + 立刻执行 —— 原先 127
// (定位吃的是构造期固化的 searchPath),现在应当执行并给出工具自己的输出。
// 两条形态都钉:同一条请求内的 export && 执行,与跨请求(下一条 Run)的生效。
func TestSessionExportPATHMakesNewToolRunnable(t *testing.T) {
	bindir := filepath.Join(t.TempDir(), "bin")
	s := NewSession()
	defer s.Close()

	// 同一条请求:建目录、写脚本、export、执行。注意 PATH 拼接用 ';':会话的
	// PATH 变量是 Windows 形式(继承自进程环境),':' 会把第一个真实目录并进一个坏条目。
	script := fmt.Sprintf(
		`mkdir -p '%[1]s' && printf '#!/bin/sh\necho ran-probe-tool\n' > '%[1]s/wb-path-probe-tool' && export PATH="%[1]s;$PATH" && wb-path-probe-tool`,
		bindir)
	out, exit, errs := s.Run(context.Background(), script, false)
	if exit != 0 {
		t.Fatalf("会话内 export PATH 后工具仍不可执行(原 bug 的形状): exit=%d out=%q errs=%v", exit, out, errs)
	}
	if !strings.Contains(out, "ran-probe-tool") {
		t.Fatalf("工具跑了但输出不对: %q", out)
	}

	// 跨请求:export 的效果留在会话里,下一条命令照常生效。
	out, exit, errs = s.Run(context.Background(), "wb-path-probe-tool", false)
	if exit != 0 || !strings.Contains(out, "ran-probe-tool") {
		t.Fatalf("export PATH 没有跨请求生效: exit=%d out=%q errs=%v", exit, out, errs)
	}

	// 逐命令前缀(PATH=… cmd)也走同一个 sync(hc.Env 里的临时值),应当同样生效;
	// 且前缀只作用于那一条命令 —— 下一条普通命令不受影响。
	out, exit, errs = s.Run(context.Background(),
		fmt.Sprintf(`PATH="%s;$PATH" wb-path-probe-tool; echo after=$?`, bindir), false)
	if exit != 0 || !strings.Contains(out, "ran-probe-tool") || !strings.Contains(out, "after=0") {
		t.Fatalf("逐命令 PATH 前缀未生效或污染了后续命令: exit=%d out=%q errs=%v", exit, out, errs)
	}
}

// 负缓存(locate.go 的 locateMisses)键 = 完整 searchPath 串:PATH 一变、searchPath
// 重派生,键就换了 —— 旧键里记过的 miss 不能挡住新 PATH 下的定位。
func TestSessionExportPATHInvalidatesNegativeCache(t *testing.T) {
	bindir := filepath.Join(t.TempDir(), "bin")
	s := NewSession()
	defer s.Close()

	// 第一步:同名工具还不存在,miss 记进负缓存(旧 searchPath 的键),判 ④ → 127。
	_, exit, _ := s.Run(context.Background(), "wb-negcache-probe-tool", false)
	if exit != 127 {
		t.Fatalf("首跑应当 127(④ 层),got %d", exit)
	}

	// 第二步:工具出现 + export 新目录。若负缓存仍按旧键挡路,这里还是 127。
	writeProbeTool(t, bindir, "wb-negcache-probe-tool", "negcache-probe-ok")
	out, exit, errs := s.Run(context.Background(),
		fmt.Sprintf(`export PATH="%s;$PATH" && wb-negcache-probe-tool`, bindir), false)
	if exit != 0 {
		t.Fatalf("export 新目录后仍被判 ④(负缓存挡住了新 PATH): exit=%d out=%q errs=%v", exit, out, errs)
	}
	if !strings.Contains(out, "negcache-probe-ok") {
		t.Fatalf("输出不对: %q", out)
	}
}

// envList 给子进程的 PATH 必须是**新** searchPath:含 export 进来的目录,
// 且 Git Bash 的 usr\bin 仍在最前(xargs 等「子进程再 exec」靠它活着)。
//
// 钉法刻意走行为而不是路径字符串比对:子进程对 PATH 的显示是 MSYS 形式
// (usr\bin → /usr/bin),且临时目录会被映射成 /tmp(2026-10-31 实测:
// C:\Users\…\Temp\TestXxx\001 → /tmp/TestXxx/001),手写换算不可靠。
// 两条硬断言:① env 打出的 PATH 首项是 /usr/bin;② 子进程**自己**能用它继承的
// PATH 把新目录里的工具找出来并执行(env <工具> 的查找用的是子进程的 PATH)。
func TestSessionExportPATHReachesChildEnv(t *testing.T) {
	if gitBashUsrBin() == "" {
		t.Skip("Git Bash 不在标准位置,跳过")
	}
	bindir := filepath.Join(t.TempDir(), "bin")
	writeProbeTool(t, bindir, "wb-childenv-probe-tool", "childenv-probe-ok")
	s := NewSession()
	defer s.Close()

	if _, exit, errs := s.Run(context.Background(),
		fmt.Sprintf(`export PATH="%s;$PATH"`, bindir), false); exit != 0 {
		t.Fatalf("export 失败: %v", errs)
	}

	// env 是 ③ 层的 MSYS 程序:它把继承的 Windows PATH 转成 POSIX 形式打印,
	// usr\bin 显示为 /usr/bin(2026-10-31 设计文档实测)。
	out, exit, errs := s.Run(context.Background(), "env | grep ^PATH=", false)
	if exit != 0 {
		t.Fatalf("子进程 PATH 查询失败: exit=%d out=%q errs=%v", exit, out, errs)
	}
	line := strings.TrimRight(out, "\r\n")
	if !strings.HasPrefix(line, "PATH=/usr/bin:") {
		t.Fatalf("子进程 PATH 的首项不再是 usr\\bin(xargs 底线被破坏): %q", line)
	}

	// 子进程拿 PATH 去找 export 进来的工具:env.exe 的查找用的是它自己继承的
	// PATH —— 找得到、跑得动,才证明「子进程真的拿到了新目录」,而不是只在
	// 显示层面含糊地包含它。
	out, exit, errs = s.Run(context.Background(), "env wb-childenv-probe-tool", false)
	if exit != 0 {
		t.Fatalf("子进程用继承的 PATH 找不到 export 进来的工具: exit=%d out=%q errs=%v", exit, out, errs)
	}
	if !strings.Contains(out, "childenv-probe-ok") {
		t.Fatalf("输出不对: %q", out)
	}
}

// 管道两段跑在**并发** goroutine 上(mvdan 的 pipeline 除首段外都在后台跑),
// 两段若是 ③ 层外部命令就并发进同一个 Session 的 execHandler —— PATH 变过之后,
// 两段都必须拿到重派生后的 searchPath。本条同时是并发形状的钉子:syncSearchPath
// 的读写都在 atomic.Pointer 快照上,`go test -race` 下跑这条才验得出裸字段双写
// 会炸(与 switchWriter 上锁是同一类问题,见 Session.path 的注释)。
func TestSessionExportPATHPipelineSegmentsBothSync(t *testing.T) {
	if gitBashUsrBin() == "" {
		t.Skip("Git Bash 不在标准位置,跳过")
	}
	s := NewSession()
	defer s.Close()

	// 两段 sed 都是 ③ 层(usr\bin),并发过 execHandler;export 在前,两段都要看到。
	out, exit, errs := s.Run(context.Background(),
		`export PATH="/nonexistent-wb-pipe;$PATH"; printf 'a\n' | sed s/a/b/ | sed s/b/c/`, false)
	if exit != 0 {
		t.Fatalf("export 后管道跑不起来: exit=%d out=%q errs=%v", exit, out, errs)
	}
	if strings.TrimSpace(out) != "c" {
		t.Fatalf("管道两段没都拿到新 searchPath: %q", out)
	}
}

// unset PATH 的刻意保守行为:定位不该跟着瞎。mvdan 的 unset 删掉会话覆盖层后,
// 变量读取落回进程继承值 —— 表现为 searchPath 回到会话初始状态,而不是卡死在
// export 后的(或空的)路径上。
func TestSessionUnsetPATHKeepsSearchPathUsable(t *testing.T) {
	if gitBashUsrBin() == "" {
		t.Skip("Git Bash 不在标准位置,跳过")
	}
	s := NewSession()
	defer s.Close()

	out, exit, errs := s.Run(context.Background(),
		`export PATH="/nonexistent-wb-unset;$PATH"; unset PATH; echo hi | sed s/hi/ho/`, false)
	if exit != 0 {
		t.Fatalf("unset PATH 之后 ③ 层不该变盲: exit=%d out=%q errs=%v", exit, out, errs)
	}
	if strings.TrimSpace(out) != "ho" {
		t.Fatalf("unset PATH 后定位到了错的东西或什么都没定位到: %q", out)
	}
}

// export **收紧** PATH(不带 $PATH)也照常重派生 —— 但 usr\bin 恒前置,所以
// Git Bash 的工具仍然可定位。这是 xargs 取舍在新语义下的延续,不是回归。
func TestSessionExportPATHStillFindsGitBashTools(t *testing.T) {
	if gitBashUsrBin() == "" {
		t.Skip("Git Bash 不在标准位置,跳过")
	}
	s := NewSession()
	defer s.Close()

	out, exit, errs := s.Run(context.Background(),
		`export PATH="/nonexistent-wb-tight"; which sed`, false)
	if exit != 0 {
		t.Fatalf("export 收紧 PATH 后 usr\\bin 的工具丢了: exit=%d out=%q errs=%v", exit, out, errs)
	}
	if !strings.Contains(out, "usr\\bin") {
		t.Fatalf("which sed 没有落在 usr\\bin: %q", out)
	}
}

// syncSearchPath 本体:PATH 没变就是一次比较走人(同一个快照指针,不重派生、
// 不碰文件系统);没设 PATH 保持现状;真变了才重建,且 usr\bin 仍前置。
func TestSyncSearchPathOnlyRebuildsOnRealChange(t *testing.T) {
	s := NewSession()
	before := s.path.Load()
	if before == nil || before.source == "" {
		t.Fatalf("构造期快照没建好: %+v", before)
	}

	// 同值:同一快照(指针相等 = 没有重派生)。
	s.syncSearchPath(expand.ListEnviron("PATH=" + before.source))
	if s.path.Load() != before {
		t.Fatal("PATH 未变却重派生了 searchPath")
	}
	// 没设 PATH(unset 且无继承值):刻意保守,保持现状。
	s.syncSearchPath(expand.ListEnviron("FOO=1"))
	if s.path.Load() != before {
		t.Fatal("PATH 缺失时不应重派生(定位不该变盲)")
	}

	if gitBashUsrBin() == "" {
		t.Skip("Git Bash 不在标准位置,跳过余下断言")
	}
	// 真变了:新快照,search = usr\bin 前置 + 新 PATH。
	s.syncSearchPath(expand.ListEnviron("PATH=C:/wb-sync-probe-dir"))
	after := s.path.Load()
	if after == before {
		t.Fatal("PATH 变了却没有重派生")
	}
	if after.source != "C:/wb-sync-probe-dir" {
		t.Fatalf("快照的 source 不对: %q", after.source)
	}
	if !strings.HasPrefix(after.search, gitBashUsrBin()) {
		t.Fatalf("重派生后 usr\\bin 不再前置: %q", after.search)
	}
	if !strings.HasSuffix(after.search, "C:/wb-sync-probe-dir") {
		t.Fatalf("重派生结果不含新目录: %q", after.search)
	}
}
