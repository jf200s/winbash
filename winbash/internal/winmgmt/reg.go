package winmgmt

import (
	"context"
	"fmt"
	"io"
	"strings"

	"winbash/internal/errs"
	"winbash/internal/pwsh"
)

// 固定列序。`reg get` 与 `reg keys` 各自固定,不随数据变化。
var (
	regValueColumns = []string{"Name", "Type", "Data"}
	regKeyColumns   = []string{"Name"}
)

const regUsage = "用法: reg get <键> [值名] [--header] | reg set <键> <值名> <数据> [--type T] | reg del <键> [值名] | reg keys <键>"

// Reg 是 Windows 注册表的读写口。
//
// 值保证在这一层最要紧:注册表导出的十六进制(`0x0000000a`)与"类型码"是最典型的
// 歧义来源。做法是**不在 pwsh 侧拼字符串**,而是把 .NET 拿到的原生值直接交给
// ConvertTo-Json —— int32 就是 JSON 数字、string[] 就是 JSON 数组,
// 于是 `0xa` 自然变成 `10`,不需要任何"格式化"步骤(也就没有改错的机会)。
func Reg(ctx context.Context, conn *pwsh.Conn, argv []string, jsonOut bool, out, errw io.Writer) int {
	if len(argv) == 0 {
		fmt.Fprintln(errw, errs.Errorf("reg", "缺少子命令", regUsage))
		return errs.ExitUsage
	}

	switch argv[0] {
	case "get":
		return regGet(ctx, conn, argv[1:], jsonOut, out, errw)
	case "set":
		return regSet(ctx, conn, argv[1:], jsonOut, out, errw)
	case "del":
		return regDel(ctx, conn, argv[1:], out, errw)
	case "keys":
		return regKeys(ctx, conn, argv[1:], jsonOut, out, errw)
	default:
		fmt.Fprintln(errw, errs.Errorf("reg", "不认识的子命令: "+argv[0], regUsage))
		return errs.ExitUsage
	}
}

// regPrelude 是每段脚本都要用到的三个辅助函数。
//
// 用 .NET 的 RegistryKey 而不是 PowerShell 的 `HKLM:\` 提供者路径:
// 提供者路径把「键」和「值」的差别模糊掉了(读值得拼 -Name,删值删不掉键),
// 而 .NET API 的 GetValueNames / GetValueKind / GetValue 正好对得上我们的三列。
const regPrelude = `
function __wb_regroot([string]$short) {
    switch ($short.ToUpper()) {
        'HKLM' { return [Microsoft.Win32.Registry]::LocalMachine }
        'HKCU' { return [Microsoft.Win32.Registry]::CurrentUser }
        'HKCR' { return [Microsoft.Win32.Registry]::ClassesRoot }
        'HKU'  { return [Microsoft.Win32.Registry]::Users }
        'HKCC' { return [Microsoft.Win32.Registry]::CurrentConfig }
    }
    throw "不认识的注册表根 '$short';可用 HKLM / HKCU / HKCR / HKU / HKCC,例如 HKLM\SOFTWARE\Microsoft"
}
function __wb_regsplit([string]$path) {
    $i = $path.IndexOf('\')
    if ($i -lt 0) {
        throw "注册表键要写成 ROOT\子键(例如 HKLM\SOFTWARE\Microsoft);收到 '$path'"
    }
    return @{ Root = $path.Substring(0, $i); Sub = $path.Substring($i + 1) }
}
function __wb_regopen([string]$path, [bool]$writable) {
    $p = __wb_regsplit $path
    $root = __wb_regroot $p.Root
    if ($writable) { return $root.CreateSubKey($p.Sub) }
    return $root.OpenSubKey($p.Sub)
}
`

