package shell

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/interp"

	"winbash/internal/errs"
)

// killGuard 接管 mvdan/sh 的 kill 内建名。
//
// 为什么要接管:mvdan/sh 把 kill 列进 POSIX 内建名表(third_party/mvdan-sh/interp/
// builtin.go 的 IsBuiltin 名单),runner 对内建名**短路**、根本不走 ExecHandler
// (runner.go 的 call:IsBuiltin → r.builtin),而 builtin 的 dispatch 没有 kill 的
// case → 恒 failf(2, "unsupported builtin")。也就是说 kill 任何形式都失败,而
// ③ 层明明有 Git Bash 自带的 kill.exe 也轮不到。修法与 printf -v / trap 同一先例:
// CallHandler 在内建 dispatch 之前,是唯一能插手的地方。
//
// 语义(Windows 信号等价模型 —— Windows 没有信号,kill 只有两种力度):
//
//	kill <pid>...        逐个 taskkill /PID <pid>(优雅尝试;成功静默,对齐 bash)
//	kill -9 / -KILL / -SIGKILL <pid>...   taskkill /PID <pid> /F(强杀)
//	kill -l              打 POSIX 1–15 信号名对照表(注明这只是名字→taskkill 语义的映射)
//	kill -l <n> / <NAME> 打对应名字 / 编号(对齐 bash 的双向用法)
//	kill %1              作业号:响亮拒绝(没有作业控制),指路 ps
//	kill -s X 等其它旗标  响亮拒绝 + remedy
//
// 实测边界(2026-10-31,决定 remedy 措辞):优雅 taskkill 给目标进程的顶层窗口发
// WM_CLOSE —— **带窗口**的程序才收得到(Start-Process 拉起的隐藏窗口进程能被优雅终止);
// go test / daemon 直接 exec 出来的控制台进程没有窗口,taskkill 自己拒绝:
// `只能强行终止这个进程(带 /F 选项)`(rc=1;inherit console / CREATE_NO_WINDOW /
// DETACHED_PROCESS 三种方式都实测如此)。所以失败文案明确指向 kill -9。
//
// 失败的控制流与 printf/trap 守卫**不同**,是有意的:callHandler 返回 error 在
// mvdan 里是 fatal(整个脚本终止),而真 bash 的 kill 失败只是该命令 rc=1、脚本继续
// —— `kill $PID || echo gone` 必须能工作。所以错误路径把三行式文案写进 stderr 后把
// 命令改写成 false(成功路径改写成 true,同样静默);两种改写都保留原语句上的重定向。
//
// 边界:CallHandler 在函数查找**之前**,所以脚本里自定义的 kill 函数会被这里挡住
// —— 与 echo / cd / trap 既有守卫同一形状,不是本守卫新引入的取舍。
// `command kill` / `builtin kill` 一并接管:mvdan 的 command/builtin 内建会**直接**
// 调 r.builtin("kill") 绕过 CallHandler,不拦的话又落回 unsupported builtin
// (与 normalizeCdInvocation 处理 command cd / builtin cd 同一理由)。
func killGuard(ctx context.Context, args []string) ([]string, bool) {
	rest, ok := splitKillInvocation(args)
	if !ok {
		return nil, false
	}
	hc := interp.HandlerCtx(ctx)

	plan, perr := parseKillArgs(rest)
	if perr != nil {
		fmt.Fprintln(hc.Stderr, perr.Error())
		return []string{"false"}, true
	}

	if plan.list {
		if len(plan.specs) == 0 {
			fmt.Fprint(hc.Stdout, killSignalTableText())
		}
		for _, spec := range plan.specs {
			out, ok := killSignalLookup(spec)
			if !ok {
				fmt.Fprintln(hc.Stderr, errs.Errorf("kill",
					fmt.Sprintf("%s 不是已知的信号编号或名字", spec),
					"先用 kill -l 看全表(Windows 没有信号,这张表只是名字与 kill 语义的对照)"))
				return []string{"false"}, true
			}
			fmt.Fprintln(hc.Stdout, out)
		}
		return []string{"true"}, true
	}

	if failed := runKillPids(plan.pids, plan.force, killRunTaskkill); len(failed) > 0 {
		fmt.Fprintln(hc.Stderr, errs.Errorf("kill",
			"没能终止进程: "+strings.Join(failed, "; "),
			"先确认 PID 还活着: winbash -c 'ps';进程还在就用强杀: kill -9 <pid>"+
				"(优雅信号只有带窗口的程序收得到,控制台程序通常要 -9)"))
		return []string{"false"}, true
	}
	return []string{"true"}, true
}

// splitKillInvocation 认出三种形态:kill …、command kill …、builtin kill …。
// 后两种返回的 rest 已剥掉前缀;不是 kill 就返回 false(放行)。
func splitKillInvocation(args []string) (rest []string, ok bool) {
	switch {
	case len(args) >= 1 && args[0] == "kill":
		return args[1:], true
	case len(args) >= 2 && (args[0] == "command" || args[0] == "builtin") && args[1] == "kill":
		return args[2:], true
	default:
		return nil, false
	}
}

// killPlan 是 parseKillArgs 的结果。pids 在 !list 时保证非空(空即解析错误)。
type killPlan struct {
	list  bool     // kill -l
	specs []string // -l 的参数(空 = 打全表)
	force bool     // -9 / -KILL / -SIGKILL
	pids  []string
}

