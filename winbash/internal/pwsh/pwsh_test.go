package pwsh

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
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

// Pid 是给 --status 与排错用的:必须暴露 pwsh 子进程的真实 pid,而不是 0。
func TestPwshPidIsTheChildProcessPid(t *testing.T) {
	c := startConn(t)
	if got := c.Pid(); got <= 0 {
		t.Fatalf("Pid() = %d, want the child's pid", got)
	}
}

// Calls 是状态展示的计数来源。Start 的握手 ping 走同一条 call 路径,
// 也算一次请求 —— 所以新会话从 1 起步,每次 Run 再 +1。这是账本的真实口径,
// 在这里钉死,免得有人按「只数业务调用」的直觉误改断言。
func TestPwshCallsCountsHandshakeAndEveryRequest(t *testing.T) {
	c := startConn(t)
	if got := c.Calls(); got != 1 {
		t.Fatalf("fresh session Calls() = %d, want 1 (the handshake ping)", got)
	}
	if _, err := c.Run(context.Background(), "1"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Run(context.Background(), "2"); err != nil {
		t.Fatal(err)
	}
	if got := c.Calls(); got != 3 {
		t.Fatalf("Calls() = %d, want 3 (handshake + 2 runs)", got)
	}
}

// 失效是单向的:活着的会话 Dead 必须是 false;Close 之后必须变成 true,
// 让调用方(② 层的 psEngine)能据此重建而不是接着用。
func TestPwshDeadFlipsOnlyAfterClose(t *testing.T) {
	c := startConn(t)
	if c.Dead() {
		t.Fatal("fresh session reported dead")
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !c.Dead() {
		t.Fatal("closed session still reported alive")
	}
}

// 协议是 4 字节小端长度 + 载荷(包注释与 pwshx 共用的约定)。把字节布局直接钉住:
// 端序搞反或改格式会在这里被抓住,而不是等到跨进程才失步。
func TestWireFrameIsLittleEndianLengthPlusBody(t *testing.T) {
	var buf bytes.Buffer
	if err := writeFrame(&buf, []byte("hi")); err != nil {
		t.Fatal(err)
	}
	got := buf.Bytes()
	if len(got) != 6 {
		t.Fatalf("frame is %d bytes, want 4-byte header + 2-byte body", len(got))
	}
	if got[0] != 2 || got[1] != 0 || got[2] != 0 || got[3] != 0 {
		t.Fatalf("length prefix is not little-endian 2: %v", got[:4])
	}
	body, err := readFrame(bufio.NewReader(&buf))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "hi" {
		t.Fatalf("readFrame recovered %q, want %q", body, "hi")
	}
}

// 0 长度或超上限的帧长说明对端已经失步(脚本往主机流写了东西),必须报错 ——
// 绝不能把垃圾字节当 JSON 解析。长度虚报(要 10 字节只来 3 字节)同理,
// 且按 io.ReadFull 的语义应得到 ErrUnexpectedEOF。
func TestReadFrameRejectsGarbageFrames(t *testing.T) {
	frame := func(n uint32, body ...byte) *bytes.Buffer {
		var b bytes.Buffer
		var hdr [4]byte
		binary.LittleEndian.PutUint32(hdr[:], n)
		b.Write(hdr[:])
		b.Write(body)
		return &b
	}
	if _, err := readFrame(bufio.NewReader(frame(0))); err == nil {
		t.Fatal("zero-length frame must be rejected")
	}
	if _, err := readFrame(bufio.NewReader(frame(maxFrameBytes + 1))); err == nil {
		t.Fatal("over-limit frame must be rejected")
	}
	if _, err := readFrame(bufio.NewReader(frame(10, 'a', 'b', 'c'))); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated frame must be ErrUnexpectedEOF, got %v", err)
	}
}

// tailBuffer 的存在理由:留排错线索但不吃内存 —— 超过 limit 的旧内容必须被丢掉,
// 只留最后 limit 字节。hint 把多行 stderr 折成一行附在错误信息里;
// 没有内容时必须给空串,不能往错误里塞无意义的空提示。
func TestTailBufferKeepsOnlyLastBytesAndHintJoinsLines(t *testing.T) {
	tail := &tailBuffer{limit: 8}
	if _, err := tail.Write([]byte("abcdefghij")); err != nil {
		t.Fatal(err)
	}
	if got := tail.buf.String(); got != "cdefghij" {
		t.Fatalf("kept %q, want the last 8 bytes %q", got, "cdefghij")
	}

	if hint := (&tailBuffer{limit: 8}).hint(); hint != "" {
		t.Fatalf("empty stderr must not produce a hint, got %q", hint)
	}

	full := &tailBuffer{limit: stderrKeep}
	if _, err := full.Write([]byte("line one\nline two\n")); err != nil {
		t.Fatal(err)
	}
	hint := full.hint()
	if !strings.HasPrefix(hint, "\n         pwsh stderr: ") {
		t.Fatalf("hint must be prefixed for error-message appending, got %q", hint)
	}
	if !strings.Contains(hint, "line one | line two") {
		t.Fatalf("hint must join stderr lines with \" | \", got %q", hint)
	}
}

// 「找不到解释器」是 Windows 上最典型的翻车形态:PATH 里没有 pwsh 时,
// 必须按候选路径找到本机安装;全都落空才退回裸名字 "pwsh.exe"(让 exec 报自己的错)。
func TestFindPwshFindsKnownInstallsWhenPathMisses(t *testing.T) {
	t.Setenv("PATH", "")
	p := findPwsh()
	if p == "" {
		t.Fatal("findPwsh returned an empty path")
	}
	if _, err := os.Stat(p); err != nil && p != "pwsh.exe" {
		t.Fatalf("findPwsh returned %q, which is neither an existing file nor the fallback name", p)
	}
}
