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
	out, exit, _ := runScriptErr(t, script)
	return out, exit
}

// runScriptErr 同上,另外把 stderr 交回来 —— 有些断言必须靠错误文本区分
// 「winbash 的问题」与「参照物这一次的退化」(见 mktemp 那条断言)。
func runScriptErr(t *testing.T, script string) (string, int, []string) {
	t.Helper()
	s := NewSession()
	defer s.Close()
	out, exit, errs := s.Run(context.Background(), script, false)
	if len(errs) > 0 {
		t.Logf("stderr: %s", strings.Join(errs, " / "))
	}
	return out, exit, errs
}

// 临时目录:挂载点本身、mktemp 的整条链、以及写进去能不能读回来。
func TestMsysMountsWorkFromShellSyntax(t *testing.T) {
	out, exit := runScript(t, `echo hi > /tmp/wb-e2e-a.txt; cat /tmp/wb-e2e-a.txt; rm -f /tmp/wb-e2e-a.txt`)
	if exit != 0 || strings.TrimSpace(out) != "hi" {
		t.Fatalf("写/读 /tmp 失败: exit=%d out=%q", exit, out)
	}

	// mktemp 是 AI 写临时文件的标准写法:它返回 `/tmp/tmp.XXXX`,整条链依赖那个路径可写。
	//
	// 这里**先把 mktemp 给的路径打出来**(MK= / MD=),用它直接判断"参照物有没有退化"。
	// 为什么不能继续从**错误文本**反推(我自己踩的回归,记下来):旧判据找的是
	// 「TEMP 的 basename 在错误里连续出现两次」,而那依赖 Win32 的**绝对路径**被原样
	// 端出来 —— 措辞层修好之后给的是用户写的 POSIX 路径,basename 只出现一次。
	// 于是同一个退化环境从 skip 变成**红**:实测 5/5 稳定红,任何人在这台机器上跑全量
	// 都会看到假失败。判据要盯**事实本身**(mktemp 返回了什么),不是某段文案的形状。
	out, exit, errs := runScriptErr(t, `f=$(mktemp); echo "MK=$f"; echo hi > "$f"; cat "$f"; rm -f "$f"`)
	mkPath, out := splitProbeLine(out, "MK=")
	if exit != 0 || strings.TrimSpace(out) != "hi" {
		// 已知的**参照物退化**,不是 winbash 的映射错误:宿主把 TEMP 设成 Windows 短名路径
		// (`...\Temp\reasonix-session-tmp-123`)时,MSYS 有约 1% 的启动会把 `/tmp` 解析成
		// `%TEMP%` 的父目录。那一次里 mktemp 会把 TMPDIR 投影成
		// `/tmp/<TEMP 的 basename>/tmp.XXX`,于是它创建的目录与 winbash 的 `/tmp`(= TEMP)
		// 根本不是同一个 —— 真 Git Bash 在那一次里同样读不回来。而且它**成片**出现:
		// 实测 -count=100 时 93 次退化、7 次正常,所以门禁会"突然连红几次"
		// (直接派生 mktemp.exe 时更是稳定复现,见 README 的已知差异表)。
		if mktempPathDegraded(mkPath) {
			t.Skipf("参照物这一次退化了(Git Bash 把 /tmp 解析成 %%LOCALAPPDATA%%\\Temp);"+
				"mktemp 给的是 %q,不是 /tmp 的直接子项:%v", mkPath, errs)
		}
		t.Fatalf("mktemp 链失败: exit=%d out=%q mktemp=%q errs=%v", exit, out, mkPath, errs)
	}

	out, exit, errs = runScriptErr(t, `d=$(mktemp -d); echo "MD=$d"; echo x > "$d/a"; cat "$d/a"; rm -rf "$d"`)
	mdPath, out := splitProbeLine(out, "MD=")
	if exit != 0 || strings.TrimSpace(out) != "x" {
		if mktempPathDegraded(mdPath) {
			t.Skipf("参照物这一次退化了(Git Bash 把 /tmp 解析成 %%LOCALAPPDATA%%\\Temp);"+
				"mktemp -d 给的是 %q:%v", mdPath, errs)
		}
		t.Fatalf("mktemp -d 链失败: exit=%d out=%q mktemp=%q errs=%v",
			exit, out, mdPath, errs)
	}
}

// splitProbeLine 从脚本输出里取出我们插进去的那一行探针(<prefix><值>),
// 返回(探针的值, 去掉探针行之后的其余输出)。
//
// 为什么要在脚本里插一行而不是从别处推断:见上面那段注释 —— 从错误文本反推的判据
// 会随文案改动而失效,而文案是本项目正在持续打磨的东西。让脚本自己报告事实最稳。
func splitProbeLine(out, prefix string) (value, rest string) {
	i := strings.Index(out, prefix)
	if i < 0 {
		return "", out
	}
	after := out[i+len(prefix):]
	if j := strings.IndexByte(after, '\n'); j >= 0 {
		return strings.TrimSpace(after[:j]), out[:i] + after[j+1:]
	}
	return strings.TrimSpace(after), out[:i]
}

