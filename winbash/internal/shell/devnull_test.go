package shell

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// `test` 的各判据对 `/dev/null` 的判定 —— 与真 Git Bash 逐条对齐。
//
// 修前 winbash 全线为假(-e/-r/-w/-c 都错):mvdan/sh 的 `r.stat` 先把路径拼成
// `<dir>\dev\null` 才交给 StatHandler,原始写法在那一步就丢了,`os.Stat` 落在
// 一个不存在的路径上。修法见 internal/shell/devnull.go 的长注释。
//
// ★ **-f 与 -c 是最容易写反的一对**,必须逐字钉:
// 返回的 FileInfo 若用 `os.Stat("NUL")` 的结果(Windows 报 regular 文件),
// `-f` 会变 0、`-c` 会变 1 —— 恰好全反。只有手工构造的
// `os.ModeDevice|os.ModeCharDevice|0o666` 才能同时给出 -f=1、-c=0。
func TestTestJudgesOnDevNull(t *testing.T) {
	dir := t.TempDir() // 干净目录:确保 cwd 下没有真的 dev/null

	cases := []struct {
		op   string
		want int // 真 bash 的退出码
	}{
		{"-e", 0}, // 存在
		{"-d", 1}, // 不是目录
		{"-f", 1}, // ★ 不是常规文件(它是字符设备)
		{"-r", 0}, // 可读
		{"-w", 0}, // 可写
		{"-x", 1}, // 不可执行(0o666 无执行位)
		{"-s", 1}, // 空(Size()==0)
		{"-b", 1}, // 不是块设备
		{"-c", 0}, // ★ 是字符设备
		{"-p", 1}, // 不是管道
		{"-S", 1}, // 不是 socket
		{"-h", 1}, // 不是符号链接
		{"-L", 1}, // 同 -h
	}
	for _, c := range cases {
		t.Run("test "+c.op+" /dev/null", func(t *testing.T) {
			out, errText, exit := runIn(t, dir,
				`test `+c.op+` /dev/null; echo "rc=$?"`)
			if !strings.Contains(out, "rc="+strconv.Itoa(c.want)) {
				t.Errorf("test %s /dev/null: 期望 rc=%d,输出 %q(errs=%q, exit=%d)",
					c.op, c.want, out, errText, exit)
			}
		})
	}
}

// 反向回归:普通路径与真失败**不许**被 /dev/null 特判吞掉。
//
// 特判只认 `/dev/null` 及其被 absPath 拼出来的形态;真不存在的路径、真文件、
// 真目录、以及 cwd 里叫 `null` 的文件都必须走原来的 stat 路径。
func TestDevNullFixDoesNotSwallowRealPaths(t *testing.T) {
	dir := dirFixture(t) // 含 adir/ 与 plain.txt
	if err := os.WriteFile(filepath.Join(dir, "null"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		script string
		want   int
	}{
		{"cat /dev/null 仍 rc=0", `cat /dev/null; echo "rc=$?"`, 0},
		{"cat < /dev/null 仍 rc=0", `cat < /dev/null; echo "rc=$?"`, 0},
		{"真不存在的路径仍 rc=1", `test -e /no/such/path-probe-xyz; echo "rc=$?"`, 1},
		{"相对的真不存在仍 rc=1", `test -e no-such-entry-xyz; echo "rc=$?"`, 1},
		{"test -d /tmp 不受影响", `test -d /tmp; echo "rc=$?"`, 0},
		{"真文件 test -f 不受影响", `test -f plain.txt; echo "rc=$?"`, 0},
		{"真目录 test -d 不受影响", `test -d adir; echo "rc=$?"`, 0},
		// 只有一段的 `null` 不是 `/dev/null` 的形态 —— 不能被特判误伤。
		{"cwd 里的 null 文件不受影响", `test -f null; echo "rc=$?"`, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, errText, exit := runIn(t, dir, c.script)
			if !strings.Contains(out, "rc="+strconv.Itoa(c.want)) {
				t.Errorf("%s: 期望 rc=%d,输出 %q(errs=%q, exit=%d)",
					c.name, c.want, out, errText, exit)
			}
		})
	}
}

// ★ 事实优先:用户真的在 cwd 建了 `dev/null`,就**采信事实**,不当 NUL。
//
// 这与 RestoreSyntheticMount 的既有约定一致(msyspath.go:「原路径存在就原样
// 采信,不做任何猜测」)。注意 `/dev/null` 与相对路径 `dev/null` 被 absPath 拼成
// **同一个字符串**,无法反推 —— 所以这一侧只能靠事实优先兜住,README 里记着。
func TestDevNullFactWinsOverSynthetic(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "dev", "null"), 0o755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		script string
		want   int
	}{
		{"dev/null 存在(目录)", `test -e dev/null; echo "rc=$?"`, 0},
		{"dev/null 是目录", `test -d dev/null; echo "rc=$?"`, 0},
		{"dev/null 不是常规文件", `test -f dev/null; echo "rc=$?"`, 1},
		{"dev/null 不是字符设备", `test -c dev/null; echo "rc=$?"`, 1},
		// 同一目录下,真失败仍然失败 —— 特判只在采信事实时让位。
		{"真不存在仍 rc=1", `test -e no-such-entry-xyz; echo "rc=$?"`, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, errText, exit := runIn(t, dir, c.script)
			if !strings.Contains(out, "rc="+strconv.Itoa(c.want)) {
				t.Errorf("%s: 期望 rc=%d,输出 %q(errs=%q, exit=%d)",
					c.name, c.want, out, errText, exit)
			}
		})
	}
}

// 合成对象的 Mode 位直接钉死 —— 端到端那组红时,这一条能把「哪一位错了」
// 缩到最小。变异验证(把 Mode 改成 0o666)会让它与 -f/-c 两条一起变红。
func TestDevNullInfoModeIsCharDevice(t *testing.T) {
	info, ok := devNullSynthetic(t.TempDir(), "/dev/null")
	if !ok {
		t.Fatal("devNullSynthetic 未认出 /dev/null")
	}
	m := info.Mode()
	if m&os.ModeDevice == 0 || m&os.ModeCharDevice == 0 {
		t.Errorf("Mode 必须含 ModeDevice|ModeCharDevice(字符设备),得到 %v", m)
	}
	if m.IsRegular() {
		t.Errorf("IsRegular() 必须为 false,否则 [ -f /dev/null ] 会为真;mode=%v", m)
	}
	if m&os.ModeDir != 0 {
		t.Errorf("Mode 不该含 ModeDir,得到 %v", m)
	}
	if m&os.ModeSymlink != 0 {
		t.Errorf("Mode 不该含 ModeSymlink,得到 %v", m)
	}
	if got := m.Perm(); got != 0o666 {
		t.Errorf("权限位期望 0o666(-r/-w 通过、-x 失败),得到 %#o", got)
	}
	if info.Size() != 0 {
		t.Errorf("Size 期望 0([ -s ] 为假),得到 %d", info.Size())
	}
	if info.Name() != "null" {
		t.Errorf("Name 期望 \"null\",得到 %q", info.Name())
	}
	// 不能 panic(接口方法逐个调一遍)。
	_ = info.ModTime()
	_ = info.IsDir()
	_ = info.Sys()
}
