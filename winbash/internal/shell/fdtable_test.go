// fd 表(fd 3+)的行为钉子。真 bash 实测基准:
//
//	exec 3>&1; echo x >&3        rc=0  输出 x
//	echo x 3>&1; echo y          rc=0  输出 x、y(fd 3 只活在那条命令内)
//	echo x 3>&1; echo y >&3      rc=1  y 不输出,stderr: 3: Bad file descriptor
//	exec 4>f; echo a >&4; echo b >&4     f 内容 a\nb\n(同一文件,偏移累积)
//	(exec 3>f; echo x >&3); echo y >&3   rc=1  后者 3: Bad file descriptor(子 shell 不穿透)
//	echo foo 5>a                 rc=0  输出 foo、静默创建 a
//	exec 3>&1                    之后跨命令仍可用
//
// 修的是 mvdan/sh 上游的**功能缺失**:fd 3+ 一律 `unsupported redirect fd: N`。
// 实现与剩余边界见 third_party/mvdan-sh/WINBASH-PATCH.md §5。
//
// 本文件同时钉住两个不变量:
//   - 命令级 fd 不泄漏(快照-恢复对);exec 建立的 fd 跨命令持久(keepRedirs 豁免);
//   - 子 shell / 管道段的 fd 表是 clone 而非共享 —— TestFdTableSubshellIsolation
//     就是那条改动的变异探针:把 clone 换成共享 map,它必须变红。
package shell

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ★ fd 表核心:上面那张真 bash 基准表的逐条对齐。
func TestFdTableCore(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name     string
		script   string
		wantOut  string
		wantErr  string // stderr 必须含有此子串
		wantExit int
	}{
		{
			name:     "exec 3>&1 后 echo x >&3 输出 x",
			script:   `exec 3>&1; echo x >&3`,
			wantOut:  "x\n",
			wantExit: 0,
		},
		{
			name:     "命令级 fd 不泄漏到后续命令",
			script:   `echo x 3>&1; echo y >&3`,
			wantOut:  "x\n",
			wantErr:  "3: Bad file descriptor",
			wantExit: 1,
		},
		{
			name:     "命令级 fd 不影响后续正常输出",
			script:   `echo x 3>&1; echo y`,
			wantOut:  "x\ny\n",
			wantExit: 0,
		},
		{
			name:     "echo foo 5>a 静默成功",
			script:   `echo foo 5>a`,
			wantOut:  "foo\n",
			wantExit: 0,
		},
		{
			name:     "exec 3>&1 跨命令仍可用",
			script:   `exec 3>&1; echo a >&3; echo b >&3`,
			wantOut:  "a\nb\n",
			wantExit: 0,
		},
		{
			name:     "subshell 的 fd 不穿透到父 shell",
			script:   `(exec 3>f; echo x >&3); echo y >&3`,
			wantOut:  "",
			wantErr:  "3: Bad file descriptor",
			wantExit: 1,
		},
		{
			name:     "管道右段的 exec 不泄漏进父 shell",
			script:   `exec 3>x | true; echo y >&3`,
			wantOut:  "",
			wantErr:  "3: Bad file descriptor",
			wantExit: 1,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, errText, exit := runIn(t, dir, c.script)
			if exit != c.wantExit {
				t.Errorf("退出码 %d,期望 %d(stderr=%q)", exit, c.wantExit, errText)
			}
			if out != c.wantOut {
				t.Errorf("stdout 期望逐字 %q,得到 %q", c.wantOut, out)
			}
			if c.wantErr != "" && !strings.Contains(errText, c.wantErr) {
				t.Errorf("stderr 应含 %q,得到 %q", c.wantErr, errText)
			}
			if c.wantErr == "" && strings.TrimSpace(errText) != "" {
				t.Errorf("stderr 应为空,得到 %q", errText)
			}
		})
	}
}

// ★ `exec 4>f` 的持久 fd:多条命令写同一个句柄,偏移累积(不是每次重开)。
// f 的内容必须是 a\nb\n —— 若每条命令各开各写,第二条会把第一条截掉。
func TestFdTableFileOffsetAccumulates(t *testing.T) {
	dir := t.TempDir()
	out, errText, exit := runIn(t, dir, `exec 4>f; echo a >&4; echo b >&4`)
	if exit != 0 {
		t.Fatalf("退出码 %d,期望 0(stderr=%q)", exit, errText)
	}
	if out != "" {
		t.Errorf("stdout 应为空(f 是输出目标),得到 %q", out)
	}
	got, err := os.ReadFile(filepath.Join(dir, "f"))
	if err != nil {
		t.Fatalf("读 f:%v", err)
	}
	if string(got) != "a\nb\n" {
		t.Errorf("f 内容期望 %q,得到 %q", "a\nb\n", got)
	}
}

