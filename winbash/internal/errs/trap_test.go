package errs_test

import (
	"strings"
	"testing"

	"winbash/internal/errs"
)

// 本文件补的是 errs 包里那四个**纯函数**:hasPosixPathArg / hasSingleSlashFlag /
// looksLikeMissingPath / isLetter。本轮的复审实测 `errs` 覆盖率全仓最低(57.1%),
// 原因是原有 4 条用例只覆盖了 bash/WSL 那一条分支,这四个函数一个都没测。
//
// 补它们不是为了数字:本轮修的 §2.2 大小写正是 `looksLikeMissingPath` 里的一个
// 大小写 bug —— 而它会**静默**地只在某些外部工具上不生效。

// 路径不存在的判据要**大小写不敏感**:同一条 POSIX 路径,git 报 "No such file"
// (大写,原先能触发),Node.js 报 "ENOENT: no such file"(小写,原先**漏掉**)。
// 两条是同一个坑,翻译存在的唯一理由就是把那个坑说清楚。
func TestLooksLikeMissingPathIsCaseInsensitive(t *testing.T) {
	missing := []string{
		"fatal: cannot change to '/c/nope': No such file or directory", // git(大写)
		"Error: ENOENT: no such file or directory, open 'C:\\c\\nope'", // node(小写)
		"NO SUCH FILE OR DIRECTORY",                                    // 全大写
		"bash: x: No such file or directory",
		"python: can't open file '/c/x': [Errno 2] No such file", // python 变体
		"cannot find package",
		"command not found",
		"does not exist",
	}
	for _, raw := range missing {
		// 走公开入口,而不是内部函数 —— 这样钉的是"使用者能不能拿到提示"。
		_, remedy, ok := errs.TranslateKnownTrap("git", []string{"-C", "/c/nope"}, raw)
		if !ok {
			t.Errorf("这段输出应当被认成「路径不存在」: %q", raw)
			continue
		}
		if !strings.Contains(remedy, `C:\`) {
			t.Errorf("提示里应当给出 Windows 原生写法: %q", remedy)
		}
	}
}

// 反方向:普通的失败不能被硬套成"路径写法不对"。
func TestTranslateKnownTrapDoesNotOverReach(t *testing.T) {
	// 参数里没有 POSIX 盘符写法 → 不该触发路径翻译(哪怕输出像"找不到")。
	if _, _, ok := errs.TranslateKnownTrap("git", []string{"-C", "C:\\nope"}, "No such file or directory"); ok {
		t.Error("参数里没有 /c/… 写法时,不该说'路径是 POSIX 写法'")
	}
	// 有 POSIX 路径,但输出**不像**路径问题 → 不该触发。
	if _, _, ok := errs.TranslateKnownTrap("git", []string{"-C", "/c/ok"}, "fatal: not a git repository"); ok {
		t.Error("输出不像路径问题时,不该触发路径翻译")
	}
	// 单斜杠开关的翻译要同时要求 argv 里有开关**且**输出是 MSYS 的改写特征。
	if _, _, ok := errs.TranslateKnownTrap("reg", []string{"/v"}, "some unrelated failure"); ok {
		t.Error("只有单斜杠开关、输出不是 MSYS 改写特征时,不该触发")
	}
	if _, remedy, ok := errs.TranslateKnownTrap("reg", []string{"/v"}, "Invalid syntax"); !ok {
		t.Error("单斜杠开关 + MSYS 改写特征,应当触发")
	} else if !strings.Contains(remedy, "winbash") {
		t.Errorf("应当指路到 winbash 自己的子命令: %q", remedy)
	}
}

// hasPosixPathArg 的边界:要 /c/ 这种**盘符**写法,不是任意斜杠开头。
func TestHasPosixPathArgBoundaries(t *testing.T) {
	cases := []struct {
		argv []string
		want bool
	}{
		{[]string{"/c/nope"}, true},
		{[]string{"-C", "/c/Users/x"}, true},
		{[]string{"/C/Upper"}, true},       // 盘符大小写都算
		{[]string{"/c"}, false},            // 太短,只有斜杠加盘符
		{[]string{"/cd"}, false},           // /cd 是开关,不是盘符路径
		{[]string{"/1/x"}, false},          // 盘符必须是字母
		{[]string{"x", "/usr/bin"}, false}, // /usr 不是盘符形式(这是 MSYS 的另一个坑)
		{[]string{"--flag=/c/x"}, false},   // 不在开头,这是另一个话题
		{[]string{"relative/x"}, false},    // 光有斜杠不算
		{nil, false},
	}
	for _, c := range cases {
		// 用"能否触发路径翻译"当观察点,避免直接测未导出的函数。
		_, _, got := errs.TranslateKnownTrap("tool", c.argv, "No such file or directory")
		if got != c.want {
			t.Errorf("argv=%q: 触发=%v, 期望 %v", c.argv, got, c.want)
		}
	}
}

// hasSingleSlashFlag 的边界:MSYS 会改写的只有 `-x` 那种**单斜杠字母**开关。
func TestHasSingleSlashFlagBoundaries(t *testing.T) {
	cases := []struct {
		argv []string
		want bool
	}{
		{[]string{"/v"}, true},
		{[]string{"reg", "query", "HKLM\\X", "/v", "Name"}, true},
		{[]string{"/V"}, true},
		{[]string{"//v"}, false}, // 双斜杠不是 MSYS 开关
		{[]string{"/v1"}, true},  // 只要前两位是斜杠+字母就算
		{[]string{"/1v"}, false}, // 第二位必须是字母
		{[]string{"/"}, false},   // 太短
		{[]string{"-v"}, false},  // 单横线是正常的 UNIX 开关
		{nil, false},
	}
	for _, c := range cases {
		_, _, got := errs.TranslateKnownTrap("tool", c.argv, "Invalid syntax")
		if got != c.want {
			t.Errorf("argv=%q: 触发=%v, 期望 %v", c.argv, got, c.want)
		}
	}
}

// `/c/…` 同时满足两个判定:它是盘符路径(**也**是"斜杠+字母"开头)。
//
// 这条洞见值得单独钉住 —— 两个分支靠**输出特征**区分,不靠参数形状:
//   - 盘符路径 → 工具会把 /c 当字面目录,报 "no such file"
//   - 单斜杠开关 → MSYS 改写之后,工具报 "Invalid syntax"
//
// 所以参数形状重叠**不会**误判,前提是两个分支的 raw 特征保持互斥。哪天有人把
// hasSingleSlashFlag 的翻译条件放宽(例如只看 "cannot" 就算),这条会立刻抓住。
func TestPosixPathArgAlsoLooksLikeSlashFlag(t *testing.T) {
	both := []string{"-C", "/c/nope"}

	if _, _, ok := errs.TranslateKnownTrap("git", both, "No such file or directory"); !ok {
		t.Error("盘符路径 + 'no such file' 应当走路径翻译")
	}
	if _, _, ok := errs.TranslateKnownTrap("git", both, "Invalid syntax"); !ok {
		t.Error("同样的参数配上 MSYS 特征,应当走单斜杠开关翻译(这是词法重叠,不是误判)")
	}

	// 真正要防的是误伤:参数里**只有**普通 UNIX 开关时,两种 raw 都不该触发。
	if _, _, ok := errs.TranslateKnownTrap("tool", []string{"-v", "-r"}, "No such file"); ok {
		t.Error("纯 UNIX 开关不该被当成 MSYS 单斜杠开关")
	}
}
