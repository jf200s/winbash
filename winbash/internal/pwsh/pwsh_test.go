package pwsh

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// 计划里的 3 个用例照抄自 plan Task 7;后面几条是本轮补的,
// 补的理由写在各自注释里。

func startConn(t *testing.T) *Conn {
	t.Helper()
	c, err := Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestPwshRunReturnsJson(t *testing.T) {
	c := startConn(t)
	out, err := c.Run(context.Background(), "42")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "42" {
		t.Fatalf("got %q", out)
	}
}

func TestPwshRunKeepsSessionState(t *testing.T) {
	c := startConn(t)
	if _, err := c.Run(context.Background(), "$x = 41"); err != nil {
		t.Fatal(err)
	}
	out, err := c.Run(context.Background(), "$x + 1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "42" {
		t.Fatalf("state lost across calls: %q", out)
	}
}

func TestPwshRunReportsPowerShellErrorWithoutZeroExit(t *testing.T) {
	// spec 第 7 节：修掉 non-terminating error 仍返回 0
	c := startConn(t)
	_, err := c.Run(context.Background(), "throw 'boom'")
	if err == nil {
		t.Fatal("expected an error for a throwing script")
	}
}

// non-terminating error 才是 PowerShell 最危险的形态:默认不改退出码,
// 上层会以为成功了。这条钉住「它必须变成 error」。
func TestPwshNonTerminatingErrorBecomesError(t *testing.T) {
	c := startConn(t)
	_, err := c.Run(context.Background(), "Get-Item 'C:\\definitely-not-here-winbash-xyz'")
	if err == nil {
		t.Fatal("non-terminating error slipped through as success")
	}
	if !strings.Contains(err.Error(), "definitely-not-here-winbash-xyz") {
		t.Fatalf("error should name the offending path: %v", err)
	}
}

// 同一条错误只能报一次。实测 `Get-Service -Name 不存在 -ErrorAction Stop` 的终止错误
// 会同时出现在 catch 与 $Error 里,不去重就会报两遍 —— 使用者会以为有两处错误。
func TestPwshTerminatingErrorReportedOnce(t *testing.T) {
	c := startConn(t)
	_, err := c.Run(context.Background(), "Get-Service -Name definitely-not-a-service-xyz -ErrorAction Stop")
	if err == nil {
		t.Fatal("expected an error")
	}
	if n := strings.Count(err.Error(), "definitely-not-a-service-xyz"); n != 1 {
		t.Fatalf("error message mentions the service %d times (want 1): %v", n, err)
	}
}

// ConvertTo-Json 对**单元素**数组会退化成对象,调用方拿到的形状会随数据条数变化。
// 那正是「值保证」要消灭的歧义:条数不该改变结构。
func TestPwshSingleElementIsStillJsonArray(t *testing.T) {
	c := startConn(t)
	// 「单元素数组」要活着穿过 PowerShell 的输出流,只能靠 -NoEnumerate ——
	// 默认它会把这层数组展开掉。命令里显式写出来,是因为这正是要钉的形状。
	out, err := c.Run(context.Background(), "$a = @([ordered]@{ Id = 7 }); Write-Output -NoEnumerate $a")
	if err != nil {
		t.Fatal(err)
	}

	var arr []map[string]any
	if err := json.Unmarshal([]byte(out), &arr); err != nil {
		t.Fatalf("out is not a JSON array: %v (%q)", err, out)
	}
	if len(arr) != 1 {
		t.Fatalf("want 1 record, got %d: %q", len(arr), out)
	}
	if _, ok := arr[0]["Id"]; !ok {
		t.Fatalf("record lost its Id field: %q", out)
	}

	// 多元素也必须是同一个形状。
	out, err = c.Run(context.Background(), "Get-Process | Select-Object -First 3 -Property Id")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(out), &arr); err != nil {
		t.Fatalf("multi-record out is not a JSON array: %v (%q)", err, out)
	}
	if len(arr) != 3 {
		t.Fatalf("want 3 records, got %d: %q", len(arr), out)
	}
}

// 形状由脚本的输出流决定(PowerShell 语义),引擎只保证「永远是合法 JSON」。
// 标量就是标量 —— 引擎不替调用方把它包装成数组,那会掩盖调用方的写法错误。
func TestPwshScalarStaysScalar(t *testing.T) {
	c := startConn(t)
	out, err := c.Run(context.Background(), "'hello'")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != `"hello"` {
		t.Fatalf("want a JSON string, got %q", out)
	}
}

// 值保证的另一半:任何输出不得带 ANSI 色码。pwsh 在终端里会自动上色,
// 而 winbash 的 pwsh 没有终端 —— 这条断言把「没有终端」这个前提钉住,
// 免得将来有人给它接上 ConPTY 时忘了同时关掉颜色。
func TestPwshOutputHasNoAnsiColors(t *testing.T) {
	c := startConn(t)
	out, err := c.Run(context.Background(), "Get-Process | Select-Object -First 1 -Property Id,ProcessName")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "\x1b") {
		t.Fatalf("ANSI escape leaked into the output: %q", out)
	}
}

// Close 之后 Run 必须立刻报错,不能挂起等待一个已经不在的引擎。
func TestPwshRunAfterCloseFailsInsteadOfHanging(t *testing.T) {
	c, err := Start()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := c.Run(context.Background(), "42")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error after Close")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run hung after Close")
	}
}

// 超时必须被尊重,而且超时后连接要明确失效 —— 不能留着半帧继续用。
func TestPwshRunHonorsContextDeadline(t *testing.T) {
	c := startConn(t)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := c.Run(ctx, "Start-Sleep -Seconds 30"); err == nil {
		t.Fatal("expected a deadline error")
	}

	// 会话已经作废:再问一次应当立刻得到错误,而不是读到上一条命令留下的半帧。
	_, err := c.Run(context.Background(), "42")
	if err == nil {
		t.Fatal("session should be marked dead after a timeout")
	}
}
