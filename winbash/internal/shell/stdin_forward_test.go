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
	"sync"
	"syscall"
	"testing"
	"time"
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

// drainingReader 是「抽干已完成」可被观测的 reader。
//
// mvdan/sh 对非 *os.File 的 reader 在**构造期**就起一支 goroutine 把它读干
// (interp/stdin_os.go 的 newStdinFile)。那支 goroutine 与测试 goroutine 并发,
// 于是「先填数据还是先读干」本是个**调度竞态**:实测同一条测试在单跑时绿、
// 在全量跑(前面的测试留下调度负载)时会间歇性给出 `[late-data]` 而不是 `[]`。
//
// 一条间歇性红的门禁测试比没有测试更糟 —— 它会训练人忽略红灯。所以这里用
// EOF 事件做**确定性同步**,把「构造期已抽干」从事实断言变成被观测到的事实。
type drainingReader struct {
	buf     bytes.Buffer
	EOFSeen chan struct{}
	once    sync.Once
}

func newDrainingReader() *drainingReader {
	return &drainingReader{EOFSeen: make(chan struct{})}
}

func (r *drainingReader) Read(p []byte) (int, error) {
	n, err := r.buf.Read(p)
	if err == io.EOF {
		r.once.Do(func() { close(r.EOFSeen) })
	}
	return n, err
}

