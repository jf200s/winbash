package builtins

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 本文件钉住 MSYS 挂载点映射。背景见 msyspath.go:
//
//	mvdan/sh 判断绝对路径用 filepath.IsAbs,而它在 Windows 上对 `/tmp` 返回 **false** ——
//	于是 `/tmp`、`/c/Users/x` 被当成相对路径拼到 cwd 后面,变成 `<cwd>\tmp`、
//	`<cwd>\c\Users\x`。后果实测:
//
//	  echo hi > /tmp/t.txt      → open <cwd>\tmp\t.txt 找不到路径
//	  cd /c/Windows; ls         → 列出的还是原目录(cd 压根没生效)
//	  [ -d /tmp ]               → no(真 bash 给 yes)
//	  f=$(mktemp); …cat "$f"    → 整条链失败(mktemp 返回 /tmp/…)
//
// 那是"静默找错地方",与本项目的底线相反,所以补这张挂载表。

func requireGitAndTemp(t *testing.T) (usrBin, temp string) {
	t.Helper()
	usrBin = gitUsrBin()
	if usrBin == "" {
		t.Skip("这台机器上没有 Git 安装,跳过挂载点测试")
	}
	temp = userTempDir()
	if temp == "" {
		t.Skip("拿不到用户 TEMP,跳过挂载点测试")
	}
	return usrBin, temp
}

// ResolveShellPath 把 POSIX 形式改写成 Windows 原生路径。
func TestResolveShellPathMapsMountsAndDrives(t *testing.T) {
	usrBin, temp := requireGitAndTemp(t)
	root := filepath.Dir(filepath.Dir(usrBin))

	cases := []struct {
		in   string
		want string
		why  string
	}{
		{"/tmp", temp, "挂载点本身"},
		{"/tmp/x.txt", filepath.Join(temp, "x.txt"), "挂载点下的文件 —— mktemp 返回的就是这种"},
		{"/usr/bin", usrBin, "usr/bin 挂载点"},
		{"/usr/bin/grep", filepath.Join(usrBin, "grep"), "usr/bin 下的工具"},
		{"/bin/sh", filepath.Join(usrBin, "sh"), "/bin 与 usr/bin 在 Git Bash 里是同一个目录"},

		{"/c/Windows", `C:\Windows`, "盘符形式(比 / 挂载点更具体,必须优先)"},
		{"/d/x", `D:\x`, "另一个盘符"},

		// 不该被改写的:原样返回,交给 mvdan/sh 的默认 handler。
		{"/dev/null", "/dev/null", "漏了这条 `> /dev/null` 会去找一个不存在的文件"},
		{"rel/x", "rel/x", "相对路径由默认 handler 用 mc.Dir 拼"},
		{`C:\native\x`, `C:\native\x`, "Windows 原生路径不动"},
		{"", "", "空串"},
	}
	for _, c := range cases {
		if got := ResolveShellPath(`D:\work\proj`, c.in); got != c.want {
			t.Errorf("ResolveShellPath(%q) = %q, 期望 %q\n  为什么:%s", c.in, got, c.want, c.why)
		}
	}

	// `/tmpfoo` 是**根挂载点下的**一个名字(真 bash 里 `[ -e /tmpfoo ]` 给 missing,
	// 因为它就是"Git 根下没有 tmpfoo",不是"这个写法不该改写")。所以它被解析到
	// Git 根下,再由 os.Stat 判不存在 —— 与真 bash 走的是同一条路。
	if got := ResolveShellPath(`D:\work`, "/tmpfoo"); got != filepath.Join(root, "tmpfoo") {
		t.Errorf("ResolveShellPath(/tmpfoo) = %q, 期望 Git 根下的 tmpfoo", got)
	}

	// `/` 挂载点是 Git 安装根。断言"指向 Git 根"而不是硬编码 C:\Program Files\Git,
	// 这样换安装位置(或 x86 版)测试仍然成立。
	if got := ResolveShellPath(`D:\work`, "/"); got != root {
		t.Errorf("ResolveShellPath(%q) = %q, 期望 %q", "/", got, root)
	}
	if got := ResolveShellPath(`D:\work`, "/etc/hosts"); got != filepath.Join(root, "etc", "hosts") {
		t.Errorf("ResolveShellPath(/etc/hosts) = %q, 期望 Git 根下的 etc\\hosts", got)
	}
}