// ★ exec 建立的 fd 跨**命令**也跨 **Run** 持久(winbash 会话里 fd 表不属于
// 单次 Run)。同一 Session 第二次 Run 仍能写 fd 3,且输出落在当次 stdout。
func TestFdTablePersistsAcrossRuns(t *testing.T) {
	dir := t.TempDir()
	s := NewSession()
	defer s.Close()
	if code := s.runForSetup(t, `cd "`+dir+`"`); code != 0 {
		t.Fatalf("cd 失败:%d", code)
	}
	if _, exit, errs := s.Run(context.Background(), `exec 3>&1`, false); exit != 0 {
		t.Fatalf("exec 3>&1 退出码 %d,期望 0(errs=%v)", exit, errs)
	}
	out, exit, errs := s.Run(context.Background(), `echo x >&3`, false)
	if exit != 0 {
		t.Errorf("echo x >&3 退出码 %d,期望 0(errs=%v)", exit, errs)
	}
	if out != "x\n" {
		t.Errorf("stdout 期望 %q,得到 %q", "x\n", out)
	}
}

// ★ 变异探针:子 shell 的 fd 表必须 **clone**。
// 把 subshell 的 cloneFdTable 换成共享 map,这条必须变红:
// `(exec 3>f)` 里的 fd 3 会泄漏进父 shell,`echo y >&3` 不再报错。
//
// **只这一条抓不到 clone 缺失**(实测踩过):`( ... )` 退出时会
// `r2.closeAllFds()`,共享 map 时它把**父的 fd 也一并清空**,于是父 shell 照样
// 报 Bad file descriptor、这条照样绿 —— 掩盖了「没 clone」这个事实。
// 真正能区分的是下面的「父先有 fd、子 shell 里动它」:共享时父的 fd 会被
// 子 shell 顺手关掉/改掉,clone 时不受影响。
func TestFdTableSubshellIsolation(t *testing.T) {
	dir := t.TempDir()
	out, errText, exit := runIn(t, dir, `(exec 3>f; echo x >&3); echo y >&3; echo done`)
	// 真 bash:subshell 写 f,父 shell 的 >&3 报 Bad file descriptor,done 照跑。
	if exit != 0 {
		t.Errorf("退出码 %d,期望 0(最后一条 echo done 成功)(stderr=%q)", exit, errText)
	}
	if out != "done\n" {
		t.Errorf("stdout 期望 %q,得到 %q", "done\n", out)
	}
	if !strings.Contains(errText, "3: Bad file descriptor") {
		t.Errorf("stderr 应含 %q,得到 %q", "3: Bad file descriptor", errText)
	}
	got, err := os.ReadFile(filepath.Join(dir, "f"))
	if err != nil {
		t.Fatalf("读 f:%v", err)
	}
	if string(got) != "x\n" {
		t.Errorf("subshell 应把 x 写进 f,得到 %q", got)
	}
}

// ★ 父 shell 的 fd **不许**被子 shell 动到 —— 这条才真正钉住 clone。
//
// 父先 `exec 5>f` 建立 fd 5,子 shell 里写它。真 bash(= fork 语义,clone fd 表):
// 子 shell 有自己的表,写入落到共享的底层文件,父的 fd 5 事后照样能用。
// 若 clone 缺失(共享 map):子 shell 退出时 closeAllFds 会把父的 fd 5 也关掉,
// 下一句 `echo parent >&5` 就报 `5: Bad file descriptor`。
func TestFdTableSubshellDoesNotStealParentsFd(t *testing.T) {
	dir := t.TempDir()
	script := `exec 5>f; (echo sub >&5); echo parent >&5; echo done`
	out, errText, exit := runIn(t, dir, script)
	if exit != 0 {
		t.Errorf("退出码 %d,期望 0(stderr=%q)", exit, errText)
	}
	if strings.Contains(errText, "Bad file descriptor") {
		t.Errorf("父的 fd 5 不该被子 shell 顺手关掉,stderr=%q", errText)
	}
	if out != "done\n" {
		t.Errorf("stdout 期望 %q,得到 %q", "done\n", out)
	}
	got, err := os.ReadFile(filepath.Join(dir, "f"))
	if err != nil {
		t.Fatalf("读 f:%v", err)
	}
	// 子 shell 的写与父的写都落在同一个底层文件(共享句柄,共享偏移)。
	if string(got) != "sub\nparent\n" {
		t.Errorf("f 内容期望 %q,得到 %q", "sub\nparent\n", got)
	}
}

// ★ 管道左右段并发跑、且共享底层文件(clone 时 retain 资源)。
// `go test -race` 下必须干净;f 的内容也验证了共享句柄的偏移语义。
func TestFdTableConcurrentPipelineNoRace(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 20; i++ {
		out, errText, exit := runIn(t, dir, `exec 4>f; echo a >&4 | cat; echo b >&4`)
		if exit != 0 {
			t.Fatalf("第 %d 轮退出码 %d,期望 0(stderr=%q)", i, exit, errText)
		}
		if out != "" {
			// 真 bash 实测:`echo a >&4 | cat` 里重定向**优先于管道** ——
			// stdout 被 `>&4` 接走,a 全写进 f,cat 读不到东西。
			// (我第一版把期望写成 "a\n",那是把管道当成了优先级更高的一方。)
			t.Fatalf("第 %d 轮 stdout 期望空(>&4 优先于 |),得到 %q", i, out)
		}
		got, err := os.ReadFile(filepath.Join(dir, "f"))
		if err != nil {
			t.Fatalf("读 f:%v", err)
		}
		if string(got) != "a\nb\n" {
			t.Fatalf("第 %d 轮 f 内容期望 %q,得到 %q", i, "a\nb\n", got)
		}
	}
}