// TestStdinLateFillIsSilentlyDropped 钉住根因：构造期的空 reader 之后再填内容，
// 脚本读到的是**空**，而且**不报错** —— 这正是要修的那个 bug 的形态。
//
// 用内建 read 而不是外部 cat：interp.New 默认没有 ExecHandler，cat 会走外部进程
// 而 127（探针第一版就栽在这，测成了「找不到 cat」）。
//
// ---- 为什么不改上游去根除这个根因（2026-10-16 调研，别再重开） ----
//
// 抽干看着反直觉，但 `*os.File` 是上游的**硬约束**，理由写在它自己的源码里：
//
//	stdin_os.go:18  type stdinFile = *os.File    ← 配合 SetReadDeadline 提供可取消读
//	stdin_os.go:28 / api.go:606  「an [*os.File] is the only way to share a
//	                              reader with subprocesses」
//
// 而子进程继承那条链我们**正在用**：上游 handler.go:154 与 winbash 的
// shell.go:513 是同一句 `cmd.Stdin = hc.Stdin`。所以真把 stdinFile 改成能后填的
// bytes.Reader，代价是**③ 层外部命令（head/cat/grep/wc）全部读不到 stdin**，
// 还会失去「timeout 打断卡在 stdin 上的读」这一能力（那靠 SetReadDeadline）。
// smoke 里 `printf … | winbash -c 'head -1'` → `y` 钉的就是第一条。
//
// 顺带：我们用的 v3.14.1 已是上游最新（40 个版本里的最后一个），没有「等新版」
// 这条路。**结论：不 fork、不 replace**，`go mod verify` 干净零维护成本；我们
// 自持 pipe 对是**满足**这个约束（交出一个 *os.File 给 runner），不是绕开它。
func TestStdinLateFillIsSilentlyDropped(t *testing.T) {
	dr := newDrainingReader()
	var out bytes.Buffer
	r, err := newRunner(dr, &out)
	if err != nil {
		t.Fatal(err)
	}

	// 等抽干 goroutine 真的读到 EOF —— 这一步把竞态变成确定事实。
	// 等不到同样是有意义的信号：说明上游不再在构造期抽干，而那时该重新查的是
	// **它是否仍把 *os.File 当 stdin 载体**（见上方「为什么不改上游」），
	// 而不是「可以简化成 bytes.Reader」—— 那条路会砸掉 ③ 层的 stdin 继承。
	select {
	case <-dr.EOFSeen:
	case <-time.After(5 * time.Second):
		t.Fatal("5s 内没等到构造期抽干的 EOF —— mvdan/sh 的构造期行为变了，先查它是否仍以 " +
			"*os.File 承载 stdin（子进程继承 + 可取消读），再重审 pipe 方案；理由见上方调研注释")
	}

	dr.buf.WriteString("late-data\n") // 确定太迟：抽干已经结束，EOFSeen 已关闭
	if err := r.Run(context.Background(), parseProbe(t, `read line; printf '[%s]' "$line"`)); err != nil {
		t.Fatal(err)
	}
	if out.String() != "[]" {
		t.Fatalf("构造期抽干后填入的数据仍被读到：期望 `[]`（read 立刻 EOF、$line 为空），得到 %q", out.String())
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

// TestStdinClearedAfterUse 钉住一条刚踩过的回归:用过 stdin 之后,下一条**不带**
// stdin 的命令必须仍然正常(立即 EOF),而不是读到上一次那个已关闭的 fd。
//
// mvdan/sh 的 runner 会**持有**上次 StdIO 设进去的 fd,所以 closeStdin 关掉之后
// 必须换成 /dev/null —— 只关不换的话,下一条命令会报「file already closed」。
// 实测端到端栽过:第 1 条命令带 stdin、第 4 条不带,第 4 条才炸,中间还隔着
// 几条好命令,查起来非常费人。
func TestStdinClearedAfterUse(t *testing.T) {
	s := NewSession()
	defer s.Close()

	if got := runWithStdin(t, s, "first\n"); got != "first\n" {
		t.Fatalf("第 1 条 stdin 未到达:out=%q", got)
	}

	// 第 2 条不设 stdin —— 必须是空输入、立即 EOF、exit 0。
	out, exit, errs := s.Run(context.Background(), "cat", false)
	if exit != 0 {
		t.Fatalf("上一条用过 stdin 后,不带 stdin 的命令失败:exit=%d errs=%v", exit, errs)
	}
	if out != "" {
		t.Fatalf("不带 stdin 的命令读到了上一次的内容:out=%q", out)
	}
}

// TestStdinClearedAcrossManyRuns 是上面那条的放大版:交替「带/不带」跑 100 次,
// 钉住「每次都回到干净的空输入」这件事,而不只是第一条之后那一次。
func TestStdinClearedAcrossManyRuns(t *testing.T) {
	s := NewSession()
	defer s.Close()
	ctx := context.Background()

	for i := 0; i < 100; i++ {
		if got := runWithStdin(t, s, "x\n"); got != "x\n" {
			t.Fatalf("第 %d 轮:stdin 未到达 out=%q", i, got)
		}
		if out, exit, errs := s.Run(ctx, "cat", false); exit != 0 || out != "" {
			t.Fatalf("第 %d 轮:不带 stdin 的命令未回到空输入 out=%q exit=%d errs=%v", i, out, exit, errs)
		}
	}
}

// TestStdinPipeCallerFeeds 钉住流式入口:写端交给调用方自己喂,内容同样到达
// runner;Run 结束后读端回到 /dev/null —— 与一次性入口共享同一份收尾。
func TestStdinPipeCallerFeeds(t *testing.T) {
	s := NewSession()
	defer s.Close()

	pw, err := s.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_, _ = pw.Write([]byte("streamed\n"))
		_ = pw.Close()
	}()

	out, exit, errs := s.Run(context.Background(), "cat", false)
	if exit != 0 {
		t.Fatalf("流式 cat exit=%d errs=%v", exit, errs)
	}
	if out != "streamed\n" {
		t.Fatalf("流式内容未到达:out=%q", out)
	}

	// Run 之后必须回到空输入,否则下一条不带 stdin 的命令会炸
	// (file already closed) —— 这条与一次性入口是同一份收尾逻辑,但要独立钉住。
	out2, exit2, errs2 := s.Run(context.Background(), "cat", false)
	if exit2 != 0 || out2 != "" {
		t.Fatalf("流式之后未回到空输入:out=%q exit=%d errs=%v", out2, exit2, errs2)
	}
}

// TestStdinPipeSessionClosesForgottenWriteEnd 钉住写端兜底:调用方**漏关**写端时,
// Run 的 defer 必须替它关掉(fd 兜底),否则写端泄漏会一直撑着管道。
//
// 为什么这条能测而不挂死:用 `read` 而不是 `cat` —— read 读到**一行**就返回,
// 不等 EOF,所以 Run 会正常结束,漏关的写端才轮到 closeStdin 去收。
func TestStdinPipeSessionClosesForgottenWriteEnd(t *testing.T) {
	s := NewSession()
	defer s.Close()

	pw, err := s.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pw.Write([]byte("hi\n")); err != nil {
		t.Fatal(err)
	}
	// 故意不 pw.Close()

	out, exit, errs := s.Run(context.Background(), "read line; echo \"got:$line\"", false)
	if exit != 0 {
		t.Fatalf("read exit=%d errs=%v", exit, errs)
	}
	if strings.TrimSpace(out) != "got:hi" {
		t.Fatalf("流式内容未到达 read:out=%q", out)
	}

	// closeStdin 必须已兜底关掉写端,并把读端换回 /dev/null。
	if s.stdinW != nil {
		t.Fatal("漏关的写端没有被兜底关闭(closeStdin 未关 stdinW)")
	}
	if out2, exit2, _ := s.Run(context.Background(), "cat", false); exit2 != 0 || out2 != "" {
		t.Fatalf("兜底之后仍有残留:out=%q exit=%d", out2, exit2)
	}
}

// TestStdinPipeDoesNotLeakHandles 钉住资源面:每请求关掉读端就不会累积句柄。
// 阈值给得宽(300 次允许 20 个),因为 GetProcessHandleCount 会被无关线程扰动;
// 真回归(改成每请求新建 pipe 却不关)会是几十到几百的量级,一定穿阈值。
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
