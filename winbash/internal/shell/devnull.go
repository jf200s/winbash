package shell

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// `/dev/null` 的 **stat 语义**(仅 stat;open 那条路归 OpenHandler 的 NUL 特例管)。
//
// 背景(实测):`[ -e /dev/null ]` 走 mvdan/sh 的 `r.stat` → `absPath(r.Dir, name)`,
// 而 `absPath` 在 Windows 上因 `filepath.IsAbs("/dev/null") == false` 会把路径
// **拼到 cwd 后面**,handler 收到的是 `<dir>\dev\null` —— 原始写法在那一步就丢了。
// 于是 `os.Stat` 落在一个不存在的路径上,`-e`/`-r`/`-w`/`-c` 全为假,
// 与真 bash(`/dev/null` 是字符设备,存在、可读、可写)不一致。
//
// 修法与 `RestoreSyntheticMount` 同构,两条硬要求:
//
//  1. **事实优先**:真的存在 `<dir>\dev\null`(用户建了 `dev/null` 文件/目录)就
//     原样采信,不猜 —— 与 `msyspath.go` 里「原路径存在就原样返回」的约定一致。
//     本函数只在调用方确认 `os.Lstat(path)` **失败**之后才被问到。
//  2. **手工构造 FileInfo**,不能用 `os.Stat("NUL")` 的结果:Windows 的 NUL
//     经 os.Stat 报的是 **regular 文件**(0o666),而真 bash 的 `/dev/null` 是
//     **字符设备** —— 直接转发会让 `[ -f /dev/null ]` 成立、`[ -c /dev/null ]`
//     不成立,恰好全反(见 devNullInfo 的注释)。
//
// 无法反推的部分照旧不猜:`/dev/null` 与相对路径 `dev/null` 拼出**同一个字符串**,
// 只能靠上面第 1 条的事实优先兜住「用户真建了」那一侧。
func devNullSynthetic(dir, path string) (fs.FileInfo, bool) {
	if !isDevNullShaped(dir, path) {
		return nil, false
	}
	return devNullInfo{}, true
}

// isDevNullShaped 判断 path 是不是 `/dev/null` 或它被 absPath 拼接后的形态。
//
// 三种形态都认:原始 `/dev/null`、拼接产物 `<dir>\dev\null`、以及 cwd 在盘根时
// 的 `C:\dev\null`(已被 Join 覆盖)与裸 `\dev\null`。大小写不敏感 —— 与
// `RestoreSyntheticMount` 的 `strings.EqualFold` 口径一致(Windows 路径不区分大小写)。
func isDevNullShaped(dir, path string) bool {
	if path == "/dev/null" {
		return true
	}
	clean := filepath.Clean(path)
	if strings.EqualFold(clean, filepath.Join(dir, "dev", "null")) {
		return true
	}
	return strings.EqualFold(clean, filepath.FromSlash("/dev/null"))
}

// devNullInfo 是 `/dev/null` 的**手工构造** FileInfo。
//
// Mode 必须是 `os.ModeDevice | os.ModeCharDevice | 0o666` —— 这一位之差就是
// 整条修复的命门,拿 `os.Stat("NUL")` 的结果(regular 文件)会让判据**刚好全反**:
//
//	ModeDevice 位在      → IsRegular() false → `[ -f ]` 为假 ✓(真 bash 同)
//	ModeCharDevice 位在  → `[ -c ]` 为真 ✓、`[ -b ]` 为假(块设备要求 CharDevice 位不在)✓
//	ModeDir 位不在       → `[ -d ]` 为假 ✓
//	0o666                → `-r`/`-w` 通过、`-x` 失败(defaultAccess 按 mode 位判)✓
//	Size() == 0          → `[ -s ]` 为假 ✓
//	ModeSymlink 位不在   → `[ -h ]`/`[ -L ]` 为假 ✓
type devNullInfo struct{}

func (devNullInfo) Name() string { return "null" }

// Size 给 0:空设备没有内容,`[ -s ]` 因此为假(与真 bash 一致)。
func (devNullInfo) Size() int64 { return 0 }

func (devNullInfo) Mode() os.FileMode {
	return os.ModeDevice | os.ModeCharDevice | 0o666
}

// ModTime 给零值:空设备没有有意义的 mtime;`[ -N ]`/`[ -nt ]` 这类判据因此
// 与真 bash 的表现不同,但那是次要差异,不值得造一个假时间。
func (devNullInfo) ModTime() time.Time { return time.Time{} }

func (devNullInfo) IsDir() bool { return false }

// Sys 给 nil:合成对象没有底层系统信息;mvdan/sh 的 test 只碰 Mode/Size/Name,
// nil 是安全值(接口要求的方法,不能省)。
func (devNullInfo) Sys() any { return nil }
