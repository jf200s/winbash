// `--fresh` 的语义测试。
//
// 为什么值得单独一个文件:`runJob` 里那个 `if req.Fresh` 分支**原先一条测试都没有**
// (覆盖率 0%),而它是三个功能共用的隔离机制 —— 一次性会话的变量、set -e、trap
// 都不该带进去,也不该漏出来。
//
// 断言的依据全部来自**手工实测**(下面每条都写了实测到的输出),不是照代码推测:
//
//	长驻 V=1            → 长驻读 $V = "1";  --fresh 读 $V = ""
//	长驻 set -e         → 长驻 `false; echo AFTER` = exit 1、无 AFTER
//	                       --fresh 同样脚本   = exit 0、有 AFTER
//	长驻 trap ... EXIT  → 长驻 `echo Y` = "Y\nTRAPPED";  --fresh `echo X` = "X"
//	--fresh exit 7      → 7(且随后的长驻命令仍是 0)
//
// 其中 trap 那条最有价值:它要求**同一个 daemon 上先 A 后 B**的序列 —— 只跑通一条
// 证明不了"两次调用之间会话是干净的"(AGENTS.md §4.3 记着这个教训)。
//
// 每条测试都用独立 daemon(startDaemon 每次给新 endpoint):set -e 与 trap 一旦装上
// 就会污染后续所有命令,共用 daemon 会让测试之间互相干扰。

package daemon

import (
	"strings"
	"testing"
	"time"

	"winbash/internal/client"
)

const (
	freshTestTimeout = 60 * time.Second
	freshTestIdle    = 60 * time.Second
)

// run 在 ep 上跑一条命令,cwd 固定为**同一个临时目录**。
//
// 为什么固定传 cwd:client 每次都把**自己进程的 cwd** 发给 daemon,daemon 收到就
// `cd` 过去(见 runJob 的 req.Cwd 分支)。所以"cwd 跨命令不持久"是这个机制的
// 结果,不是会话状态被清了 —— 测试里固定 cwd 才能把这一点隔离掉,免得把
// "每条命令都被 cd 重置"误当成"fresh 的 cwd 隔离生效了"。
func run(t *testing.T, ep, cwd, script string, opts ...client.Option) (out string, exit int, ok bool) {
	t.Helper()
	resp, err := client.RunTimeout(ep, script, cwd, false, freshTestTimeout, freshTestIdle, opts...)
	if err != nil {
		t.Fatalf("跑 %q 失败: %v", script, err)
	}
	return string(resp.Out), resp.Exit, resp.OK
}

// ★ `--fresh` 不得继承长驻会话的**变量**。
//
// 实测:长驻 `V=1` 之后,长驻读得 "V=[1]",而 `--fresh` 读到 "V=[]"。
func TestFreshDoesNotInheritVariables(t *testing.T) {
	ep := startDaemon(t, DefaultReadTimeout)
	cwd := t.TempDir()

	// 先确认变量在长驻会话里确实持久 —— 否则下面的断言会"因为变量压根没生效"而假绿。
	if _, _, ok := run(t, ep, cwd, "V=1; echo set"); !ok {
		t.Fatal("设变量那条命令就没成功")
	}
	if out, _, _ := run(t, ep, cwd, `echo "V=[$V]"`); !strings.Contains(out, "V=[1]") {
		t.Fatalf("长驻会话里变量应当持久(对照组),得到 %q", out)
	}

	// 关键断言:fresh 看不到它。
	out, _, _ := run(t, ep, cwd, `echo "V=[$V]"`, client.Fresh())
	if strings.Contains(out, "V=[1]") {
		t.Fatalf("--fresh 继承了长驻会话的变量(应当用一次性会话): %q", out)
	}

	// 再确认长驻会话的变量还在 —— fresh 也不该把它清掉。
	if out, _, _ := run(t, ep, cwd, `echo "V=[$V]"`); !strings.Contains(out, "V=[1]") {
		t.Fatalf("--fresh 之后长驻会话的变量被弄丢了: %q", out)
	}
}