// parseKillArgs 解析 kill 的参数(不含 "kill" 本身)。
// 旗标与 PID 的相对顺序不挑(bash 的 getopts 在第一个非旗标处停,这里更宽:
// `kill 123 -9` 也当强杀 —— 旗标语义不因位置而变,不会静默做错事)。
func parseKillArgs(rest []string) (killPlan, error) {
	var plan killPlan
	endOfFlags := false
	for _, a := range rest {
		switch {
		case !endOfFlags && a == "--":
			endOfFlags = true
		case !endOfFlags && a == "-l":
			if plan.force {
				return plan, killUsageErr("-l 不能与 -9 同时使用")
			}
			plan.list = true
		case !endOfFlags && (a == "-9" || strings.EqualFold(a, "-KILL") || strings.EqualFold(a, "-SIGKILL")):
			if plan.list {
				return plan, killUsageErr("-9 不能与 -l 同时使用")
			}
			plan.force = true
		case !endOfFlags && len(a) > 1 && a[0] == '-':
			return plan, killUsageErr(fmt.Sprintf(
				"不支持旗标 %s(Windows 上没有信号,只有强杀 -9/-KILL 可用;优雅尝试是默认,不用旗标)", a))
		case plan.list:
			plan.specs = append(plan.specs, a)
		case strings.HasPrefix(a, "%"):
			return plan, errors.New(errs.Errorf("kill",
				fmt.Sprintf("%s 是作业号写法,而 Windows 版没有作业控制", a),
				"先用 winbash -c 'ps' 查到进程的 Windows PID,再 kill <pid>"))
		case isKillDigits(a):
			plan.pids = append(plan.pids, a)
		default:
			return plan, killUsageErr(fmt.Sprintf("%s 不是 Windows PID(只收纯数字)", a))
		}
	}
	if plan.list {
		return plan, nil
	}
	if len(plan.pids) == 0 {
		return plan, killUsageErr("缺少进程 ID")
	}
	return plan, nil
}

func killUsageErr(problem string) error {
	return errors.New(errs.Errorf("kill", problem,
		"用法: kill [-9] <pid>... | kill -l [<编号>|<名字>];查 PID 用 winbash -c 'ps'"))
}

func isKillDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// killSignalNames 是 POSIX 1–15 的信号名(下标即编号,0 号空置)。
// Windows 没有信号;这张表唯一的存在理由是 kill -l 的对照输出与本实现的两种力度:
// 9/KILL → taskkill /F,其余 → 优雅尝试。只收 1–15:kill 的实际语义只有两档,
// 列更多名字只会暗示存在不存在的力度差别。
var killSignalNames = [...]string{
	"", "HUP", "INT", "QUIT", "ILL", "TRAP", "ABRT", "BUS", "FPE",
	"KILL", "USR1", "SEGV", "USR2", "PIPE", "ALRM", "TERM",
}

// killSignalLookup 对齐 bash 的双向用法:编号 → 名字,名字 → 编号。
// 名字不区分大小写、SIG 前缀可有可无(kill -l sigkill → 9)。
func killSignalLookup(spec string) (string, bool) {
	if n, err := strconv.Atoi(spec); err == nil {
		if n >= 1 && n < len(killSignalNames) {
			return killSignalNames[n], true
		}
		return "", false
	}
	name := strings.TrimPrefix(strings.ToUpper(spec), "SIG")
	for n := 1; n < len(killSignalNames); n++ {
		if killSignalNames[n] == name {
			return strconv.Itoa(n), true
		}
	}
	return "", false
}

// killSignalTableText 是 kill -l 的全表:首行注明这是对照而非真信号。
func killSignalTableText() string {
	var b strings.Builder
	b.WriteString("# Windows 没有信号:这张表只是 POSIX 信号名与本实现 kill 语义的对照" +
		" —— 9/KILL → taskkill /PID <pid> /F 强杀,其余 → taskkill /PID <pid> 优雅尝试\n")
	for n := 1; n < len(killSignalNames); n++ {
		fmt.Fprintf(&b, "%d %s\n", n, killSignalNames[n])
	}
	return b.String()
}

// runKillPids 对每个 pid 逐个执行,**不因前一个失败而停**(对齐 bash:kill 报出
// 所有失败者、只要有一个失败整体就非零)。返回值是失败者及原因概要;全成功为空。
func runKillPids(pids []string, force bool, run func(argv []string) error) []string {
	var failed []string
	for _, pid := range pids {
		argv := []string{"taskkill", "/PID", pid}
		if force {
			argv = append(argv, "/F")
		}
		if err := run(argv); err != nil {
			failed = append(failed, pid+"("+killTaskkillBrief(err)+")")
		}
	}
	return failed
}

// killTaskkillBrief 把 taskkill 的失败压成一句。**不透传 taskkill 的原始输出**:
// 中文 Windows 上它是 GBK 编码(「错误: 没有找到进程 …」),拼进 UTF-8 管道就是乱码;
// 退出码足以区分「进程不存在」(128)与「拒绝终止」(1)。
func killTaskkillBrief(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return "taskkill rc=" + strconv.Itoa(ee.ExitCode())
	}
	return "taskkill " + err.Error()
}

// killRunTaskkill 是 runKillPids 的真实执行器。taskkill 的输出一律丢弃:
// 成功时 bash 的 kill 静默,失败时我们用自己的三行式 —— 两头都不需要它的本地化文本。
func killRunTaskkill(argv []string) error {
	return exec.Command(argv[0], argv[1:]...).Run()
}
