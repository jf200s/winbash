package winmgmt

import (
	"bytes"
	"context"
	"io"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"winbash/internal/errs"
)

// 计划里的 3 个用例照抄自 plan Task 10;后面几条是本轮补的。

func TestPsListsSelf(t *testing.T) {
	conn := startTestPwsh(t)
	var out bytes.Buffer
	if code := Ps(context.Background(), conn, nil, false, &out, io.Discard); code != 0 {
		t.Fatal(code)
	}
	if !strings.Contains(out.String(), "winbash") && !strings.Contains(out.String(), "pwsh") {
		t.Fatalf("own process family not listed: %q", firstLine(out.String()))
	}
}

func TestPsMemoryIsRawBytesNotCommaString(t *testing.T) {
	// 值保证：不得是 "86,488 K"
	conn := startTestPwsh(t)
	var out bytes.Buffer
	Ps(context.Background(), conn, []string{"pwsh"}, false, &out, io.Discard)
	s := out.String()
	if strings.Contains(s, ",") {
		t.Fatalf("thousands separator leaked: %q", s)
	}
	if regexp.MustCompile(`\bK\b`).MatchString(s) {
		t.Fatalf("unit suffix leaked: %q", s)
	}
}

func TestPsFilterByName(t *testing.T) {
	conn := startTestPwsh(t)
	var out bytes.Buffer
	Ps(context.Background(), conn, []string{"pwsh"}, false, &out, io.Discard)
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		if !strings.Contains(strings.ToLower(line), "pwsh") {
			t.Fatalf("filter leaked a row: %q", line)
		}
	}
}

// 列序固定,不得随数据变化(使用者会写死 cut -f1 拿 PID)。
func TestPsHeaderIsFixedColumns(t *testing.T) {
	conn := startTestPwsh(t)
	var out bytes.Buffer
	if code := Ps(context.Background(), conn, []string{"--header"}, false, &out, io.Discard); code != 0 {
		t.Fatal(code)
	}
	if firstLine(out.String()) != "PID\tName\tMemoryBytes" {
		t.Fatalf("header: %q", firstLine(out.String()))
	}
	for _, line := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n")[1:] {
		fields := strings.Split(line, "\t")
		if len(fields) != 3 {
			t.Fatalf("row is not 3 fields: %q", line)
		}
		if _, err := strconv.Atoi(fields[0]); err != nil {
			t.Fatalf("PID is not a plain integer: %q", fields[0])
		}
		if _, err := strconv.ParseInt(fields[2], 10, 64); err != nil {
			t.Fatalf("MemoryBytes is not a plain integer: %q", fields[2])
		}
	}
}

// 纯数字参数按 PID 精确匹配 —— 用引擎自己的 pid 验,一定能命中。
func TestPsFilterByPid(t *testing.T) {
	conn := startTestPwsh(t)
	pid := strconv.Itoa(conn.Pid())

	var out bytes.Buffer
	if code := Ps(context.Background(), conn, []string{pid}, false, &out, io.Discard); code != 0 {
		t.Fatal(code)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("want exactly the engine process, got %d rows: %q", len(lines), out.String())
	}
	if fields := strings.Split(lines[0], "\t"); fields[0] != pid {
		t.Fatalf("PID filter returned the wrong process: %q", lines[0])
	}
}

// 名字过滤不区分大小写(Windows 进程名本来就不区分)。
func TestPsNameFilterIsCaseInsensitive(t *testing.T) {
	conn := startTestPwsh(t)
	var out bytes.Buffer
	if code := Ps(context.Background(), conn, []string{"PWSH"}, false, &out, io.Discard); code != 0 {
		t.Fatal(code)
	}
	if !strings.Contains(strings.ToLower(out.String()), "pwsh") {
		t.Fatalf("uppercase filter should still match: %q", out.String())
	}
}

func TestPsUnknownFlagExitsUsage(t *testing.T) {
	conn := startTestPwsh(t)
	var errw bytes.Buffer
	code := Ps(context.Background(), conn, []string{"--frob"}, false, io.Discard, &errw)
	if code != errs.ExitUsage {
		t.Fatalf("code=%d want %d", code, errs.ExitUsage)
	}
	if !strings.Contains(errw.String(), "--frob") {
		t.Fatalf("error must name the flag: %q", errw.String())
	}
}

func TestPsTooManyArgsExitsUsage(t *testing.T) {
	conn := startTestPwsh(t)
	if code := Ps(context.Background(), conn, []string{"a", "b"}, false, io.Discard, io.Discard); code != errs.ExitUsage {
		t.Fatalf("code=%d want %d", code, errs.ExitUsage)
	}
}

// 经典 ps 旗标必须响亮拒绝,绝不落进名称过滤 —— 落进去就是把 aux/ef 当
// 进程名子串去匹配,匹配不到给空列表 + exit 0(修前的实测行为),使用方
// 会把「没有输出」读成「没有进程在跑」。每条断言三件事:退出码 2、错误
// 点名原参数、怎么办里给得出 winbash 等价写法。
func TestPsClassicFlagsAreRejectedLoudly(t *testing.T) {
	cases := []struct {
		arg    string
		remedy string // remedy 里必须出现的等价写法片段
	}{
		{"aux", "ps pwsh"},
		{"-ef", "ps pwsh"},
		{"-e", "裸写 ps"},
		{"-f", "裸写 ps"},
		{"-l", "裸写 ps"},
		{"-u", "ps pwsh"},
		{"-p", "ps 1234"},
	}
	for _, tc := range cases {
		t.Run(tc.arg, func(t *testing.T) {
			conn := startTestPwsh(t)
			var errw bytes.Buffer
			code := Ps(context.Background(), conn, []string{tc.arg}, false, io.Discard, &errw)
			if code != errs.ExitUsage {
				t.Fatalf("arg=%q code=%d want %d", tc.arg, code, errs.ExitUsage)
			}
			if !strings.Contains(errw.String(), tc.arg) {
				t.Fatalf("error must name the original arg %q: %q", tc.arg, errw.String())
			}
			if !strings.Contains(errw.String(), tc.remedy) {
				t.Fatalf("remedy must give the equivalent form %q: %q", tc.remedy, errw.String())
			}
		})
	}
}

// `-u 名字` 是经典 ps 按用户过滤的常见形态,第一个参数就该被拒,
// 后面的名字不再参与解释。
func TestPsUserFlagWithArgumentIsRejected(t *testing.T) {
	conn := startTestPwsh(t)
	var errw bytes.Buffer
	code := Ps(context.Background(), conn, []string{"-u", "pwsh"}, false, io.Discard, &errw)
	if code != errs.ExitUsage {
		t.Fatalf("code=%d want %d", code, errs.ExitUsage)
	}
	if !strings.Contains(errw.String(), "-u") {
		t.Fatalf("error must name the flag: %q", errw.String())
	}
}

// 以 - 开头但不在经典旗标名单里的参数同样响亮拒绝(照未知参数路径),
// 不静默当名字。
func TestPsDashPrefixedUnknownFlagIsRejected(t *testing.T) {
	conn := startTestPwsh(t)
	var errw bytes.Buffer
	code := Ps(context.Background(), conn, []string{"-z"}, false, io.Discard, &errw)
	if code != errs.ExitUsage {
		t.Fatalf("code=%d want %d", code, errs.ExitUsage)
	}
	if !strings.Contains(errw.String(), "-z") {
		t.Fatalf("error must name the arg: %q", errw.String())
	}
}
