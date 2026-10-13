package winmgmt

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"testing"

	"winbash/internal/errs"
)

// 计划里的 3 个用例照抄自 plan Task 11;后面几条是本轮补的。

func TestSysOverviewIncludesOsAndMemory(t *testing.T) {
	conn := startTestPwsh(t)
	var out bytes.Buffer
	if code := Sys(context.Background(), conn, nil, false, &out, io.Discard); code != 0 {
		t.Fatal(code)
	}
	s := strings.ToLower(out.String())
	for _, want := range []string{"os", "mem", "disk"} {
		if !strings.Contains(s, want) {
			t.Fatalf("overview missing %q: %q", want, out.String())
		}
	}
}

func TestSysDiskReportsRawByteSizes(t *testing.T) {
	conn := startTestPwsh(t)
	var out bytes.Buffer
	Sys(context.Background(), conn, []string{"disk"}, false, &out, io.Discard)
	if strings.Contains(out.String(), "GB") || strings.Contains(out.String(), "MB") {
		t.Fatalf("human unit leaked into default output: %q", out.String())
	}
}

func TestSysJSONIsTyped(t *testing.T) {
	conn := startTestPwsh(t)
	var out bytes.Buffer
	Sys(context.Background(), conn, []string{"mem"}, true, &out, io.Discard)
	var items []map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &items); err != nil {
		t.Fatalf("not JSON: %v (%q)", err, out.String())
	}
}

// mem 的容量必须是**字节数**。CIM 给的 TotalVisibleMemorySize 单位是 KB ——
// 直接透传就等于把「86,488 K」换个写法继续交给使用者去猜。
func TestSysMemCapacitiesAreBytesNotKilobytes(t *testing.T) {
	conn := startTestPwsh(t)
	var out bytes.Buffer
	if code := Sys(context.Background(), conn, []string{"mem"}, true, &out, io.Discard); code != 0 {
		t.Fatal(code)
	}
	var items []map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &items); err != nil {
		t.Fatal(err)
	}
	var total float64
	for _, it := range items {
		if it["Field"] == "TotalBytes" {
			total, _ = it["Value"].(float64)
		}
	}
	// 本机内存必然大于 1 GiB;若还停在 KB 量级,说明没乘 1024。
	if total < 1<<30 {
		t.Fatalf("TotalBytes looks like KB, not bytes: %v", total)
	}
}

// 概览是三列(Section/Field/Value),这样 grep/cut 都能用。
func TestSysOverviewHasFixedThreeColumns(t *testing.T) {
	conn := startTestPwsh(t)
	var out bytes.Buffer
	if code := Sys(context.Background(), conn, nil, false, &out, io.Discard); code != 0 {
		t.Fatal(code)
	}
	rows := 0
	for _, line := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		rows++
		if fields := strings.Split(line, "\t"); len(fields) != 3 {
			t.Fatalf("row is not 3 fields: %q", line)
		}
	}
	if rows == 0 {
		t.Fatal("overview is empty")
	}
}

// disk 固定列序 Device / TotalBytes / FreeBytes,两个容量都是纯数字。
func TestSysDiskHasFixedColumns(t *testing.T) {
	conn := startTestPwsh(t)
	var out bytes.Buffer
	if code := Sys(context.Background(), conn, []string{"disk", "--header"}, false, &out, io.Discard); code != 0 {
		t.Fatal(code)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if lines[0] != "Device\tTotalBytes\tFreeBytes" {
		t.Fatalf("header: %q", lines[0])
	}
	if len(lines) < 2 {
		t.Fatal("no volumes listed")
	}
	for _, l := range lines[1:] {
		fields := strings.Split(l, "\t")
		if len(fields) != 3 {
			t.Fatalf("row is not 3 fields: %q", l)
		}
		if _, err := strconv.ParseInt(fields[1], 10, 64); err != nil {
			t.Fatalf("TotalBytes is not an integer: %q", l)
		}
		if _, err := strconv.ParseInt(fields[2], 10, 64); err != nil {
			t.Fatalf("FreeBytes is not an integer: %q", l)
		}
	}
}

func TestSysUnknownSubcommandExitsUsage(t *testing.T) {
	conn := startTestPwsh(t)
	var errw bytes.Buffer
	code := Sys(context.Background(), conn, []string{"frobnicate"}, false, io.Discard, &errw)
	if code != errs.ExitUsage {
		t.Fatalf("code=%d want %d", code, errs.ExitUsage)
	}
	if !strings.Contains(errw.String(), "frobnicate") {
		t.Fatalf("error must name the subcommand: %q", errw.String())
	}
}

// 概览的 Value 必须是干净的值,不能是「字符串化的表达式」。
// 实测踩过:在 PowerShell 的**命令参数模式**里写 `__wb_pair 'os' 'Caption' [string]$o.Caption`,
// 得到的 Value 是 `[string]Win32_OperatingSystem: Microsoft Windows 11 专业版.Caption` ——
// 命令模式与表达式模式对 cast 的解析不是一回事,漏了括号就会把这种垃圾当值发出去。
func TestSysOverviewValuesAreNotStringifiedCasts(t *testing.T) {
	conn := startTestPwsh(t)
	var out bytes.Buffer
	if code := Sys(context.Background(), conn, nil, false, &out, io.Discard); code != 0 {
		t.Fatal(code)
	}
	for _, line := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 3 {
			t.Fatalf("row is not 3 fields: %q", line)
		}
		v := fields[2]
		if strings.Contains(v, "[string]") || strings.Contains(v, "[int") || strings.Contains(v, "Win32_") {
			t.Fatalf("value looks like a stringified cast expression: %q", line)
		}
	}
}