func regGet(ctx context.Context, conn *pwsh.Conn, argv []string, jsonOut bool, out, errw io.Writer) int {
	var (
		pos        []string
		withHeader bool
	)
	for _, a := range argv {
		if a == "--header" {
			withHeader = true
			continue
		}
		pos = append(pos, a)
	}
	if len(pos) < 1 || len(pos) > 2 {
		fmt.Fprintln(errw, errs.Errorf("reg", "get 需要 <键> 或 <键> <值名>", regUsage))
		return errs.ExitUsage
	}
	key := pos[0]
	name := ""
	if len(pos) == 2 {
		name = pos[1]
	}

	head := "$__wb_key = " + psQuote(key) + "\n" +
		"$k = __wb_regopen $__wb_key $false\n" +
		`if ($null -eq $k) { throw "注册表键不存在: $__wb_key(先确认键名,或用 reg keys 看它的子键)" }` + "\n"

	var body string
	if len(pos) == 1 {
		// 无值名:列出该键下的全部值(含默认值,它的名字是空字符串)。
		body = `$__wb_rows = @($k.GetValueNames()) | ForEach-Object { [ordered]@{ Name = $_; Type = $k.GetValueKind($_).ToString(); Data = $k.GetValue($_) } }` + "\n" +
			`$__wb_rows | Sort-Object -Property Name`
	} else {
		body = "$__wb_name = " + psQuote(name) + "\n" +
			`if (@($k.GetValueNames()) -notcontains $__wb_name) { throw "注册表值不存在: $__wb_name(用 reg get '$__wb_key' 列出该键下全部值)" }` + "\n" +
			`[ordered]@{ Name = $__wb_name; Type = $k.GetValueKind($__wb_name).ToString(); Data = $k.GetValue($__wb_name) }`
	}

	records, err := runRecords(ctx, conn, regPrelude+head+body)
	if err != nil {
		fmt.Fprintln(errw, errs.Errorf("reg", stripEnginePrefix(err.Error()), "确认键名与值名: reg get "+key))
		return errs.ExitError
	}
	if err := renderRecords(records, regValueColumns, withHeader, jsonOut, out); err != nil {
		fmt.Fprintln(errw, errs.Errorf("reg", err.Error(), "写输出时出错"))
		return errs.ExitError
	}
	return errs.ExitOK
}

func regSet(ctx context.Context, conn *pwsh.Conn, argv []string, jsonOut bool, out, errw io.Writer) int {
	var (
		pos  []string
		kind = "REG_SZ"
	)
	for i := 0; i < len(argv); i++ {
		if argv[i] == "--type" {
			if i+1 >= len(argv) {
				fmt.Fprintln(errw, errs.Errorf("reg", "--type 后面要跟类型名", regUsage))
				return errs.ExitUsage
			}
			kind = argv[i+1]
			i++
			continue
		}
		if strings.HasPrefix(argv[i], "--type=") {
			kind = strings.TrimPrefix(argv[i], "--type=")
			continue
		}
		pos = append(pos, argv[i])
	}
	if len(pos) != 3 {
		fmt.Fprintln(errw, errs.Errorf("reg", "set 需要 <键> <值名> <数据>", regUsage))
		return errs.ExitUsage
	}
	if !knownRegType(kind) {
		// 类型决定 Data 的形态。认不出就报错 —— 猜一个类型等于给一个错的值。
		fmt.Fprintln(errw, errs.Errorf("reg", "不认识的类型: "+kind,
			"可用: REG_SZ / REG_EXPAND_SZ / REG_DWORD / REG_QWORD / REG_MULTI_SZ / REG_BINARY"))
		return errs.ExitUsage
	}

	script := regPrelude +
		"$__wb_key = " + psQuote(pos[0]) + "\n" +
		"$__wb_name = " + psQuote(pos[1]) + "\n" +
		"$__wb_data = " + psQuote(pos[2]) + "\n" +
		"$__wb_kind = " + psQuote(kind) + "\n" +
		`$k = __wb_regopen $__wb_key $true
if ($null -eq $k) { throw "打不开也建不了注册表键: $__wb_key(确认有没有写权限)" }
$__wb_vk = switch ($__wb_kind.ToUpper()) {
    'REG_SZ'        { [Microsoft.Win32.RegistryValueKind]::String }
    'REG_EXPAND_SZ' { [Microsoft.Win32.RegistryValueKind]::ExpandString }
    'REG_DWORD'     { [Microsoft.Win32.RegistryValueKind]::DWord }
    'REG_QWORD'     { [Microsoft.Win32.RegistryValueKind]::QWord }
    'REG_MULTI_SZ'  { [Microsoft.Win32.RegistryValueKind]::MultiString }
    'REG_BINARY'    { [Microsoft.Win32.RegistryValueKind]::Binary }
}
$__wb_value = switch ($__wb_kind.ToUpper()) {
    'REG_DWORD'    { [uint32]$__wb_data }
    'REG_QWORD'    { [uint64]$__wb_data }
    'REG_MULTI_SZ' { @($__wb_data -split ',') }
    'REG_BINARY'   { [byte[]](@($__wb_data -split '[, ]') | Where-Object { $_ -ne '' } | ForEach-Object { [Convert]::ToByte($_, 16) }) }
    default        { [string]$__wb_data }
}
$k.SetValue($__wb_name, $__wb_value, $__wb_vk)
[ordered]@{ Name = $__wb_name; Type = $k.GetValueKind($__wb_name).ToString(); Data = $k.GetValue($__wb_name) }`

	records, err := runRecords(ctx, conn, script)
	if err != nil {
		fmt.Fprintln(errw, errs.Errorf("reg", stripEnginePrefix(err.Error()),
			"检查数据是否是该类型认得的写法(DWORD 要十进制数字、MULTI_SZ 用逗号分隔、BINARY 用十六进制)"))
		return errs.ExitError
	}
	if err := renderRecords(records, regValueColumns, false, jsonOut, out); err != nil {
		fmt.Fprintln(errw, errs.Errorf("reg", err.Error(), "写输出时出错"))
		return errs.ExitError
	}
	return errs.ExitOK
}

