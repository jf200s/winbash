package winmgmt

import (
	"context"
	"fmt"
	"io"
	"strings"

	"winbash/internal/errs"
	"winbash/internal/pwsh"
)

// 三种固定列序。单项查询是 Field/Value 两列,概览多一列 Section 以便一次拿全,
// disk 天生是每条卷一条记录。
var (
	sysPairColumns     = []string{"Field", "Value"}
	sysOverviewColumns = []string{"Section", "Field", "Value"}
	sysDiskColumns     = []string{"Device", "TotalBytes", "FreeBytes"}
)

const sysUsage = "用法: sys [os|cpu|mem|disk] [--header]"

// sysPrelude 提供概要里把「一节一行」拼起来的辅助函数。
const sysPrelude = `
function __wb_pair([string]$section, [string]$field, $value) {
    return [ordered]@{ Section = $section; Field = $field; Value = $value }
}
`

// sysOSScript / sysCPUScript / sysMemScript / sysDiskScript 是四种单项查询。
//
// 容量一律给**原始字节数**:CIM 的 *MemorySize 字段单位是 KB(历史上就是如此),
// 直接透传等于把「86,488 K」换个写法继续丢给使用者去换算 ——
// 这里显式 *1024 变成字节,值与单位从此没有歧义(spec §6.2)。
const (
	sysOSScript = `
$o = Get-CimInstance Win32_OperatingSystem
@(
    [ordered]@{ Field = 'Caption';      Value = [string]$o.Caption },
    [ordered]@{ Field = 'Version';      Value = [string]$o.Version },
    [ordered]@{ Field = 'BuildNumber';  Value = [string]$o.BuildNumber },
    [ordered]@{ Field = 'Architecture'; Value = [string]$o.OSArchitecture },
    [ordered]@{ Field = 'LastBoot';     Value = $o.LastBootUpTime.ToString('yyyy-MM-dd HH:mm:ss') }
)`

	sysCPUScript = `
$c = Get-CimInstance Win32_Processor | Select-Object -First 1
@(
    [ordered]@{ Field = 'Name';              Value = ([string]$c.Name).Trim() },
    [ordered]@{ Field = 'Cores';             Value = [int]$c.NumberOfCores },
    [ordered]@{ Field = 'LogicalProcessors'; Value = [int]$c.NumberOfLogicalProcessors },
    [ordered]@{ Field = 'MaxClockMHz';       Value = [int]$c.MaxClockSpeed }
)`

	sysMemScript = `
$o = Get-CimInstance Win32_OperatingSystem
@(
    [ordered]@{ Field = 'TotalBytes';        Value = [int64]$o.TotalVisibleMemorySize * 1024 },
    [ordered]@{ Field = 'FreeBytes';         Value = [int64]$o.FreePhysicalMemory * 1024 },
    [ordered]@{ Field = 'VirtualTotalBytes'; Value = [int64]$o.TotalVirtualMemorySize * 1024 },
    [ordered]@{ Field = 'VirtualFreeBytes';  Value = [int64]$o.FreeVirtualMemory * 1024 }
)`

	sysDiskScript = `
Get-CimInstance Win32_LogicalDisk -Filter 'DriveType=3' | Sort-Object -Property DeviceID | ForEach-Object {
    [ordered]@{ Device = [string]$_.DeviceID; TotalBytes = [int64]$_.Size; FreeBytes = [int64]$_.FreeSpace }
}`

	sysOverviewScript = sysPrelude + `
$o = Get-CimInstance Win32_OperatingSystem
$c = Get-CimInstance Win32_Processor | Select-Object -First 1
$rows = @()
# 每个 cast 都必须包在括号里。实测:在 PowerShell 的**命令参数模式**里写
#   __wb_pair 'os' 'Caption' [string]$o.Caption
# 得到的 Value 是 "[string]Win32_OperatingSystem: ... .Caption" ——
# 命令模式与表达式模式对 cast 的解析不是一回事,括号是这里的强制要求。
$rows += __wb_pair 'os'  'Caption' ([string]$o.Caption)
$rows += __wb_pair 'os'  'Version' ([string]$o.Version)
$rows += __wb_pair 'cpu' 'Name'    (([string]$c.Name).Trim())
$rows += __wb_pair 'cpu' 'Cores'   ([int]$c.NumberOfCores)
$rows += __wb_pair 'mem' 'TotalBytes' ([int64]$o.TotalVisibleMemorySize * 1024)
$rows += __wb_pair 'mem' 'FreeBytes'  ([int64]$o.FreePhysicalMemory * 1024)
foreach ($d in (Get-CimInstance Win32_LogicalDisk -Filter 'DriveType=3' | Sort-Object -Property DeviceID)) {
    $rows += __wb_pair 'disk' ([string]$d.DeviceID) ([int64]$d.Size)
}
$rows`
)

// Sys 报告这台机器的概况。无参数给概览(os + cpu + mem + disk 各几行),
// 给子命令只看一项 —— 概览的用处是「一眼看清」,单项的用处是「精确取值」。
func Sys(ctx context.Context, conn *pwsh.Conn, argv []string, jsonOut bool, out, errw io.Writer) int {
	var (
		sub        string
		withHeader bool
	)
	for _, a := range argv {
		switch {
		case a == "--header":
			withHeader = true
		case strings.HasPrefix(a, "-"):
			fmt.Fprintln(errw, errs.Errorf("sys", "不认识的参数: "+a, sysUsage))
			return errs.ExitUsage
		case sub == "":
			sub = a
		default:
			fmt.Fprintln(errw, errs.Errorf("sys", "多余的参数: "+a, sysUsage))
			return errs.ExitUsage
		}
	}

	var (
		script  string
		columns []string
	)
	switch sub {
	case "":
		script, columns = sysOverviewScript, sysOverviewColumns
	case "os":
		script, columns = sysOSScript, sysPairColumns
	case "cpu":
		script, columns = sysCPUScript, sysPairColumns
	case "mem":
		script, columns = sysMemScript, sysPairColumns
	case "disk":
		script, columns = sysDiskScript, sysDiskColumns
	default:
		fmt.Fprintln(errw, errs.Errorf("sys", "不认识的子命令: "+sub, sysUsage))
		return errs.ExitUsage
	}

	records, err := runRecords(ctx, conn, script)
	if err != nil {
		fmt.Fprintln(errw, errs.Errorf("sys", stripEnginePrefix(err.Error()),
			"这类查询走 CIM(WMI);确认 Windows Management Instrumentation 服务在跑"))
		return errs.ExitError
	}
	if err := renderRecords(records, columns, withHeader, jsonOut, out); err != nil {
		fmt.Fprintln(errw, errs.Errorf("sys", err.Error(), "写输出时出错"))
		return errs.ExitError
	}
	return errs.ExitOK
}
