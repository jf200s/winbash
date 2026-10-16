package builtins

import (
	"os"
	"path/filepath"
	"strings"
)

// msysMounts 解决"AI 写 /tmp、真 bash 能用、winbash 找不到"这一类差异。
//
// 真 Git Bash 有一张**挂载表**(`mount` 命令可查),它把几个 POSIX 路径映到 Windows 目录:
//
//	C:/Program Files/Git              on  /            (安装根)
//	C:/Program Files/Git/usr/bin      on  /bin
//	<用户 TEMP>                        on  /tmp
//	C:                                on  /c
//	D:                                on  /d
//
// winbash 原先只实现最后两条(盘符形式),于是 `/tmp`、`/usr/bin`、`/bin` 全落到
// 「当前盘根目录下的同名目录」。这不是"少支持一个写法",是**静默找错地方**:
// `f=$(mktemp); echo hi > "$f"; cat "$f"` 是 AI 写临时文件的标准写法,mktemp 返回
// `/tmp/tmp.XXXX`,于是整条链上每个 `open` 都去 `<cwd>\tmp\` 找一个并不存在的目录。
//
// 这里补上前三条。**只走一层**:`/tmp` 与 `/tmp/foo` 会被映射,`/usr/bin/grep` 会被映射,
// 但不做符号链接展开、不做前缀逐级回溯 —— 映射表本身就是一层,复杂化只会引入新的
// "看起来对但指向别处"的可能。
//
// 不改任何环境变量,也不写文件:纯查表(见 Global Constraints)。

// msysMountFor 返回 p 是否落在某个 MSYS 挂载点下,以及它对应的 Windows 路径。
//
// 第二个返回值 ok 为 false 时,调用方应继续走原来的解析(盘符形式、原生绝对路径、相对路径)。
func msysMountFor(p string) (string, bool) {
	if len(p) == 0 || p[0] != '/' {
		return "", false
	}

	// 顺序要紧:先试更长的挂载点(`/usr/bin` 必须在 `/` 之前)。
	for _, m := range msysMounts {
		rest, matched := trimMount(p, m.mount)
		if !matched {
			continue
		}
		base := m.windows()
		if base == "" {
			continue // 这个挂载点在当前机器上不存在
		}
		if rest == "" {
			return base, true
		}
		return filepath.Join(base, filepath.FromSlash(rest)), true
	}
	return "", false
}

