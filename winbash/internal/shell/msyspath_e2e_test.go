package shell

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 本文件是 MSYS 挂载点修复的**端到端**回归 —— 单测在 internal/builtins/msyspath_test.go,
// 这里钉的是"从 shell 语法走一遍"的那条链:重定向、cd、`[ ]`、命令替换。
//
// 背景(实测):mvdan/sh 判断绝对路径用 filepath.IsAbs,而它在 Windows 上对 `/tmp`
// 返回 false —— 于是 `/tmp`、`/c/Users/x` 被当成相对路径拼到 cwd 后面。修之前:
//
//	echo hi > /tmp/t.txt      → open <cwd>\tmp\t.txt 找不到路径
//	f=$(mktemp); …cat "$f"    → mktemp 返回 /tmp/…,整条链失败
//	cd /c/Windows; ls         → cd 压根没生效,列出的还是原目录
//	[ -d /tmp ]               → no(真 bash 给 yes)
//
// 修法是三条互补的:builtins.ResolveShellPath 改写 handler 收到的路径、
// RestoreSyntheticMount 还原 mvdan/sh 预拼接过的路径、ResolvePath 补上挂载表。

// run 跑一条脚本,返回 stdout 与退出码。
func runScript(t *testing.T, script string) (string, int) {
	t.Helper()
	s := NewSession()
	defer s.Close()
	out, exit, errs := s.Run(context.Background(), script, false)
	if len(errs) > 0 {
		t.Logf("stderr: %s", strings.Join(errs, " / "))
	}
	return out, exit
}

// 临时目录:挂载点本身、mktemp 的整条链、以及写进去能不能读回来。
func TestMsysMountsWorkFromShellSyntax(t *testing.T) {
	out, exit := runScript(t, `echo hi > /tmp/wb-e2e-a.txt; cat /tmp/wb-e2e-a.txt; rm -f /tmp/wb-e2e-a.txt`)
	if exit != 0 || strings.TrimSpace(out) != "hi" {
		t.Fatalf("写/读 /tmp 失败: exit=%d out=%q", exit, out)
	}

	// mktemp 是 AI 写临时文件的标准写法:它返回 `/tmp/tmp.XXXX`,整条链依赖那个路径可写。
	out, exit = runScript(t, `f=$(mktemp); echo hi > "$f"; cat "$f"; rm -f "$f"`)
	if exit != 0 || strings.TrimSpace(out) != "hi" {
		t.Fatalf("mktemp 链失败: exit=%d out=%q", exit, out)
	}

	out, exit = runScript(t, `d=$(mktemp -d); echo x > "$d/a"; cat "$d/a"; rm -rf "$d"`)
	if exit != 0 || strings.TrimSpace(out) != "x" {
		t.Fatalf("mktemp -d 链失败: exit=%d out=%q", exit, out)
	}
}

// `[ ]` 走的是 mvdan/sh 的 r.stat —— 它在调用 handler **之前**就把路径拼成了
// `<dir>\tmp`,原始写法在那一步丢了,所以靠 RestoreSyntheticMount 还原。
func TestMsysMountsInTestBuiltin(t *testing.T) {
	cases := map[string]string{
		`[ -d /tmp ] && echo yes || echo no`:        "yes",
		`[ -d /usr/bin ] && echo yes || echo no`:    "yes",
		`[ -d /bin ] && echo yes || echo no`:        "yes",
		`[ -d / ] && echo yes || echo no`:           "yes",
		`[ -e /c/Windows ] && echo yes || echo no`:  "yes",
		`[ -e /c/nope-xyz ] && echo yes || echo no`: "no",
	}
	for script, want := range cases {
		out, _ := runScript(t, script)
		if strings.TrimSpace(out) != want {
			t.Errorf("%s → %q, 期望 %q", script, strings.TrimSpace(out), want)
		}
	}
}

