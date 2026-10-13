package winmgmt

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"

	"winbash/internal/errs"
	"winbash/internal/pwsh"
)

// 整个包共用一个 pwsh 引擎:起一个要 ~0.5s,而这些都是只读查询,
// 没有需要隔离的会话状态。任务 9–11 的同包测试直接复用它。
var (
	sharedConn *pwsh.Conn
	sharedErr  error
	sharedOnce sync.Once
)

func TestMain(m *testing.M) {
	code := m.Run()
	if sharedConn != nil {
		_ = sharedConn.Close()
	}
	os.Exit(code)
}

// startTestPwsh 起(或复用)一个 pwsh 引擎。
func startTestPwsh(t *testing.T) *pwsh.Conn {
	t.Helper()
	sharedOnce.Do(func() { sharedConn, sharedErr = pwsh.Start() })
	if sharedErr != nil {
		t.Fatalf("pwsh 引擎不可用: %v", sharedErr)
	}
	return sharedConn
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// 计划里的 5 个用例照抄自 plan Task 8;后面几条是本轮补的。

func TestSvcListDefaultIsTSVWithFixedColumns(t *testing.T) {
	conn := startTestPwsh(t)
	var out bytes.Buffer
	if code := Svc(context.Background(), conn, []string{"list"}, false, &out, io.Discard); code != 0 {
		t.Fatal(code)
	}
	line := firstLine(out.String())
	fields := strings.Split(line, "\t")
	if len(fields) != 3 {
		t.Fatalf("want 3 TSV fields, got %d: %q", len(fields), line)
	}
}

func TestSvcListHeaderFlagPrefixedWithColumnNames(t *testing.T) {
	conn := startTestPwsh(t)
	var out bytes.Buffer
	Svc(context.Background(), conn, []string{"list", "--header"}, false, &out, io.Discard)
	if firstLine(out.String()) != "Name\tStatus\tStartType" {
		t.Fatalf("header: %q", firstLine(out.String()))
	}
}

func TestSvcStatusReportsHumanReadableStateNotConstantCode(t *testing.T) {
	// 值保证：不能是 "STATE: 1"
	conn := startTestPwsh(t)
	var out bytes.Buffer
	Svc(context.Background(), conn, []string{"status", "W32Time"}, false, &out, io.Discard)
	s := out.String()
	if strings.Contains(s, "STATE:") || regexp.MustCompile(`^\s*1\s*$`).MatchString(s) {
		t.Fatalf("raw constant code leaked: %q", s)
	}
	if !regexp.MustCompile(`(?i)stopped|running`).MatchString(s) {
		t.Fatalf("want readable state: %q", s)
	}
}

func TestSvcJSONIsTypedArray(t *testing.T) {
	conn := startTestPwsh(t)
	var out bytes.Buffer
	Svc(context.Background(), conn, []string{"list"}, true, &out, io.Discard)
	var items []map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &items); err != nil {
		t.Fatalf("not a JSON array: %v (%q)", err, out.String())
	}
	if _, ok := items[0]["Name"]; !ok {
		t.Fatalf("missing Name key: %v", items[0])
	}
}

func TestSvcUnknownSubcommandExitsTwo(t *testing.T) {
	conn := startTestPwsh(t)
	var errw bytes.Buffer
	if code := Svc(context.Background(), conn, []string{"frobnicate"}, false, io.Discard, &errw); code != errs.ExitUsage {
		t.Fatalf("code=%d want %d", code, errs.ExitUsage)
	}
}

// StartType 出厂是枚举,ConvertTo-Json 直接给 3 —— 与 "STATE: 1" 是同一类翻车。
// 实测 `(Get-Service -Name W32Time).StartType | ConvertTo-Json` 就是 `3`。
func TestSvcStartTypeIsAWordNotARawEnumNumber(t *testing.T) {
	conn := startTestPwsh(t)
	var out bytes.Buffer
	if code := Svc(context.Background(), conn, []string{"list"}, false, &out, io.Discard); code != 0 {
		t.Fatal(code)
	}

	rows := 0
	for _, line := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 3 {
			t.Fatalf("row is not 3 fields: %q", line)
		}
		rows++
		if regexp.MustCompile(`^\d+$`).MatchString(fields[2]) {
			t.Fatalf("StartType leaked as a raw enum number: %q", line)
		}
		if !regexp.MustCompile(`(?i)^(automatic|manual|disabled|boot|system)$`).MatchString(fields[2]) {
			t.Fatalf("unexpected StartType word %q in %q", fields[2], line)
		}
		if !regexp.MustCompile(`(?i)^(running|stopped|startpending|stoppending|paused|pausepending|continuepending)$`).MatchString(fields[1]) {
			t.Fatalf("unexpected Status word %q in %q", fields[1], line)
		}
	}
	if rows == 0 {
		t.Fatal("no services listed at all")
	}
}

// 过滤必须真的生效:一个只含 Running 的列表里不该出现 Stopped。
func TestSvcListRunningFilterReturnsOnlyRunning(t *testing.T) {
	conn := startTestPwsh(t)
	var out bytes.Buffer
	if code := Svc(context.Background(), conn, []string{"list", "--running"}, false, &out, io.Discard); code != 0 {
		t.Fatal(code)
	}
	for _, line := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		if fields := strings.Split(line, "\t"); len(fields) == 3 && !strings.EqualFold(fields[1], "Running") {
			t.Fatalf("--running leaked a non-running service: %q", line)
		}
	}
}

// 不认识的 flag 必须报错(Global Constraints),而且是三行式。
func TestSvcListUnknownFlagExitsTwoWithThreeLineError(t *testing.T) {
	conn := startTestPwsh(t)
	var errw bytes.Buffer
	code := Svc(context.Background(), conn, []string{"list", "--frob"}, false, io.Discard, &errw)
	if code != errs.ExitUsage {
		t.Fatalf("code=%d want %d", code, errs.ExitUsage)
	}
	if !strings.Contains(errw.String(), "--frob") {
		t.Fatalf("error must name the flag: %q", errw.String())
	}
	if n := strings.Count(strings.TrimRight(errw.String(), "\n"), "\n"); n != 2 {
		t.Fatalf("want 3 lines, got %d: %q", n+1, errw.String())
	}
}

// 不存在的服务必须明确失败,不能静默给一行空 TSV 让人以为服务没在跑。
func TestSvcStatusForMissingServiceFailsWithActionableError(t *testing.T) {
	conn := startTestPwsh(t)
	var out, errw bytes.Buffer
	code := Svc(context.Background(), conn, []string{"status", "definitely-not-a-service-xyz"}, false, &out, &errw)
	if code == 0 {
		t.Fatal("expected non-zero for a missing service")
	}
	if out.Len() != 0 {
		t.Fatalf("a failed status must not print a data row: %q", out.String())
	}
	if !strings.Contains(errw.String(), "definitely-not-a-service-xyz") {
		t.Fatalf("error should name the service: %q", errw.String())
	}
}

// 服务名里的单引号不得让 PowerShell 脚本被改写(注入/语法错误两种都不行)。
func TestSvcStatusQuotesServiceNameSafely(t *testing.T) {
	conn := startTestPwsh(t)
	var out, errw bytes.Buffer
	code := Svc(context.Background(), conn, []string{"status", `x'; Write-Output 'INJECTED`}, false, &out, &errw)
	if code == 0 {
		t.Fatal("expected non-zero for a nonexistent service with a quote in its name")
	}
	if strings.Contains(out.String(), "INJECTED") {
		t.Fatalf("service name was interpolated into the script unsafely: %q", out.String())
	}
}