func regDel(ctx context.Context, conn *pwsh.Conn, argv []string, out, errw io.Writer) int {
	if len(argv) < 1 || len(argv) > 2 {
		fmt.Fprintln(errw, errs.Errorf("reg", "del 需要 <键> 或 <键> <值名>", regUsage))
		return errs.ExitUsage
	}
	key := argv[0]

	var script string
	if len(argv) == 2 {
		script = regPrelude +
			"$__wb_key = " + psQuote(key) + "\n" +
			"$__wb_name = " + psQuote(argv[1]) + "\n" +
			`$k = __wb_regopen $__wb_key $true
if ($null -eq $k) { throw "注册表键不存在: $__wb_key" }
if (@($k.GetValueNames()) -notcontains $__wb_name) { throw "注册表值不存在: $__wb_name" }
$k.DeleteValue($__wb_name)
$null`
	} else {
		// 无值名 = 删整个键。先确认它真的存在,而不是让 DeleteSubKeyTree 静默放过一个错键名。
		script = regPrelude +
			"$__wb_key = " + psQuote(key) + "\n" +
			`$p = __wb_regsplit $__wb_key
$root = __wb_regroot $p.Root
if ($null -eq $root.OpenSubKey($p.Sub)) { throw "注册表键不存在: $__wb_key" }
$root.DeleteSubKeyTree($p.Sub, $false)
$null`
	}

	// 删除成功时**不说话**:这是 bash 惯例(Unix 的 rm -rf 成功也没有输出),
	// 而失败会明确报错。回读一次已删的东西没有意义。
	if _, err := conn.Run(ctx, script); err != nil {
		fmt.Fprintln(errw, errs.Errorf("reg", stripEnginePrefix(err.Error()), "确认键名与值名: reg get "+key))
		return errs.ExitError
	}
	return errs.ExitOK
}

func regKeys(ctx context.Context, conn *pwsh.Conn, argv []string, jsonOut bool, out, errw io.Writer) int {
	if len(argv) != 1 {
		fmt.Fprintln(errw, errs.Errorf("reg", "keys 需要一个键", regUsage))
		return errs.ExitUsage
	}
	key := argv[0]

	script := regPrelude +
		"$__wb_key = " + psQuote(key) + "\n" +
		`$k = __wb_regopen $__wb_key $false
if ($null -eq $k) { throw "注册表键不存在: $__wb_key" }
@($k.GetSubKeyNames()) | Sort-Object | ForEach-Object { [ordered]@{ Name = $_ } }`

	records, err := runRecords(ctx, conn, script)
	if err != nil {
		fmt.Fprintln(errw, errs.Errorf("reg", stripEnginePrefix(err.Error()), "确认键名: reg get "+key))
		return errs.ExitError
	}
	if err := renderRecords(records, regKeyColumns, false, jsonOut, out); err != nil {
		fmt.Fprintln(errw, errs.Errorf("reg", err.Error(), "写输出时出错"))
		return errs.ExitError
	}
	return errs.ExitOK
}

// knownRegType 判断类型名是否是这一层支持的注册表类型。
func knownRegType(t string) bool {
	switch strings.ToUpper(t) {
	case "REG_SZ", "REG_EXPAND_SZ", "REG_DWORD", "REG_QWORD", "REG_MULTI_SZ", "REG_BINARY":
		return true
	}
	return false
}
