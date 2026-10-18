// `cd` 与 I/O 编号重定向(`>&` / `<&`)的措辞。
//
// 这两处是「措辞泄漏」那条线的最后一批。它们都在 mvdan/sh 的内建里,走不到
// winbash 的 builtins 措辞层,所以只能给上游打补丁(WINBASH-PATCH.md §4)。
//
// 断言全部对着真 Git Bash 实测(2026-10-18,**主代理亲跑** —— 只读子代理的沙箱会
// 拦掉所有外部进程,所以那次实测没法交给子代理):
//
//	cd /no-such-dir   → cd: <路径>: No such file or directory    rc=1
//	cd <一个文件>      → cd: <路径>: Not a directory              rc=1
//	cd ""             → cd: null directory                       rc=1
//	cd a b            → cd: too many arguments                   rc=2
//	echo x >& 99      → 99: Bad file descriptor                  rc=1
//	echo x 2>&99      → 99: Bad file descriptor                  rc=1
//	echo x <& 99      → 99: Bad file descriptor                  rc=1
//
// 修前分别给:`cd: no such file or directory: "<路径>"`(引号 + 小写 + 两类失败混同)、
// `cd: empty directory path`、`usage: cd [dir]`、`unhandled >& arg: "99"`。
//
// ## 本文件最要紧的一组:分类
//
// 上游把「stat 失败」与「不是目录」合并成同一个分支,于是 `cd <一个文件>` 也报
// "不存在"。拆开后要防两件事:① 两类仍然混同(等于没拆);② `err == nil` 时
// 还去判 `errors.Is`(误判)。
//
// ## 一处我写测试时踩的坑(记下来)
//
// 第一版所有用例直接 `NewSession()` 就在**当前目录**跑,而 `plain.txt` 是用
// `t.TempDir()` 造的、根本没 cd 过去 —— 于是测出 `No such file or directory`
// 还以为是实现没走对分支。依赖文件的用例必须经 `runIn`(定义在 readerr_test.go)
// 在独立临时目录里跑。

package shell

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ★ `cd` 的**四类**失败各给各的措辞,不能混同。
//
// 上游把它们压成了两种(空串 / 其它)。这里逐类钉住,因为「不存在」与「不是目录」
// 决定 AI 下一步做什么:前者去建目录,后者去查自己是不是把文件名当目录了。
func TestCdDistinguishesFailureKinds(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "plain.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		script string
		want   string
		exit   int
	}{
		{"目录不存在", `cd /no-such-dir-for-winbash-test`,
			": No such file or directory", 1},
		{"目标是普通文件", `cd plain.txt`,
			"cd: plain.txt: Not a directory", 1},
		{"空串", `cd ""`,
			"cd: null directory", 1},
		{"参数过多", `cd a b`,
			"cd: too many arguments", 2},
		// `cd -` 但 OLDPWD 未设置:**不能**报成 null directory。
		// 「你还没 cd 过」和「你给了个空目录」指向完全不同的修法。
		{"cd - 但 OLDPWD 未设置", `unset OLDPWD; cd -`,
			"cd: OLDPWD not set", 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, errText, exit := runIn(t, dir, c.script)
			if exit != c.exit {
				t.Errorf("退出码 %d,期望 %d(errs=%q)", exit, c.exit, errText)
			}
			if !strings.Contains(errText, c.want) {
				t.Errorf("期望含 %q,得到 %q", c.want, errText)
			}
		})
	}
}

// ★ 措辞的**形态**:不带引号、首字母大写。
//
// 单靠 Contains 抓不住这两点 —— `cd: plain.txt: Not a directory` 与
// `cd: "plain.txt": not a directory` 都能"包含"某个子串。
//
// 为什么值得单独一条:改措辞最容易的退化就是"只换个词、忘了去引号",
// 而引号会让使用者以为路径里真的有引号。
func TestCdErrorLineHasNoQuotes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "plain.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, errText, _ := runIn(t, dir, `cd plain.txt`)
	line := strings.TrimSpace(errText)
	if strings.Contains(line, `"`) || strings.Contains(line, `'`) {
		t.Errorf("路径不该带引号(真 bash 不带):%q", line)
	}
	// 首字母大写的形态 —— 这是 POSIX strerror 的样子,与项目其它几处措辞一致。
	if !strings.Contains(line, "Not a directory") {
		t.Errorf("期望含 %q(首字母大写),得到 %q", "Not a directory", line)
	}
}

// ★ `>&` / `<&` 的非法编号:点名那个编号,不露内部 token 名。
//
// 上游给 `unhandled >& arg: "99"` —— `>&` 是 `syntax.RedirOperator` 的字符串化,
// 属于**内部 token 名**,使用者既看不懂也搜不到;那对引号同理。
// 真 bash 只做一件事:点名坏掉的 fd。
func TestFdRedirectBadNumberNamesTheFd(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name   string
		script string
	}{
		{"输出复制", `echo x >& 99`},
		{"stderr 复制", `echo x 2>&99`},
		{"输入复制", `echo x <& 99`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, errText, exit := runIn(t, dir, c.script)
			if exit == 0 {
				t.Fatal("非法 fd 编号必须非零退出")
			}
			want := "99: Bad file descriptor"
			if !strings.Contains(errText, want) {
				t.Errorf("期望含 %q(真 bash 的措辞),得到 %q", want, errText)
			}
			// 反面对照:内部 token 名与那对引号都不能出现。
			for _, bad := range []string{"unhandled", "arg:", `"99"`} {
				if strings.Contains(errText, bad) {
					t.Errorf("不该出现 %q(内部实现细节),得到 %q", bad, errText)
				}
			}
		})
	}
}