// ★ `--fresh` 不得继承长驻会话的 set -e。
//
// 实测(同一个 daemon 上先后跑):
//
//	长驻 `set -e`               → 之后长驻 `false; echo AFTER` = exit 1、输出为空
//	--fresh `false; echo AFTER` → exit 0、输出 "AFTER"
//
// 这一条比变量那条更容易出事:set -e 一旦漏进去,一条"本来会失败但被 || 兜住"的
// 命令会突然中断,而调用方看不出原因。
func TestFreshDoesNotInheritSetDashE(t *testing.T) {
	ep := startDaemon(t, DefaultReadTimeout)
	cwd := t.TempDir()

	// 长驻会话打开 set -e。
	if _, _, ok := run(t, ep, cwd, "set -e; echo armed"); !ok {
		t.Fatal("开 set -e 那条命令没成功")
	}
	// 对照组:长驻会话里 set -e 生效 —— false 让整条命令立刻以 1 结束,AFTER 不打印。
	out, exit, _ := run(t, ep, cwd, "false; echo AFTER")
	if exit != 1 || strings.Contains(out, "AFTER") {
		t.Fatalf("对照组失败:长驻会话里 set -e 应当生效。得到 exit=%d out=%q", exit, out)
	}

	// 关键断言:fresh 里没有 set -e,脚本应当跑完。
	out, exit, ok := run(t, ep, cwd, "false; echo AFTER", client.Fresh())
	if exit != 0 {
		t.Fatalf("--fresh 继承了 set -e:exit=%d(应当 0),out=%q", exit, out)
	}
	if !strings.Contains(out, "AFTER") {
		t.Fatalf("--fresh 里 set -e 生效了,AFTER 没打印:out=%q", out)
	}
	if !ok {
		t.Fatalf("fresh 那条命令被报成失败:out=%q", out)
	}
}

// ★ `--fresh` 不得继承长驻会话的 trap,长驻会话的 trap 也不得被它触发。
//
// 实测:
//
//	长驻 `trap 'echo TRAPPED' EXIT; echo armed`
//	长驻 `echo Y`    → "Y\nTRAPPED"     ← trap 跨命令持久(既有设计)
//	--fresh `echo X` → "X"              ← 没有 TRAPPED
//
// 这条要求**先 A 后 B**的序列:先证明长驻的 trap 真的在,再证明 fresh 看不到。
// 只测"fresh 输出里没有 TRAPPED"是不够的 —— 若 trap 压根没装上,那条断言也会绿。
func TestFreshDoesNotInheritTrap(t *testing.T) {
	ep := startDaemon(t, DefaultReadTimeout)
	cwd := t.TempDir()

	// 装上 trap。注意 mvdan/sh 的 trap 只支持 ERR/EXIT(见 README 已知差异)。
	if _, _, ok := run(t, ep, cwd, "trap 'echo TRAPPED' EXIT; echo armed"); !ok {
		t.Fatal("装 trap 那条命令没成功")
	}
	// 对照组:长驻会话的 trap 在**后续**命令结束时依然触发 —— 这就是"trap 跨命令持久"。
	out, _, _ := run(t, ep, cwd, "echo Y")
	if !strings.Contains(out, "TRAPPED") {
		t.Fatalf("对照组失败:长驻会话的 EXIT trap 应当在后续命令结束时触发,"+
			"否则下面的断言无从判断。out=%q", out)
	}

	// 关键断言:fresh 不触发那条 trap。
	out, _, _ = run(t, ep, cwd, "echo X", client.Fresh())
	if strings.Contains(out, "TRAPPED") {
		t.Fatalf("--fresh 继承了长驻会话的 trap:out=%q", out)
	}
	if !strings.Contains(out, "X") {
		t.Fatalf("脚本本身没跑出预期输出:out=%q", out)
	}
}

// ★ `--fresh` 里改的东西不得漏回长驻会话,退出码要原样带回。
//
// 实测:`--fresh cd /c/Windows && pwd` 之后长驻会话的 cwd 不变;
// `--fresh exit 7` → 7,随后长驻 `exit 0` → 0。
func TestFreshDoesNotPolluteSessionAndKeepsExitCode(t *testing.T) {
	ep := startDaemon(t, DefaultReadTimeout)
	cwd := t.TempDir()

	// fresh 里设变量 + cd,两样都不该漏出去。
	if _, _, ok := run(t, ep, cwd, "LEAK=1; cd /c/Windows; echo done", client.Fresh()); !ok {
		t.Fatal("fresh 那条命令没成功")
	}
	out, _, _ := run(t, ep, cwd, `echo "LEAK=[$LEAK]"; pwd`)
	if strings.Contains(out, "LEAK=[1]") {
		t.Fatalf("--fresh 里的变量漏进了长驻会话: %q", out)
	}
	if strings.Contains(out, `C:\Windows`) {
		t.Fatalf("--fresh 里的 cd 漏进了长驻会话: %q", out)
	}

	// fresh 的退出码必须原样带回(它不是"隔离掉了连结果都不要")。
	out, exit, ok := run(t, ep, cwd, "exit 7", client.Fresh())
	if exit != 7 {
		t.Fatalf("--fresh 的退出码没原样带回:exit=%d(应当 7), out=%q", exit, out)
	}
	if ok {
		t.Fatal("exit 7 不该被报成 OK=true")
	}

	// 且不影响随后的长驻命令。
	if _, exit, _ := run(t, ep, cwd, "exit 0"); exit != 0 {
		t.Fatalf("--fresh 之后长驻命令受影响:exit=%d", exit)
	}
}