// ResolveShellPath 是给 mvdan/sh 的文件访问 handler 用的路径改写。
//
// 为什么必须有这一层:mvdan/sh 自己打开文件时(重定向、`[ -d ... ]`、glob),
// 判断"是不是绝对路径"用的是 `filepath.IsAbs` —— 而在 Windows 上
// `filepath.IsAbs("/tmp")` 返回 **false**!于是它把 `/tmp`、`/c/Users/x` 这类
// POSIX 路径当成**相对路径**,拼到 cwd 后面,变成 `<cwd>\tmp`、`<cwd>\c\Users\x`。
//
// 后果实测:
//
//	echo hi > /tmp/t.txt        → open <cwd>\tmp\t.txt: 找不到路径
//	echo hi > /c/Users/x.txt    → open <cwd>\c\Users\x.txt: 找不到路径
//	cd /c/Windows; ls           → 列出的还是原目录(cd 压根没生效)
//	[ -d /tmp ] && echo yes     → no(真 bash 给 yes)
//
// 它只对 `/dev/null` 做了 Windows 特例(`path = "NUL"`),别的都没管。
//
// 这里的策略是**最小改动**:只改写"看起来是 POSIX 绝对路径"的那些,
// 其余原样返回,交给 mvdan/sh 的默认 handler 处理 —— 相对路径的拼接与
// `/dev/null` 的特例都保持它的原行为,不重复实现、也不覆盖。
//
// dir 是 shell 的当前目录(handler context 里的那个),用于保留默认的相对路径语义。
func ResolveShellPath(dir, path string) string {
	if path == "" {
		return path
	}
	// `/dev/null` 原样交给默认 handler:它在 Windows 上会转成 NUL,并调整 flag。
	// 这一条**不能漏** —— 漏了 `> /dev/null` 会去找一个不存在的文件。
	if path == "/dev/null" {
		return path
	}
	slash := filepath.ToSlash(path)
	if len(slash) == 0 || slash[0] != '/' {
		return path // 相对路径:让默认 handler 用 mc.Dir 拼
	}
	// `/c/...` 这种盘符形式:先判它,因为它比 `/` 挂载点更具体。
	if drive, rest, ok := splitPOSIXDrive(slash); ok {
		return filepath.Join(drive+`\`, filepath.FromSlash(rest))
	}
	if win, ok := msysMountFor(slash); ok {
		return win
	}
	// 落在这里的是 `/dev/fd/...` 这类 msysMountFor 覆盖不到的路径 ——
	// 原样返回,失败方式与改动前一致(不会变得更糟)。
	return path
}

// RestoreSyntheticMount 修 mvdan/sh **预拼接**造成的路径污染。
//
// 与 ResolveShellPath 的区别在于时机:那个能拿到原始路径,这个拿不到。
// 因为 mvdan/sh 的 `test` / `[` 走的是 `r.stat` —— 它在调用 handler **之前**就先做了
//
//	path := absPath(r.Dir, name)   // filepath.Join(dir, "/tmp") == <dir>\tmp
//
// 于是 handler 收到的已经是 `<dir>\tmp`,原始写法(`/tmp`)在那一步就丢了。
// 无法从 `<dir>\tmp` 反推它是 `/tmp` 还是 `tmp`(两者 Join 结果完全相同)。
//
// 所以这里的策略是**只在原路径不存在时才尝试还原**,并且要求"还原出来的那个目录
// 确实存在"才采信:
//
//	[ -d /tmp ]                 → <dir>\tmp 不存在 → 还原成 %TEMP% → 存在 → yes ✅
//	[ -d "$PWD/tmp" ](真建了)   → <dir>\tmp 存在  → 原样采信,不猜测 ✅
//
// 第二条是关键:用户真的在当前目录下有个 `tmp` 子目录时,我们的猜测**必须让位**给事实。
// 而真 bash 在 `<dir>/tmp` 不存在时对 `/tmp` 也给 yes —— 所以第一种情况与它一致。
func RestoreSyntheticMount(dir, path string) string {
	if path == "" {
		return path
	}
	// 原路径存在就原样采信,不做任何猜测。
	if _, err := os.Lstat(path); err == nil {
		return path
	}
	clean := filepath.Clean(path)
	for _, alt := range syntheticCandidates(dir) {
		if strings.EqualFold(filepath.Clean(alt.synthetic), clean) {
			if _, err := os.Lstat(alt.windows); err == nil {
				return alt.windows
			}
		}
	}
	// 盘符形式单独走反推:它要拼出完整路径(`<dir>\c\Windows`),候选表只能列首段。
	if win, ok := restoreDriveFromSynthetic(dir, clean); ok {
		return win
	}
	return path
}

// restoreDriveFromSynthetic 从"被预拼接过的"路径反推盘符形式。
//
// `<dir>\c\Windows` 里那段 `c\Windows` 是 mvdan/sh 把 `/c/Windows` 用 Join 拼出来的,
// 所以取相对 dir 的第一段:单字母即盘符。用 Rel 而不是字符串切,是因为它顺带处理了
// "不在 dir 之下"(例如跨盘符)的情况 —— 那种情况直接放弃猜测。
func restoreDriveFromSynthetic(dir, path string) (string, bool) {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return "", false
	}
	if rel == "." || strings.HasPrefix(rel, "..") {
		return "", false
	}
	first, rest, hasRest := strings.Cut(rel, string(filepath.Separator))
	if len(first) != 1 || !isASCIILetter(first[0]) {
		return "", false
	}
	drive := strings.ToUpper(first) + ":"
	// 盘符必须真的存在 —— 否则 `dir\c\foo` 可能只是用户自己的一个叫 c 的目录。
	if st, err := os.Lstat(drive + `\`); err != nil || !st.IsDir() {
		return "", false
	}
	if !hasRest || rest == "" {
		return drive + `\`, true
	}
	return filepath.Join(drive+`\`, rest), true
}

type syntheticAlt struct {
	synthetic string // mvdan/sh 预拼接后会得到的那个路径
	windows   string // 它本该对应的真实路径
}

//	syntheticCandidates 列出"mvdan/sh 会把哪个 MSYS 挂载点拼成什么"。
//
// 只列**挂载点本身**(`/tmp`、`/usr/bin`、`/bin`):它们的首段不是单字母,反推不出来,
// 只能靠这张表。盘符形式(`/c/...`)交给 restoreDriveFromSynthetic —— 那个能精确反推。
//
// `/` 挂载点**刻意不列**:`Join(dir, "/")` 就是 dir 自己,而 `[ -e / ]` 的路径本来就是
// dir(dir 一定存在),原路径检查已经覆盖,列进来是多余的。
//
// 由此带来一条**已知限制**:`[ -e /etc ]`(根挂载点下的子路径)拿不回来 ——
// 它被预拼成 `<dir>\etc`,而用户写 `[ -e etc ]`(相对路径)拼出的是**同一个字符串**。
// 无法区分,于是只能不猜。详见 RestoreSyntheticMount 的注释。
func syntheticCandidates(dir string) []syntheticAlt {
	var out []syntheticAlt
	for _, m := range msysMounts {
		win := m.windows()
		if win == "" {
			continue
		}
		out = append(out, syntheticAlt{
			// Join 会自己处理 mount 的前导斜杠,与 absPath 的做法一致。
			synthetic: filepath.Join(dir, filepath.FromSlash(m.mount)),
			windows:   win,
		})
	}
	return out
}

// msysMount 是一条挂载规则。windows 是取值函数而不是字符串,因为其中几条依赖运行时环境。
type msysMount struct {
	mount   string
	windows func() string
}

var msysMounts = []msysMount{
	{mount: "/usr/bin", windows: func() string { return gitUsrBin() }},
	{mount: "/bin", windows: func() string { return gitUsrBin() }},
	{mount: "/tmp", windows: func() string { return userTempDir() }},
	// `/` 最后:它是"兜底"的那一条,前面几条都匹配不上才会走到这里。
	//
	// 与真 Bash 一致的只有"看到什么":/ 是 Git 安装根,里面有 usr/、etc/、bin/。
	// 但真 bash 还能看到它**派生的**目录与软链接,而 winbash 是直接读那个真实目录,
	// 所以数量不保证逐字一样 —— 这一点写进 README 的已知差异表,不在这里假装一致。
	{mount: "/", windows: func() string { return gitInstallRoot() }},
}

// trimMount 判断 p 是否正好是 mount 或落在 mount 下,返回去掉挂载点后的相对部分。
//
// 边界要严: `/tmpfoo` **不**匹配 `/tmp`;但 `/tmp/foo` 与 `/tmp` 都匹配。
func trimMount(p, mount string) (rest string, ok bool) {
	if mount == "/" {
		// 根挂载点匹配任何以 / 开头的路径,**除了**盘符形式的单段(`/c`、`/d`)——
		// 那些交给 splitPOSIXDrive,不归这里。
		//
		// 早先这里写的是 `p[1] != '/' 就 false`,于是 `/etc`、`/usr`、`/opt` 这些
		// **单段**都被排除了 —— 实测 `ls /etc` 给 0 项(真 bash 给 25),
		// 尽管 `C:\Program Files\Git\etc` 就在那里。
		if p == "/" {
			return "", true
		}
		if len(p) < 2 {
			return "", false
		}
		// 排除**所有**盘符形式:`/c`、`/c/`、`/c/Windows` 都是盘符路径,不归根挂载点。
		//
		// 这里早先写的是 `len(p)==2 || (len(p)==3 && p[2]=='/')` —— 只挡住了前两种,
		// 于是 `/c/Windows` 落进根挂载点,变成 `C:\Program Files\Git\c\Windows`
		// (实测 `cd /c/Windows` 报 "no such file or directory")。
		if isASCIILetter(p[1]) && (len(p) == 2 || p[2] == '/') {
			return "", false
		}
		return p[1:], true
	}
	if !strings.HasPrefix(p, mount) {
		return "", false
	}
	rest = p[len(mount):]
	if rest == "" {
		return "", true
	}
	if rest[0] != '/' {
		return "", false // /tmpfoo 不是 /tmp 下的东西
	}
	return rest[1:], true
}

// gitInstallRoot 返回 Git 安装根(usr\bin 的父目录),找不到则空串。
func gitInstallRoot() string {
	usrBin := gitUsrBin()
	if usrBin == "" {
		return ""
	}
	return filepath.Dir(filepath.Dir(usrBin))
}

// gitUsrBin 返回存在的 Git Bash usr\bin。
//
// 与 shell 包的 gitBashUsrBin 是同一份候选表。两份是因为它们分属 builtins 与 shell
// (shell 不能 import builtins 的反向依赖),而这条规则未来若要改(Git 换了安装位置),
// 两处都要改 —— 已在注释里点明。
func gitUsrBin() string {
	for _, dir := range []string{
		`C:\Program Files\Git\usr\bin`,
		`C:\Program Files (x86)\Git\usr\bin`,
	} {
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			return dir
		}
	}
	return ""
}

// userTempDir 返回当前用户的 TEMP。
//
// 为什么用 %TEMP% 而不是 %TMP%:真 Git Bash 的 /tmp 挂的就是用户的 TEMP 目录
// (实测 mount 输出里的 `usertemp` 标记,以及两边都指向同一个 reasonix-session-tmp-* 目录)。
// 优先 TEMP、退回 TMP,再退回 %LOCALAPPDATA%\Temp。
func userTempDir() string {
	for _, k := range []string{"TEMP", "TMP"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			if st, err := os.Stat(v); err == nil && st.IsDir() {
				return v
			}
		}
	}
	if la := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); la != "" {
		v := filepath.Join(la, "Temp")
		if st, err := os.Stat(v); err == nil && st.IsDir() {
			return v
		}
	}
	return ""
}
