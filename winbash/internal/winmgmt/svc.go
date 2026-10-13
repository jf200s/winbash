package winmgmt

import (
	"context"
	"fmt"
	"io"

	"winbash/internal/errs"
	"winbash/internal/pwsh"
)

// svcColumns 是 `svc` 输出的**固定列序**。不得变更:使用者会写死 `cut -f2`。
var svcColumns = []string{"Name", "Status", "StartType"}

const svcUsage = "用法: svc list [--running|--stopped] [--header] | svc status <名称> | svc start|stop|restart <名称>"

// Svc 暴露 Windows 服务的查询与控制。
//
// 为什么叫 `svc` 而不是 `service`:Windows 侧另有 `sc.exe`(语义不同、参数风格是
// `sc query`),而 `service` 在 bash 里是常见的用户函数名 —— 换个短词,冲突面最小。
func Svc(ctx context.Context, conn *pwsh.Conn, argv []string, jsonOut bool, out, errw io.Writer) int {
	if len(argv) == 0 {
		fmt.Fprintln(errw, errs.Errorf("svc", "缺少子命令", svcUsage))
		return errs.ExitUsage
	}

	switch argv[0] {
	case "list":
		return svcList(ctx, conn, argv[1:], jsonOut, out, errw)
	case "status":
		return svcStatus(ctx, conn, argv[1:], jsonOut, out, errw)
	case "start", "stop", "restart":
		return svcControl(ctx, conn, argv[0], argv[1:], jsonOut, out, errw)
	default:
		fmt.Fprintln(errw, errs.Errorf("svc", "不认识的子命令: "+argv[0], svcUsage))
		return errs.ExitUsage
	}
}

func svcList(ctx context.Context, conn *pwsh.Conn, argv []string, jsonOut bool, out, errw io.Writer) int {
	var (
		filter     string
		withHeader bool
		sawFilter  string
	)

	for _, a := range argv {
		switch a {
		case "--running", "--stopped":
			if sawFilter != "" {
				// 两个互斥的过滤条件同时给:报错,而不是让后一个悄悄覆盖前一个。
				fmt.Fprintln(errw, errs.Errorf("svc", a+" 与 "+sawFilter+" 互斥", svcUsage))
				return errs.ExitUsage
			}
			sawFilter = a
			if a == "--running" {
				filter = " | Where-Object { $_.Status.ToString() -eq 'Running' }"
			} else {
				filter = " | Where-Object { $_.Status.ToString() -eq 'Stopped' }"
			}
		case "--header":
			withHeader = true
		default:
			fmt.Fprintln(errw, errs.Errorf("svc", "不认识的参数: "+a, svcUsage))
			return errs.ExitUsage
		}
	}

	records, err := runRecords(ctx, conn, "Get-Service"+filter+svcProjection)
	if err != nil {
		fmt.Fprintln(errw, errs.Errorf("svc", stripEnginePrefix(err.Error()), "确认 pwsh 能跑: winbash -c 'pwsh -NoProfile -c \"Get-Service | Select-Object -First 1\"'"))
		return errs.ExitError
	}

	if err := renderRecords(records, svcColumns, withHeader, jsonOut, out); err != nil {
		fmt.Fprintln(errw, errs.Errorf("svc", err.Error(), "写输出时出错"))
		return errs.ExitError
	}
	return errs.ExitOK
}

func svcStatus(ctx context.Context, conn *pwsh.Conn, argv []string, jsonOut bool, out, errw io.Writer) int {
	if len(argv) != 1 {
		fmt.Fprintln(errw, errs.Errorf("svc", "status 需要一个服务名", svcUsage))
		return errs.ExitUsage
	}
	name := argv[0]

	// -ErrorAction Stop 是这里的要害:服务不存在时 Get-Service 默认只给一条
	// non-terminating error(退出码不变),调用方会拿到一行空数据、以为服务没在跑。
	// 换成终止性错误,它才会变成非零退出 + 三行式错误。
	script := "Get-Service -Name " + psQuote(name) + " -ErrorAction Stop" + svcProjection

	records, err := runRecords(ctx, conn, script)
	if err != nil {
		fmt.Fprintln(errw, errs.Errorf("svc", stripEnginePrefix(err.Error()), "用 svc list 看看有哪些服务名(可用 --running 过滤)"))
		return errs.ExitError
	}

	if err := renderRecords(records, svcColumns, false, jsonOut, out); err != nil {
		fmt.Fprintln(errw, errs.Errorf("svc", err.Error(), "写输出时出错"))
		return errs.ExitError
	}
	return errs.ExitOK
}

func svcControl(ctx context.Context, conn *pwsh.Conn, action string, argv []string, jsonOut bool, out, errw io.Writer) int {
	if len(argv) != 1 {
		fmt.Fprintln(errw, errs.Errorf("svc", action+" 需要一个服务名", svcUsage))
		return errs.ExitUsage
	}
	name := argv[0]

	verb := map[string]string{
		"start":   "Start-Service",
		"stop":    "Stop-Service",
		"restart": "Restart-Service",
	}[action]

	// 控制类 cmdlet 会把 ServiceController 对象写到输出流。若不用 Out-Null 丢掉,
	// 它会和下面那条查询结果混进同一个 JSON 里 —— 逐项序列化也救不了形状。
	// 然后用一次查询回读真实状态:与其说「命令成功了」,不如给出结果本身。
	script := verb + " -Name " + psQuote(name) + " -ErrorAction Stop | Out-Null; " +
		"Get-Service -Name " + psQuote(name) + " -ErrorAction Stop" + svcProjection

	records, err := runRecords(ctx, conn, script)
	if err != nil {
		fmt.Fprintln(errw, errs.Errorf("svc", stripEnginePrefix(err.Error()),
			"这类操作通常需要管理员权限;请在提升的终端里执行,或先确认服务名: svc list"))
		return errs.ExitError
	}

	if err := renderRecords(records, svcColumns, false, jsonOut, out); err != nil {
		fmt.Fprintln(errw, errs.Errorf("svc", err.Error(), "写输出时出错"))
		return errs.ExitError
	}
	return errs.ExitOK
}

// svcProjection 是把服务对象压成三列的固定投影。
//
// `$_.Status.ToString()` 这两个 ToString() 是值保证的落地点:不加它们,
// ConvertTo-Json 会把枚举序列化成裸数字(实测:StartType 给的是 `3`),
// 而 `3` 正是 spec §6 点名要消灭的 "STATE: 1" 那一类歧义。
var svcProjection = " | ForEach-Object { [ordered]@{ Name = $_.Name; Status = $_.Status.ToString(); StartType = $_.StartType.ToString() } }"
