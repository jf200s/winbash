// `ls -l` 的权限位列:复刻 MSYS(Git Bash 用的那套)算法。
//
// 为什么需要单独一个文件:Go 的 `os.FileInfo.Mode()` 在 Windows 上只反映
// 「只读」一个属性,给出来的是 `-rw-rw-rw-` / `drwxrwxrwx`。那既不符合真 bash,
// 也会误导人 —— 看上去像是 group/other 都能写。
//
// 本文件里的每条规则都对着**真 Git Bash 实测**过(2026-10-18),
// 实测值抄在注释里。规则不是照 GNU 手册推的:GNU 的 mode 来自 inode,
// 而 Windows 没有 inode,这里是 MSYS 自己按属性 + 文件头合成出来的东西。

package builtins

import (
	"os"
	"path/filepath"
	"strings"
)

// defaultUmask 是 winbash 假定的 umask。
//
// 为什么是常量而不是读出来:winbash **没有 umask 内建**(实测 `umask` 在 winbash
// 里跑不出结果),而 Windows 本身也没有进程级 umask 概念 —— MSYS 的 umask 是它
// 自己进程内维护的一个变量。真 Git Bash 的默认值就是 022,所以这里按它固定。
//
// 限制已记进 README:在 winbash 里改不了 umask,`umask 077` 那种写法不会生效。
// 真要按 umask 算权限,现在是做不到的 —— 但**不能假装做了**(那是静默错值)。
const defaultUmask = 0o022

// msysMode 生成 `ls -l` 首列的权限串。
//
// 真 Git Bash 实测出来的完整规则(2026-10-18,umask=022):
//
//	              可写            只读(Windows 只读属性)
//	目录          drwxr-xr-x      drwxr-xr-x     ← 只读属性对目录**不生效**
//	文件·可执行   -rwxr-xr-x      -r-xr-xr-x
//	文件·普通     -rw-r--r--      -r--r--r--
//
// 等价说法:基础位是 0777(目录 or 可执行文件)/ 0666(普通文件);Windows 的只读
// 属性把所有 w 位清掉(**目录除外** —— 实测 `chmod 0444` 的目录仍给 drwxr-xr-x);
// 最后按 defaultUmask 去掉被掩掉的位。
//
// 验算(umask 022):
//
//	目录      0777 &^ 022 = 0755 → drwxr-xr-x
//	普通文件  0666 &^ 022 = 0644 → -rw-r--r--
//	只读普通  0444 &^ 022 = 0444 → -r--r--r--
//	可执行    0777 &^ 022 = 0755 → -rwxr-xr-x
//	只读可执行 0555 &^ 022 = 0555 → -r-xr-xr-x
//
// exec 由 looksExecutable 判定 —— 传 false 表示"这个文件不是可执行形态"。
// 目录不看 exec(目录天然带 x)。
func msysMode(info os.FileInfo, exec bool) string {
	var perm os.FileMode
	switch {
	case info.IsDir():
		// 目录:一律 0777,忽略只读属性。实测真 bash 对只读目录也给 drwxr-xr-x,
		// 因为 Windows 目录的"只读"属性不表示不可写(它另有用途)。
		perm = 0o777
	case exec:
		perm = 0o777
		if isWindowsReadonly(info) {
			perm = 0o555
		}
	default:
		perm = 0o666
		if isWindowsReadonly(info) {
			perm = 0o444
		}
	}
	perm &^= os.FileMode(defaultUmask)

	// 用 FileMode.String() 而不是自己拼 rwx 串:它已经正确处理了 r/w/x 三组九位。
	// 只把 ModeDir 带进来,其余特殊位(symlink 等)一律不带 —— 实测真 Git Bash
	// 对符号链接也不显示 `l`(它给的是目标类型),见 README。
	if info.IsDir() {
		return (os.ModeDir | perm).String()
	}
	return perm.String()
}

// isWindowsReadonly 判断文件是否带 Windows 只读属性。
//
// Go 把它映射成「权限位里一个 w 都没有」(`os.Chmod(path, 0o444)` 在 Windows 上
// 就是设这个属性)。所以判据是 `Perm()&0o222 == 0`。
func isWindowsReadonly(info os.FileInfo) bool {
	return info.Mode().Perm()&0o222 == 0
}

// looksExecutable 判定一个文件是否会被 MSYS 认为「可执行」(即在权限串里带 x)。
//
// 实测规则(2026-10-18,造了 28 种扩展名 + 10 种文件头的矩阵,逐条对照真 Git Bash):
//
//	扩展名 .exe                        → 可执行(唯一靠扩展名就够的)
//	文件头 MZ                          → 可执行(**不管**扩展名,连 .txt 也算)
//	文件头 #!                          → 可执行(不管扩展名)
//	其余                               → 不可执行
//
// 几个容易猜错的点,都是实测结果:
//   - `.com` / `.dll` **不是**靠扩展名:内容为普通文本时给 `-rw-r--r--`,
//     内容带 MZ 头时给 `-rwxr-xr-x`。所以它们走的是文件头那条。
//   - `.bat` / `.cmd` / `.ps1` **不可执行**(`-rw-r--r--`)。这与 Cygwin 的传统
//     不同(那边把 .bat 当可执行),MSYS 这一版不认。
//   - `.sh` / `.py` 等脚本扩展名,**光有扩展名不可执行**;要有 `#!` 头才行。
//   - ELF / PNG / ZIP 头**都不可执行**。
//
// 为什么要读文件头而不是只看扩展名:上游 MSYS 就是这么做的,而这正是
// 「`ls -l` 能不能用来判可执行」的关键 —— 只看扩展名会让 PATH 里那 44 个
// 无扩展名的 shebang 脚本全显示成不可执行。
func looksExecutable(path string, isDir bool, head []byte) bool {
	if isDir {
		// 目录天然带 x,不需要读头;调用方也不会为目录读文件。
		return true
	}
	if strings.EqualFold(filepath.Ext(path), ".exe") {
		return true
	}
	if len(head) >= 2 {
		if head[0] == 'M' && head[1] == 'Z' {
			return true
		}
		if head[0] == '#' && head[1] == '!' {
			return true
		}
	}
	return false
}