// ★ 合法写法不能被这次改动带坏。
//
// 改了 `DplOut` / `DplIn` 的 default 分支,必须确认 `1` / `2` / `-` 那三个 case
// 还通 —— 否则 `2>&1` 这类**极常见**的写法会突然失败,那比措辞问题严重得多。
func TestFdRedirectLegalFormsStillWork(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name   string
		script string
		want   string
	}{
		// `2>&1` 把 stderr 并进 stdout:两条 echo 都该落到 stdout,共 2 行。
		{"2>&1 合并", `{ echo o; echo e >&2; } 2>&1 | wc -l`, "2"},
		{">&1 复制到 stdout", `echo hi >&1`, "hi"},
		{"关闭 stdin 不影响 echo", `echo x <&-`, "x"},
		{"exec 之后的 2>&1", `exec 2>&1; echo ok`, "ok"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, errText, _ := runIn(t, dir, c.script)
			if !strings.Contains(out, c.want) {
				t.Errorf("stdout 期望含 %q,得到 %q(errs=%q)", c.want, out, errText)
			}
		})
	}
}

// ★ `echo x >&-`(关闭 stdout)的**已知差异**,诚实钉住。
//
// 真 bash:写到一个已关闭的 fd 会炸 —— `echo: write error: Bad file descriptor`、rc=1。
// winbash:mvdan/sh 把 `>&-` 实现成 `*orig = io.Discard`,于是写"成功"、rc=0、无报错。
//
// 这条差异**已修**(见 TestCloseStdoutReportsWriteError):`>&-` 现在换成一个
// 总是报 `Bad file descriptor` 的 writer,写错误沿 out/outf 记账变成 rc=1 与
// 真 bash 的措辞。上面这段历史注释与这条「只钉不崩」的用例**保留** —— 它钉的
// 「关掉 stdout 后脚本继续」在真 bash 里也成立,与 rc 无关,是独立的底线。
// rc 的断言由紧随其后的新用例负责。
func TestCloseStdoutDoesNotCrash(t *testing.T) {
	dir := t.TempDir()
	out, _, _ := runIn(t, dir, `echo x >&-; echo "after"`)
	if !strings.Contains(out, "after") {
		t.Errorf("关掉 stdout 后脚本应当继续(真 bash 也继续),得到 stdout=%q", out)
	}
}

// ★ 上一条差异已修:写关闭的 stdout 必须失败,rc=1、措辞与真 bash 逐字相同。
//
// 修之前这里会红(winbash 给 rc=0、无报错),那正是「静默做错事」的形态;
// 断言 rc=1 不是固化错误行为,而是把**已修**的行为钉在正确的一侧。
func TestCloseStdoutReportsWriteError(t *testing.T) {
	dir := t.TempDir()
	_, errText, exit := runIn(t, dir, `echo x >&-`)
	if exit != 1 {
		t.Errorf("echo x >&- 退出码 %d,期望 1(真 bash:写关闭的 fd 失败)", exit)
	}
	want := "echo: write error: Bad file descriptor"
	if strings.TrimSpace(errText) != want {
		t.Errorf("措辞应当逐字是 %q,得到 %q", want, errText)
	}
}

// ★ `pushd` / `popd` 与 `cd` **共用** changeDir,措辞必须一起变。
//
// 这条防的是一种很容易漏的回归:只改 `cd` 的用例、没意识到 pushd/popd 走同一个
// 函数。上游测试里也有三条跟着变了 —— 那正是这个共用的证据。
func TestPushdPopdShareTheSameWording(t *testing.T) {
	dir := t.TempDir()
	_, errText, exit := runIn(t, dir, `pushd /no-such-dir-for-winbash-test`)
	if exit == 0 {
		t.Fatal("pushd 到不存在的目录必须非零退出")
	}
	if !strings.Contains(errText, "pushd: ") ||
		!strings.Contains(errText, ": No such file or directory") {
		t.Errorf("pushd 应当复用 cd 的措辞形态(`pushd: <路径>: No such file or directory`),得到 %q", errText)
	}
	if strings.Contains(errText, `"`) {
		t.Errorf("pushd 的路径也不该带引号:%q", errText)
	}
}

// ★ 成功路径不能被带坏 —— 改了 changeDir 的前置判定就要确认正常 `cd` 还能用。
//
// 这条也覆盖「拆分 stat 失败 / 非目录」之后 `err == nil` 那条分支仍被正确走完
// (PWD/OLDPWD 更新在函数末尾,前置判定写错就会漏掉)。
func TestCdSuccessStillWorks(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name   string
		script string
		want   string
	}{
		{"cd 到 /c/Windows 再 pwd", `cd /c/Windows && pwd | grep -c Windows`, "1"},
		{"cd 相对目录", `mkdir -p sub && cd sub && pwd | grep -c sub`, "1"},
		{"cd .. 有效", `mkdir -p a/b && cd a/b && cd .. && pwd | grep -c 'a$'`, "1"},
		{"PWD 跟着更新", `cd /c/Windows && test -n "$PWD" && echo ok`, "ok"},
		{"OLDPWD 被设置", `cd /c/Windows && test -n "$OLDPWD" && echo ok`, "ok"},
		{"cd 到 /tmp", `cd /tmp && pwd | grep -ci temp`, "1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, errText, exit := runIn(t, dir, c.script)
			if exit != 0 {
				t.Errorf("退出码 %d(errs=%q)", exit, errText)
			}
			if !strings.Contains(out, c.want) {
				t.Errorf("stdout 期望含 %q,得到 %q(errs=%q)", c.want, out, errText)
			}
		})
	}
}
