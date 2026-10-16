package shell

// stdin 承载方式的回归网（fix/stdin-silent）。
//
// 它钉的是 mvdan/sh 的一个**非直觉行为**：`interp.StdIO` 对非 `*os.File` 的 reader
// 在**构造期**就 `os.Pipe()` + 起 goroutine 一次性抽干（interp/stdin_os.go 的
// newStdinFile）。而 Session 是常驻的、`StdIO` 是构造期 RunnerOption —— 于是
// 「构造期给个空 reader、每请求前再填内容」必然静默失效：抽干那支 goroutine 早就
// 读到 EOF 并 `pw.Close()` 了，之后往里塞什么都进不去。
//
// 2026-10-16 实测（N=300，GetProcessHandleCount 前后差）：
//
//	方案①  pipe 读端 + 每次关读端   delta = -26   ← 采用，确定性释放
//	方案①  pipe 读端 + 不关读端     delta = +18
//	方案②  memoryReader 重 apply    delta = +46，强制 GC 后归 0（延迟回收，非真泄漏）
//
// 方案②能用但赌 GC 时机，daemon 长驻 30 分钟仍有累积窗口，故不取。
//
// 300KB 大输入必须 goroutine 写 + Run 并发，否则写端会卡在 os.Pipe 的
// 内核 buffer（默认 64KB）上死锁 —— 下面那条测试就是钉这个时序。

import (
	"bytes"
	"context"
	"io"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"unsafe"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

var (
	kernel32        = syscall.NewLazyDLL("kernel32.dll")
	procHandleCount = kernel32.NewProc("GetProcessHandleCount")
)

// handleCount 取当前进程的句柄数，用来量 fd 泄漏。
func handleCount() uint32 {
	ph, _ := syscall.GetCurrentProcess()
	var n uint32
	_, _, _ = procHandleCount.Call(uintptr(ph), uintptr(unsafe.Pointer(&n)))
	return n
}

// parseProbe 把一段脚本解析成语法树（探针/回归测试共用）。
func parseProbe(t *testing.T, src string) *syntax.File {
	t.Helper()
	f, err := syntax.NewParser().Parse(strings.NewReader(src), "")
	if err != nil {
		t.Fatalf("parse %q: %v", src, err)
	}
	return f
}

// newRunner 造一个不带 ExecHandler 的裸 runner —— 只用来验 mvdan/sh 的 stdin 语义，
// 不能跑会派生外部进程的命令（那会 127）。
func newRunner(stdin io.Reader, stdout io.Writer) (*interp.Runner, error) {
	return interp.New(interp.StdIO(stdin, stdout, io.Discard))
}

// runWithStdin 走「方案①」跑一条 `cat`，返回它的 stdout。
// 时序在实现内部解决（goroutine 写 + Run 并发），这里只管喂字节；
// 300KB 那条测试正是钉住「若实现改成同步写就会卡在 64KB 内核 buffer 上」。
func runWithStdin(t *testing.T, s *Session, content string) string {
	t.Helper()
	s.SetStdinBytes([]byte(content))
	out, exit, errs := s.Run(context.Background(), "cat", false)
	if exit != 0 {
		t.Fatalf("cat exit=%d errs=%v", exit, errs)
	}
	return out
}

// TestStdinLateFillIsSilentlyDropped 钉住根因：构造期的空 reader 之后再填内容，
// 脚本读到的是**空**，而且**不报错** —— 这正是要修的那个 bug 的形态。
//
// 用内建 read 而不是外部 cat：interp.New 默认没有 ExecHandler，cat 会走外部进程
// 而 127（探针第一版就栽在这，测成了「找不到 cat」）。
func TestStdinLateFillIsSilentlyDropped(t *testing.T) {
	buf := new(bytes.Buffer)
	var out bytes.Buffer
	r, err := newRunner(buf, &out)
	if err != nil {
		t.Fatal(err)
	}
	buf.WriteString("late-data\n") // 构造之后才填 —— 太迟了
	if err := r.Run(context.Background(), parseProbe(t, `read line; printf '[%s]' "$line"`)); err != nil {
		t.Fatal(err)
	}
	if out.String() != "[]" {
		t.Fatalf("mvdan/sh 的构造期行为变了？期望 `[]`（read 立刻 EOF、$line 为空），得到 %q。"+
			"若上游已改为支持后填 stdin，本文件的承载方案可以简化", out.String())
	}
}

// TestStdinPipeDeliverContent 钉住采用的方案：给 Session 一个 *os.File（管道读端）
// 之后，内容能真正到达 runner。
func TestStdinPipeDeliverContent(t *testing.T) {
	s := NewSession()
	defer s.Close()

	if got := runWithStdin(t, s, "hello-from-stdin\n"); got != "hello-from-stdin\n" {
		t.Fatalf("stdin 未到达 runner：out=%q", got)
	}
}

// TestStdinPipeKeepsSessionState 钉住：换 stdin 不会丢掉常驻会话的状态。
// 这条若红，说明承载方案伤了「变量 / set -e 跨命令持久」，那比 stdin 本身更严重。
func TestStdinPipeKeepsSessionState(t *testing.T) {
	s := NewSession()
	defer s.Close()
	ctx := context.Background()

	if _, exit, errs := s.Run(ctx, "X=hello", false); exit != 0 {
		t.Fatalf("set X: exit=%d errs=%v", exit, errs)
	}
	runWithStdin(t, s, "data\n") // 中间插一次 stdin 请求，模拟真实请求序列

	if out, _, _ := s.Run(ctx, "echo $X", false); strings.TrimSpace(out) != "hello" {
		t.Fatalf("会话变量丢失：echo $X = %q，期望 hello", out)
	}
	if _, exit, _ := s.Run(ctx, "set -e", false); exit != 0 {
		t.Fatalf("set -e 执行失败：exit=%d", exit)
	}
	if _, exit, _ := s.Run(ctx, "false", false); exit == 0 {
		t.Fatalf("set -e 未跨命令持久：false 的 exit=%d", exit)
	}
}

// TestStdinPipeLargeInput 钉住时序：300KB 输入下不能死锁、内容必须完整。
func TestStdinPipeLargeInput(t *testing.T) {
	s := NewSession()
	defer s.Close()

	big := strings.Repeat("x", 300*1024-1) + "\n"
	if got := runWithStdin(t, s, big); len(got) != len(big) {
		t.Fatalf("大输入长度不符：got %d want %d", len(got), len(big))
	}
}

// TestStdinPipeDoesNotLeakHandles 钉住资源面：每请求关掉读端就不会累积句柄。
// 阈值给得宽（300 次允许 20 个），因为 GetProcessHandleCount 会被无关线程扰动；
// 真回归（改成每请求新建 pipe 却不关）会是几十到几百的量级，一定穿阈值。
func TestStdinPipeDoesNotLeakHandles(t *testing.T) {
	s := NewSession()
	defer s.Close()

	const n = 300
	for i := 0; i < 10; i++ { // warmup，把首批分配的抖动排掉
		runWithStdin(t, s, "x\n")
	}
	runtime.GC()
	before := handleCount()
	for i := 0; i < n; i++ {
		runWithStdin(t, s, "x\n")
	}
	after := handleCount()
	if d := int(after) - int(before); d > 20 {
		t.Fatalf("stdin 句柄疑似泄漏：N=%d delta=%d", n, d)
	}
}