// cd 到挂载点必须真的生效。这一条尤其重要:`cd` 失败时 mvdan/sh 报错但我们**不**改
// 退出码语义,一旦静默失效,后面所有相对路径都跑在错的目录里。
func TestMsysMountCdTakesEffect(t *testing.T) {
	gitRoot := filepath.Dir(filepath.Dir(gitUsrBinPath(t)))

	out, exit := runScript(t, `cd /c/Windows && ls | wc -l`)
	if exit != 0 {
		t.Fatalf("cd /c/Windows 失败: exit=%d out=%q", exit, out)
	}
	if n := strings.TrimSpace(out); n == "0" || n == "" {
		t.Fatalf("cd /c/Windows 之后 ls 给 %q —— cd 没有生效", n)
	}

	// cd 到 Git 安装根,然后**相对**读一个那里确实有的目录。
	out, exit = runScript(t, `cd / && ls -1 etc | wc -l`)
	if exit != 0 {
		t.Fatalf("cd / 失败: exit=%d out=%q", exit, out)
	}
	if strings.TrimSpace(out) == "0" {
		t.Fatalf("cd / && ls etc 给了 0 项 —— 根挂载点没生效")
	}
	_ = gitRoot
}

// **刻意不支持**:`/` 挂载点下的子路径走 `[ ]` 时无法安全还原,见
// builtins.RestoreSyntheticMount 的注释 —— 它与相对路径拼出同一个字符串,猜就会误伤。
//
// 这条测试把边界钉住:哪天有人"顺手支持一下",它必须同时改这里并解释怎么避免误判。
func TestRootMountSubpathViaTestIsDeliberatelyUnsupported(t *testing.T) {
	// `ls`(能拿到原始路径)是支持的 —— 这才是有事时该用的写法。
	out, exit := runScript(t, `ls /etc 2>/dev/null | wc -l`)
	if exit != 0 || strings.TrimSpace(out) == "0" {
		t.Fatalf("ls /etc 应当能用(它走 ResolvePath),得到 exit=%d out=%q", exit, out)
	}

	// 与之对照:`[ -e /usr ]` 这类走 r.stat 的**拿不回原路径**。
	// 这里不断言它一定失败(行为可能随 mvdan/sh 版本变),只记录当前事实。
	out, _ = runScript(t, `[ -e /usr ] && echo yes || echo no`)
	t.Logf("已知限制: [ -e /usr ] → %q(真 bash 给 yes)", strings.TrimSpace(out))
}

// 用户真的在当前目录下建了一个 `tmp` 时,`[ -d tmp ]` 测的必须是**那个** tmp,
// 而不是 %TEMP%。这是整个还原逻辑里最容易出错、后果最坏的一处:
// 猜错会让脚本基于"这个目录存在"走错分支。
func TestLocalTmpDirWinsOverMountGuess(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tmp", "mine"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := NewSession()
	defer s.Close()
	// 用 cd 而不是 SetDir(没有那个方法):这条语料本身也要走通。
	if out, exit, _ := s.Run(context.Background(), "cd '"+strings.ReplaceAll(dir, "'", `'\''`)+"'", false); exit != 0 {
		t.Skipf("cd 到临时目录失败: %q", out)
	}
	out, exit, _ := s.Run(context.Background(), `[ -d tmp ] && cat tmp/mine`, false)
	if exit != 0 || strings.TrimSpace(out) != "mine" {
		t.Fatalf("当前目录下的 tmp 被误判成 %%TEMP%%: exit=%d out=%q", exit, out)
	}
}

// gitUsrBinPath 与 builtins.gitUsrBin 是同一份候选表(两个包无法互相 import)。
func gitUsrBinPath(t *testing.T) string {
	t.Helper()
	for _, d := range []string{
		`C:\Program Files\Git\usr\bin`,
		`C:\Program Files (x86)\Git\usr\bin`,
	} {
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			return d
		}
	}
	t.Skip("这台机器上没有 Git 安装")
	return ""
}

// gitInstallRootForTest 是 Git 安装根,也就是 `/` 挂载点的目标。
//
// 真 bash 的证据:`cd / && pwd -W` 给 `C:/Program Files/Git`。
func gitInstallRootForTest() string {
	for _, d := range []string{
		`C:\Program Files\Git\usr\bin`,
		`C:\Program Files (x86)\Git\usr\bin`,
	} {
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			return filepath.Dir(filepath.Dir(d))
		}
	}
	return ""
}