// ResolveShellPath 对 `/` 的边界:`/c/Windows` 绝不能被当成"根下的 c 目录"。
//
// 这是**踩过的**一处:早先的排除规则只挡住 `/c` 与 `/c/`,于是 `/c/Windows` 落进
// 根挂载点,变成 `C:\Program Files\Git\c\Windows` —— 实测 `cd /c/Windows` 直接报
// "no such file or directory"。
func TestResolveShellPathDoesNotTreatDriveAsRootChild(t *testing.T) {
	requireGitAndTemp(t)
	root := gitInstallRoot()

	for _, p := range []string{"/c/Windows", "/d/x", "/c"} {
		got := ResolveShellPath(`D:\work`, p)
		if strings.HasPrefix(strings.ToLower(got), strings.ToLower(root)) {
			t.Errorf("%q 被当成了根挂载点下的东西: %q", p, got)
		}
	}
	if got := ResolveShellPath(`D:\work`, "/c"); got != `C:\` {
		t.Errorf("ResolveShellPath(/c) = %q, 期望 C:\\", got)
	}
}

// RestoreSyntheticMount 处理 mvdan/sh **预拼接**过的路径(`test`/`[` 走这条)。
func TestRestoreSyntheticMountRecoversPreJoinedPaths(t *testing.T) {
	_, temp := requireGitAndTemp(t)
	root := gitInstallRoot()

	t.Run("原路径不存在时还原挂载点", func(t *testing.T) {
		dir := t.TempDir() // 里面没有 tmp / usr / bin 子目录
		cases := []struct{ in, want string }{
			{filepath.Join(dir, "tmp"), temp},
			{filepath.Join(dir, "usr", "bin"), gitUsrBin()},
			{filepath.Join(dir, "bin"), gitUsrBin()},
		}
		for _, c := range cases {
			if got := RestoreSyntheticMount(dir, c.in); got != c.want {
				t.Errorf("RestoreSyntheticMount(%q) = %q, 期望 %q", c.in, got, c.want)
			}
		}
	})

	t.Run("原路径存在就原样采信,不猜", func(t *testing.T) {
		// 这是最要紧的边界:用户真的在当前目录下建了一个 `tmp` 时,
		// `[ -d tmp ]` 测的必须是**那个** tmp,而不是 %TEMP%。
		dir := t.TempDir()
		mine := filepath.Join(dir, "tmp")
		if err := os.Mkdir(mine, 0o755); err != nil {
			t.Fatal(err)
		}
		if got := RestoreSyntheticMount(dir, mine); got != mine {
			t.Errorf("用户自己的 tmp 被误判成 %%TEMP%%: got %q, 期望 %q", got, mine)
		}
	})

	t.Run("盘符形式从预拼接结果反推", func(t *testing.T) {
		dir := t.TempDir()
		got := RestoreSyntheticMount(dir, filepath.Join(dir, "c", "Windows"))
		if got != `C:\Windows` {
			t.Errorf("got %q, 期望 C:\\Windows", got)
		}
		// `/c` 本身(没有尾斜杠)也要还原成盘符根。
		if got := RestoreSyntheticMount(dir, filepath.Join(dir, "c")); got != `C:\` {
			t.Errorf("got %q, 期望 C:\\", got)
		}
		// 盘符根必须真实存在才采信:单字母非盘符的目录名(理论上是某个不存在的盘)
		// 不该被当盘符。
		if got := RestoreSyntheticMount(dir, filepath.Join(dir, "q", "Windows")); got != filepath.Join(dir, "q", "Windows") {
			t.Errorf("不存在的盘符被采信了: %q", got)
		}
		_ = root
	})

	t.Run("不在 dir 之下的路径不动", func(t *testing.T) {
		dir := t.TempDir()
		other := filepath.Join(t.TempDir(), "elsewhere")
		if got := RestoreSyntheticMount(dir, other); got != other {
			t.Errorf("got %q, 期望原样 %q", got, other)
		}
	})
}

// **已知限制**(刻意不支持,不是遗漏):`[ -e /etc ]` 这类"根挂载点下的子路径"。
//
// 它被预拼成 `<dir>\etc`,而用户写相对路径 `[ -e etc ]` 拼出的是**同一个字符串** ——
// 无法区分。猜成 Git 根会把大量相对路径误判成"存在",而**误判比不支持更糟**:
// 不支持是可预期的失败,误判会让脚本走错分支。
//
// 这条测试把边界写死:改用 `test -d` 的等价写法(能拿到原始路径的场景)不受影响,
// 但 `[ -e /etc ]` 保持"不支持"。哪天有人想"顺手支持一下",这条会提醒他那会误伤什么。
func TestRootMountSubpathIsDeliberatelyNotGuessed(t *testing.T) {
	if _, ok := msysMountFor("/etc"); !ok {
		t.Fatal("/etc 应当能被 msysMountFor 解析(它用于能拿到原始路径的那些调用)")
	}
	if got := ResolveShellPath(`D:\work`, "/etc/hosts"); got == "/etc/hosts" {
		t.Fatal("ResolveShellPath 应当解析 /etc/hosts")
	}

	dir := t.TempDir()
	// 预拼接后的形态与"相对路径 etc"完全相同 —— 所以这里刻意不还原。
	if got := RestoreSyntheticMount(dir, filepath.Join(dir, "etc")); got != filepath.Join(dir, "etc") {
		t.Fatalf("/etc 的子路径不该被猜测还原(会误伤相对路径), got %q", got)
	}
}