// mktempPathDegraded 判断 mktemp 给出的路径是不是**参照物退化**的形态。
//
// 正常:`/tmp/tmp.XXXXXX` —— 直接挂在挂载点根下。
// 退化:`/tmp/<TEMP 的 basename>/tmp.XXXXXX` —— 中间多出一层 TEMP 自己的名字,
// 那是 MSYS 把 `/tmp` 解析成 `%TEMP%` 的父目录造成的(见上面长注释)。
//
// 判据只认这种确定形态,所以不会掩盖真的映射故障:真的映射坏了,mktemp 要么失败、
// 要么给出的路径不在 `/tmp` 下,而上面那条「写/读 /tmp」的断言会先红。
func mktempPathDegraded(p string) bool {
	if p == "" {
		return false
	}
	base := filepath.Base(os.Getenv("TEMP"))
	if base == "" || base == "." {
		return false
	}
	return strings.Contains(filepath.ToSlash(p), "/"+base+"/")
}

// ★ 判据本身要能双向判定:认得出退化,也**不能**把正常形态当成退化。
//
// 为什么值得单独测:这个函数决定的是「真失败要不要被 skip 掉」。
// 若它恒返回 true,真的映射故障会被静默跳过 —— 门禁永远绿,而 bug 留在那里。
// 恒 false 只是回到"退化时红"(可察觉),恒 true 才是危险的那一边。
//
// 上面的集成测试在当前机器上只能观察到**退化**那一侧(这台机器的 TEMP 是短名),
// 所以正常侧必须靠这个纯函数测试来钉。
func TestMktempPathDegradedRecognizesBothShapes(t *testing.T) {
	base := filepath.Base(os.Getenv("TEMP"))
	if base == "" || base == "." {
		t.Skip("本机没有可用的 TEMP basename,无从判断退化形态")
	}

	cases := []struct {
		name string
		in   string
		want bool
		why  string
	}{
		{"正常形态", "/tmp/tmp.ABC123", false,
			"直接挂在 /tmp 下 —— 这是正常情况,绝不能 skip"},
		{"退化形态", "/tmp/" + base + "/tmp.ABC123", true,
			"中间多了一层 TEMP 的 basename,正是退化"},
		{"空串", "", false,
			"拿不到 mktemp 输出时不判退化(交给断言去红,不要静默跳过)"},
		{"只有那层没后续", "/tmp/" + base, false,
			"路径不完整,不构成退化形态"},
		{"别的 tmp 根", "/var/tmp/tmp.ABC", false,
			"不含 TEMP 的 basename"},
		{"Windows 形态但无关", `C:\some\where\tmp.ABC`, false,
			"不含 TEMP 的 basename"},
	}
	for _, c := range cases {
		if got := mktempPathDegraded(c.in); got != c.want {
			t.Errorf("%s:mktempPathDegraded(%q) = %v,期望 %v —— %s",
				c.name, c.in, got, c.want, c.why)
		}
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

// ★ `command cd` 与 `builtin cd` 也必须走同一条路径规范化。
//
// 它们与裸 `cd` 是**同一个** mvdan/sh 内建,所以踩同一个坑,但修之前 callGuard 只认
// args[0]=="cd",于是这两条整条绕过规范化。实测后果比"不起效"更糟 —— 都返回 **0**:
//
//	command cd /        → cwd 没动(仍是原目录)
//	builtin cd /c/Users → cwd 变成 <原 cwd>\c\Users(一个不存在的相对路径)
//
// 也就是说脚本以为切过去了、后面所有相对路径全跑在错的目录里,全程没有任何报错。
// 这两条恰恰是"刻意绕过函数/别名"的写法,使用者往往正是在调试这类问题,
// 结果拿到一个假的成功。
func TestCdThroughCommandAndBuiltinTakesEffect(t *testing.T) {
	// `command cd /`:cwd 必须变成 Git 安装根(与裸 cd / 一致)。
	out, exit := runScript(t, `command cd / && pwd`)
	if exit != 0 {
		t.Fatalf("command cd / 失败: exit=%d out=%q", exit, out)
	}
	got := strings.TrimSpace(out)
	if got == "" || strings.HasSuffix(got, "c\\Users") || strings.HasSuffix(got, "c/Users") {
		t.Fatalf("command cd / 之后 pwd 给 %q —— 路径没被规范化", got)
	}
	// 与裸 `cd /` 的结果**逐字相等**才叫一致(两者本就是同一个内建)。
	out2, exit2 := runScript(t, `cd / && pwd`)
	if exit2 != 0 || strings.TrimSpace(out2) != got {
		t.Fatalf("command cd / 给 %q,而 cd / 给 %q —— 两者不一致", got, strings.TrimSpace(out2))
	}

	// `builtin cd /c/Users`:必须真到 Users 目录,而不是 <cwd>\c\Users。
	out, exit = runScript(t, `builtin cd /c/Users && pwd`)
	if exit != 0 {
		t.Fatalf("builtin cd /c/Users 失败: exit=%d out=%q", exit, out)
	}
	got = filepath.ToSlash(strings.TrimSpace(out))
	if !strings.HasSuffix(strings.ToLower(got), "users") || strings.Contains(strings.ToLower(got), "/c/users") {
		t.Fatalf("builtin cd /c/Users 之后 pwd 给 %q —— 被拼成了相对路径", got)
	}

	// 相对路径的 cd 不能被误伤:真的不存在时仍要非零。
	if _, exit := runScript(t, `cd definitely-no-such-dir-xyz`); exit == 0 {
		t.Fatal("cd 到不存在的目录仍返回 0 —— 规范化把失败也一起吞了")
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
