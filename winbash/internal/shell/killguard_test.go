package shell

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// kill 内建接管的回归测试。
//
// 背景见 killguard.go 顶上的注释:mvdan/sh 列了 kill 这个内建名却没有实现,
// 恒 failf(2, "unsupported builtin");callGuard 在 dispatch 之前把它接管成
// taskkill 翻译。这里分四层钉:
//
//  1. 参数解析(parseKillArgs)—— 计划是什么、哪些写法响亮拒绝;
//  2. 翻译(runKillPids)—— 钉死吐给 taskkill 的命令行,含 /F 的有无与多 pid 展开;
//  3. 走整条 Run 链路的 -l 对照表与各类拒绝;
//  4. **失败不终止脚本**(false 改写)—— callHandler 的 error 在 mvdan 里是 fatal,
//     真 bash 的 kill 失败却只是 rc=1、脚本继续,`kill $PID || echo gone` 必须能工作。
//
// 4. 里对不存在 PID 的两条用例会真的跑一次 taskkill:PID 999999 在 Windows 上
// 永不存在(pids 恒为 4 的倍数,999999%4==3),所以无副作用、不碰任何真进程;
// 真正终止进程的验证在 cmd/winbash/e2e_test.go(只杀测试自己拉起的进程)。

func TestParseKillArgsPlans(t *testing.T) {
	cases := []struct {
		name  string
		args  []string
		want  killPlan
		errIn string // 非空 = 期望报错,且错误文案含这个子串
	}{
		{
			name: "单个 pid 优雅",
			args: []string{"123"},
			want: killPlan{pids: []string{"123"}},
		},
		{
			name: "多 pid 优雅",
			args: []string{"123", "456"},
			want: killPlan{pids: []string{"123", "456"}},
		},
		{
			name: "-9 强杀",
			args: []string{"-9", "123"},
			want: killPlan{force: true, pids: []string{"123"}},
		},
		{
			name: "-KILL 强杀",
			args: []string{"-KILL", "123"},
			want: killPlan{force: true, pids: []string{"123"}},
		},
		{
			name: "-kill 小写也认(bash 信号名不分大小写)",
			args: []string{"-kill", "123"},
			want: killPlan{force: true, pids: []string{"123"}},
		},
		{
			name: "-SIGKILL 强杀",
			args: []string{"-SIGKILL", "123"},
			want: killPlan{force: true, pids: []string{"123"}},
		},
		{
			name: "旗标在 pid 之后也收(不因位置变义)",
			args: []string{"123", "-9"},
			want: killPlan{force: true, pids: []string{"123"}},
		},
		{
			name: "-- 之后的 -9 不再是旗标,按 pid 解析被拒",
			args: []string{"--", "-9"},
			errIn: "不是 Windows PID",
		},
		{
			name:  "作业号响亮拒绝",
			args:  []string{"%1"},
			errIn: "作业",
		},
		{
			name:  "-s 拒绝",
			args:  []string{"-s", "TERM", "123"},
			errIn: "-s",
		},
		{
			name:  "-TERM 拒绝",
			args:  []string{"-TERM", "123"},
			errIn: "-TERM",
		},
		{
			name:  "-n 拒绝",
			args:  []string{"-n", "9", "123"},
			errIn: "-n",
		},
		{
			name:  "非数字 pid 拒绝",
			args:  []string{"abc"},
			errIn: "不是 Windows PID",
		},
		{
			name:  "无参数 = 缺 PID",
			args:  nil,
			errIn: "缺少进程 ID",
		},
		{
			name:  "只有 -9 没有 pid",
			args:  []string{"-9"},
			errIn: "缺少进程 ID",
		},
		{
			name: "-l 全表",
			args: []string{"-l"},
			want: killPlan{list: true},
		},
		{
			name: "-l 编号",
			args: []string{"-l", "9"},
			want: killPlan{list: true, specs: []string{"9"}},
		},
		{
			name: "-l 名字",
			args: []string{"-l", "TERM"},
			want: killPlan{list: true, specs: []string{"TERM"}},
		},
		{
			name:  "-9 与 -l 同用拒绝",
			args:  []string{"-9", "-l"},
			errIn: "同时使用",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plan, err := parseKillArgs(c.args)
			if c.errIn != "" {
				if err == nil {
					t.Fatalf("parseKillArgs(%q) 没有报错,plan=%+v", c.args, plan)
				}
				if !strings.Contains(err.Error(), c.errIn) {
					t.Errorf("错误文案缺 %q: %q", c.errIn, err.Error())
				}
				// 三行式是全项目的错误形状,缺「怎么办」等于把人留在原地。
				if !strings.Contains(err.Error(), "怎么办") {
					t.Errorf("错误文案缺「怎么办」: %q", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("parseKillArgs(%q): %v", c.args, err)
			}
			if !reflect.DeepEqual(plan, c.want) {
				t.Errorf("plan=%+v want %+v", plan, c.want)
			}
		})
	}
}

// 翻译层:钉死吐给 taskkill 的命令行 —— 这是本接管唯一会产生的副作用,
// 多一个 /F 少一个 /PID 都是要么杀错力度、要么静默无效。
func TestKillPidsTranslateToTaskkill(t *testing.T) {
	var got [][]string
	failed := runKillPids([]string{"123", "456"}, false, func(argv []string) error {
		got = append(got, argv)
		return nil
	})
	if len(failed) != 0 {
		t.Errorf("全成功却报失败: %v", failed)
	}
	want := [][]string{{"taskkill", "/PID", "123"}, {"taskkill", "/PID", "456"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("argv=%v want %v", got, want)
	}

	got = nil
	failed = runKillPids([]string{"9"}, true, func(argv []string) error {
		got = append(got, argv)
		return nil
	})
	if len(failed) != 0 {
		t.Errorf("全成功却报失败: %v", failed)
	}
	if want := [][]string{{"taskkill", "/PID", "9", "/F"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("强杀 argv=%v want %v", got, want)
	}
}

// 多 pid 逐个处理、不因前一个失败而停(对齐 bash:报出所有失败者),且失败概要
// 点名是哪个 pid —— 否则使用者面对一串数字不知道哪个没死。
func TestKillPidsCollectsAllFailures(t *testing.T) {
	failed := runKillPids([]string{"111", "222", "333"}, true, func(argv []string) error {
		if strings.HasSuffix(argv[2], "222") || strings.HasSuffix(argv[2], "333") {
			return errors.New("boom")
		}
		return nil
	})
	if len(failed) != 2 {
		t.Fatalf("failed=%v want 2 条(222 与 333)", failed)
	}
	if !strings.Contains(failed[0], "222") || !strings.Contains(failed[1], "333") {
		t.Errorf("失败概要没点名 pid: %v", failed)
	}
	// 非 ExitError 的失败也要有可读概要,不能只给空括号。
	if !strings.Contains(failed[0], "taskkill boom") {
		t.Errorf("失败概要缺原因: %v", failed)
	}
}

// 走整条 Run 链路:-l 对照表与双向查询。
func TestKillListThroughSession(t *testing.T) {
	s := NewSession()
	defer s.Close()

	out, exit, errs := s.Run(context.Background(), "kill -l", false)
	if exit != 0 || len(errs) != 0 {
		t.Fatalf("kill -l 失败 exit=%d errs=%v", exit, errs)
	}
	// 表必须注明 Windows 无信号(这是对照表,不是真信号),且首尾条目在。
	if !strings.Contains(out, "Windows 没有信号") {
		t.Errorf("kill -l 输出缺「Windows 没有信号」注明: %q", out)
	}
	if !strings.Contains(out, "9 KILL") || !strings.Contains(out, "15 TERM") {
		t.Errorf("kill -l 输出缺条目: %q", out)
	}

	cases := []struct {
		script string
		want   string
	}{
		{"kill -l 9", "KILL\n"},
		{"kill -l 15", "TERM\n"},
		{"kill -l KILL", "9\n"},
		{"kill -l sigkill", "9\n"}, // 名字不分大小写、SIG 前缀可有可无
	}
	for _, c := range cases {
		out, exit, errs := s.Run(context.Background(), c.script, false)
		if exit != 0 || len(errs) != 0 {
			t.Errorf("%s 失败 exit=%d errs=%v", c.script, exit, errs)
		}
		if out != c.want {
			t.Errorf("%s out=%q want %q", c.script, out, c.want)
		}
	}

	// 未知的编号/名字:响亮拒绝,不静默输出空行。
	out, exit, errs = s.Run(context.Background(), "kill -l 999", false)
	if exit == 0 {
		t.Errorf("kill -l 999 竟然成功(out=%q)", out)
	}
	if !strings.Contains(strings.Join(errs, " / "), "999") {
		t.Errorf("错误没点名 999: %v", errs)
	}
}

// 走整条 Run 链路的拒绝类:全部必须非零 + 三行式。
func TestKillRejectionsThroughSession(t *testing.T) {
	cases := []struct {
		name   string
		script string
		errIn  string
	}{
		{"作业号", "kill %1", "ps"},
		{"-s", "kill -s TERM 123", "-9"},
		{"-TERM", "kill -TERM 123", "不支持旗标"},
		{"非数字", "kill abc", "不是 Windows PID"},
		{"无参数", "kill", "缺少进程 ID"},
		{"只有旗标", "kill -9", "缺少进程 ID"},
		{"command kill 前缀同样拦", "command kill %1", "ps"},
		{"builtin kill 前缀同样拦", "builtin kill -s TERM 123", "-9"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := NewSession()
			defer s.Close()
			out, exit, errs := s.Run(context.Background(), c.script, false)
			if exit == 0 {
				t.Fatalf("%s 竟然成功(out=%q)", c.script, out)
			}
			errText := strings.Join(errs, " / ")
			if !strings.Contains(errText, c.errIn) {
				t.Errorf("错误文案缺 %q: %q", c.errIn, errText)
			}
			if !strings.Contains(errText, "怎么办") {
				t.Errorf("错误文案缺「怎么办」: %q", errText)
			}
		})
	}
}

// 失败不终止脚本:false 改写 —— 这是与 printf/trap 守卫**有意不同**的控制流,
// 必须钉住:真 bash 的 kill 失败只是 rc=1,`||` 兜底、`;` 续跑都要照常工作。
func TestKillFailureDoesNotAbortScript(t *testing.T) {
	// PID 999999 在 Windows 上永不存在(pid 恒为 4 的倍数),taskkill 无副作用。
	s := NewSession()
	defer s.Close()

	// 优雅失败:非零路径($? 为 1)、stderr 三行式,但脚本报的是**最后一条** echo 的
	// 退出码 0 —— kill 失败终止了脚本的话,这里连输出都拿不到。
	out, exit, errs := s.Run(context.Background(), "kill 999999; echo continued", false)
	if out != "continued\n" {
		t.Errorf("kill 失败终止了脚本: out=%q errs=%v", out, errs)
	}
	if exit != 0 {
		t.Errorf("exit=%d, want 0(echo 是最后一条)", exit)
	}

	// `||` 兜底必须能走:这是 false 改写存在的全部理由。
	out, exit, errs = s.Run(context.Background(), "kill 999999 || echo gone", false)
	if exit != 0 || out != "gone\n" {
		t.Errorf("kill 失败后 || 没有兜底: exit=%d out=%q errs=%v", exit, out, errs)
	}

	// 强杀不存在的 pid 同样报错不终止。
	out, exit, errs = s.Run(context.Background(), "kill -9 999999 || echo gone", false)
	if exit != 0 || out != "gone\n" {
		t.Errorf("kill -9 失败后 || 没有兜底: exit=%d out=%q errs=%v", exit, out, errs)
	}

	// 错误文案点名 PID 与失败原因概要(taskkill rc=128 = 没有找到进程)。
	errText := strings.Join(errs, " / ")
	if !strings.Contains(errText, "999999") || !strings.Contains(errText, "taskkill rc=128") {
		t.Errorf("错误文案缺 PID 或 taskkill rc: %q", errText)
	}
}