// ★ `exec 3<&0` 之后 stdin 被 winbash 的 closeStdin 换掉(每次 Run 收尾):
// fd 3 必须**响亮**失效(`3: Bad file descriptor`),不许变成 use-after-close
// (`file already closed`)—— 后者是拿着已 Close 的 *os.File 继续读。
func TestFdTableStdinDupDoesNotUseAfterClose(t *testing.T) {
	dir := t.TempDir()
	s := NewSession()
	defer s.Close()
	if code := s.runForSetup(t, `cd "`+dir+`"`); code != 0 {
		t.Fatalf("cd 失败:%d", code)
	}
	s.SetStdinBytes([]byte("hello\n"))
	out, exit, errs := s.Run(context.Background(), `exec 3<&0; cat <&3`, false)
	if exit != 0 {
		t.Fatalf("第一次 Run 退出码 %d,期望 0(errs=%v)", exit, errs)
	}
	if out != "hello\n" {
		t.Fatalf("第一次 Run stdout 期望 %q,得到 %q", "hello\n", out)
	}
	s.SetStdinBytes([]byte("world\n"))
	out, exit, errs = s.Run(context.Background(), `cat <&3`, false)
	errText := strings.Join(errs, "\n")
	if exit == 0 {
		t.Errorf("第二次 Run 退出码 0,期望非 0(fd 3 应已失效)")
	}
	if strings.Contains(errText, "file already closed") {
		t.Errorf("use-after-close:不得读已关闭的 stdin,得到 %q", errText)
	}
	if !strings.Contains(errText, "3: Bad file descriptor") {
		t.Errorf("stderr 应含 %q,得到 %q", "3: Bad file descriptor", errText)
	}
	if out != "" {
		t.Errorf("第二次 Run stdout 应为空,得到 %q", out)
	}
}

// ★ 外部命令拿不到 fd 3+(cmd.ExtraFiles 在 Windows 不支持):
// `cmd 3>log` 不能静默丢重定向(命令"成功"、文件没写 = 无声数据丢失),
// 要响亮报错。而 `exec 4>f; cmd`(命令自身不带 fd 3+ 重定向)不拦。
func TestFdTableExternalCommandLoudlyRejectsExtraFd(t *testing.T) {
	dir := t.TempDir()
	_, errText, exit := runIn(t, dir, `sed s/a/b/ 3>log`)
	if exit == 0 {
		t.Errorf("退出码 0,期望非 0:fd 3 传不进子进程,必须响亮失败")
	}
	if !strings.Contains(errText, "fd 3 cannot be passed to external commands") {
		t.Errorf("stderr 应含 %q,得到 %q", "fd 3 cannot be passed to external commands", errText)
	}

	// 反例:命令自身不带 fd 3+ 重定向时,遗留的持久 fd 不许误伤。
	out, errText, exit := runIn(t, dir, `exec 4>f; echo ok`)
	if exit != 0 {
		t.Errorf("exec 4>f; echo ok 退出码 %d,期望 0(stderr=%q)", exit, errText)
	}
	if out != "ok\n" {
		t.Errorf("stdout 期望 %q,得到 %q", "ok\n", out)
	}
}

// ★ 反向回归:fd 表没碰过的路径一字不差。改动落在 redir 的**每一次**重定向
// 都会过的路上,正常 echo/cat/管道/$(...)/子 shell/2>&1 坏了就是灾难。
func TestFdTableNoRegressionOnPlainIO(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "plain.txt"), []byte("a\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		script  string
		wantOut string
	}{
		{"echo", `echo hi`, "hi\n"},
		{"cat 文件", `cat plain.txt`, "a\nb\n"},
		{"管道", `echo hi | sed s/hi/ho/`, "ho\n"},
		{"命令替换", `echo "$(echo in)"`, "in\n"},
		{"子 shell", `(echo sub); echo end`, "sub\nend\n"},
		{"重定向到文件再读回", `echo z > o.txt; cat o.txt`, "z\n"},
		{"追加", `echo 1 >> ap.txt; echo 2 >> ap.txt; cat ap.txt`, "1\n2\n"},
		{"2>&1 合流", `echo e 2>&1`, "e\n"},
		{"here-document", `cat <<EOF
hd
EOF`, "hd\n"},
		{"<<< 字符串输入", `cat <<< s`, "s\n"},
		{"fd 0 输入重定向", `cat < plain.txt`, "a\nb\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, errText, exit := runIn(t, dir, c.script)
			if exit != 0 {
				t.Errorf("退出码 %d,期望 0(stderr=%q)", exit, errText)
			}
			if out != c.wantOut {
				t.Errorf("stdout 期望逐字 %q,得到 %q", c.wantOut, out)
			}
		})
	}
}
