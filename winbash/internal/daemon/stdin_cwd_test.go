// 这个文件补上 daemon 里**请求头之后**那两条路径的回归测试:stdin 流式转发、cwd 切换。
//
// 为什么单独一个文件:它们与 guard_test.go 的"攻击者视角"不同 —— 这里测的是**合法
// client 的主路径**。补之前 daemon 包对这两条路径的覆盖率是 **0%**:
//   - startStdinStream()   0.0%
//   - shellQuote()         0.0%(唯一调用点就是 cwd 那条)
// 也就是说 stdin 转发与 cwd 切换在 daemon 层**没有任何回归保护** —— 改坏了不会变红。
// 它们平时被 cmd/winbash 的 e2e 间接带到,但 e2e 慢、且失败时指不出是 daemon 哪一段坏的。

package daemon

import (
	"encoding/binary"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"winbash/internal/client"
	"winbash/internal/ipc"
)

// rawRequestWithStdin 按协议发一帧请求,紧接着发裸 stdin 字节,然后**半关写端**再读响应。
//
// 为什么必须半关(CloseWrite)而不是直接 Close:stdin 流式靠 io.Copy 读到对端 FIN 才收尾。
// 若整条连接都关掉,响应也读不回来了;只关写侧既给了 daemon 那个 FIN、又留着读响应的另一半。
// TCPConn 提供 CloseWrite,纯 net.Conn 没有 —— 所以这里断言类型。
//
// 为什么不能复用 guard_test.go 的 rawRequest:那个函数发完帧就直接读响应,
// 根本不会写 stdin 字节,于是 daemon 侧 io.Copy 一直等 EOF,只能靠 deadline 收场。
func rawRequestWithStdin(t *testing.T, ep string, req ipc.Request, stdin []byte) (ipc.Response, error) {
	t.Helper()
	addr, _ := readEndpointFile(t, ep)
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	// deadline 给得宽一点:200KB 那条要真搬完;但必须存在,否则实现退化成
	// "先读完 stdin 再 Run" 时这里会永久挂住,而 go test 的包超时提示看不出是哪条。
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))

	body, err := ipc.Encode(req)
	if err != nil {
		t.Fatal(err)
	}
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], uint32(len(body)))
	if _, err := c.Write(append(hdr[:], body...)); err != nil {
		t.Fatalf("写请求帧: %v", err)
	}

	if len(stdin) > 0 {
		if _, err := c.Write(stdin); err != nil {
			// 写失败在"实现先关读端"时会合法发生(Errno 232),不当致命错误 ——
			// 但要把已写多少告诉调用方,免得测试给出误导性的断言失败。
			t.Logf("写 stdin 中途失败(已写 %d 字节): %v", 0, err)
		}
	}

	tc, ok := c.(*net.TCPConn)
	if !ok {
		t.Fatalf("不是 *net.TCPConn(%T),无法半关", c)
	}
	if err := tc.CloseWrite(); err != nil {
		t.Fatalf("半关写端: %v", err)
	}

	raw, err := ipc.ReadMessage(c)
	if err != nil {
		return ipc.Response{}, err
	}
	return ipc.DecodeResponse(raw)
}

// ★ stdin 流式转发:请求头之后跟的裸字节必须原样到达脚本的 stdin。
//
// 这条钉的是 startStdinStream —— 修(补)之前该函数覆盖率 0%,
// 也就是说「stdin 能不能到脚本手里」在 daemon 层没有任何测试。
// 手工实测当时是通的(`echo x | winbash -c cat` 能回显),但那是 e2e 保的;
// 一旦有人在 daemon 里改动读连接的顺序,e2e 只会告诉你"某条命令输出不对"。
func TestStdinStreamIsForwardedToScript(t *testing.T) {
	ep := startDaemon(t, DefaultReadTimeout)
	_, token := readEndpointFile(t, ep)

	resp, err := rawRequestWithStdin(t, ep, ipc.Request{
		Proto:     ipc.ProtoVersion,
		Token:     token,
		Script:    "cat",
		StdinMode: ipc.StdinStream,
		StdinLen:  ipc.StdinUntilEOF,
	}, []byte("hello-stdin\n"))
	if err != nil {
		t.Fatalf("没拿到响应: %v", err)
	}
	if !resp.OK {
		t.Fatalf("stdin 转发失败: ok=false exit=%d errs=%v", resp.Exit, resp.Errors)
	}
	if got := string(resp.Out); !strings.Contains(got, "hello-stdin") {
		t.Fatalf("脚本没收到 stdin:out=%q", got)
	}
}

// ★ 超过 os.Pipe 内核缓冲(64KB)的 stdin 也必须走通。
//
// 这是本文件里最有价值的一条:实现若退化成「先把 stdin 读完,再 Run 脚本」,
// 数据一旦超过 64KB,写端就会阻塞在一个**还没有读者**的管道上 —— 结构性死锁,
// 而且是**永久**的(client 在等响应、不会关连接)。小数据(十几字节)测不出这个形态,
// 所以必须显式造一条大流。
//
// 靠 startStdinStream 的并发结构(建管道 → 起 goroutine 边读边写 → 立刻返回去 Run)
// 才不会死锁。这段注释就是那份设计的验收条件。
func TestStdinStreamSurvivesMoreThanPipeBuffer(t *testing.T) {
	const size = 200 * 1024 // 200KB,远超 os.Pipe 的 64KB
	ep := startDaemon(t, DefaultReadTimeout)
	_, token := readEndpointFile(t, ep)

	payload := make([]byte, size)
	for i := range payload {
		payload[i] = 'x'
	}

	resp, err := rawRequestWithStdin(t, ep, ipc.Request{
		Proto:     ipc.ProtoVersion,
		Token:     token,
		Script:    "wc -c",
		StdinMode: ipc.StdinStream,
		StdinLen:  ipc.StdinUntilEOF,
	}, payload)
	if err != nil {
		t.Fatalf("没拿到响应(可能死锁后被我方 deadline 打断): %v", err)
	}
	if !resp.OK {
		t.Fatalf("大流量 stdin 失败: exit=%d errs=%v", resp.Exit, resp.Errors)
	}
	// wc -c 的输出会带前导空格(不同实现的填充不同),所以比对数字而不是整行。
	want := strconv.Itoa(size)
	if got := strings.TrimSpace(string(resp.Out)); got != want {
		t.Fatalf("脚本收到的字节数不对: got %q want %q(%d KiB) —— "+
			"若明显偏小,说明 stdin 被截断(写端提前关闭或读端只读了一部分)",
			got, want, size/1024)
	}
}

// ★ 没有 stdin 时,读 stdin 的命令必须立刻拿到 EOF,而不是挂住。
//
// 这条防的是把 StdinNone 也错当成"要读流"——那会让 `winbash -c cat` 卡到超时,
// 而使用者的体感是"winbash 挂了"。真 bash 的 `bash -c cat` 是立刻返回的。
func TestStdinNoneGivesImmediateEOF(t *testing.T) {
	ep := startDaemon(t, DefaultReadTimeout)
	_, token := readEndpointFile(t, ep)

	for _, mode := range []string{"", ipc.StdinNone} {
		resp, err := rawRequest(t, ep, ipc.Request{
			Proto:     ipc.ProtoVersion,
			Token:     token,
			Script:    "cat; echo '[eof]'",
			StdinMode: mode,
		})
		if err != nil {
			t.Fatalf("mode=%q 没拿到响应(说明挂住了): %v", mode, err)
		}
		if !resp.OK {
			t.Fatalf("mode=%q: exit=%d errs=%v", mode, resp.Exit, resp.Errors)
		}
		if got := string(resp.Out); !strings.Contains(got, "[eof]") {
			t.Fatalf("mode=%q: cat 之后没走到 echo,out=%q", mode, got)
		}
	}
}

// ★ 同一 daemon 上**先流式 stdin、后无 stdin** 的序列回归。
//
// 单请求测试各管各的:TestStdinStreamIsForwardedToScript 与
// TestStdinNoneGivesImmediateEOF 各自 startDaemon 起独立 daemon,从未在同一条
// 会话上先 stream 再 none。而 stdin 的收尾时序恰恰要跨请求才验得出问题:
// runJob 在 Run 返回**之后**才调 stopStdin(daemon.go:821-822),stopStdin 用
// deadline 唤醒卡在 conn.Read 的喂入 goroutine、等它退出后再清 deadline
// (daemon.go:949-951)。谁要是动了这个顺序(提前 stop、不等喂入 goroutine 退出、
// 忘清 deadline),单请求测试大概率仍绿,只有同一 daemon 的第二条请求才暴露 ——
// 典型形态就是第二条的 stderr 里冒出 `read |0: file already closed` 一类的收尾
// 残留(AGENTS.md §4「端到端要专门构造同一 daemon 上先 A 后 B 的序列」点名的形态)。
//
// 所以这里在同一 ep 上连发两条:第一条喂 payload 断言内容到达;紧接着第二条
// StdinNone,断言 cat 立刻拿到 EOF、echo 正常走到、且错误里没有收尾残留。
func TestStdinStreamThenNoneOnSameDaemon(t *testing.T) {
	ep := startDaemon(t, DefaultReadTimeout)
	_, token := readEndpointFile(t, ep)

	// 第一条:流式喂入,内容必须原样到达。
	resp, err := rawRequestWithStdin(t, ep, ipc.Request{
		Proto:     ipc.ProtoVersion,
		Token:     token,
		Script:    "cat",
		StdinMode: ipc.StdinStream,
		StdinLen:  ipc.StdinUntilEOF,
	}, []byte("seq-payload\n"))
	if err != nil {
		t.Fatalf("第一条(流式)没拿到响应: %v", err)
	}
	if !resp.OK {
		t.Fatalf("第一条(流式)失败: exit=%d errs=%v", resp.Exit, resp.Errors)
	}
	if got := string(resp.Out); !strings.Contains(got, "seq-payload") {
		t.Fatalf("第一条没收到 stdin: out=%q", got)
	}

	// 第二条:**同一 ep**、无 stdin。cat 必须立刻拿到 EOF(rawRequest 的 5s
	// deadline 在挂住时会先报错),echo 必须走到,且不得带回上一条收尾的残留错误。
	resp2, err := rawRequest(t, ep, ipc.Request{
		Proto:     ipc.ProtoVersion,
		Token:     token,
		Script:    "cat; echo '[eof]'",
		StdinMode: ipc.StdinNone,
	})
	if err != nil {
		t.Fatalf("第二条(无 stdin)没拿到响应(挂住或断连): %v", err)
	}
	if !resp2.OK {
		t.Fatalf("第二条失败: exit=%d errs=%v", resp2.Exit, resp2.Errors)
	}
	if got := string(resp2.Out); !strings.Contains(got, "[eof]") {
		t.Fatalf("第二条 cat 之后没走到 echo,out=%q", got)
	}
	errText := strings.Join(resp2.Errors, " ")
	if strings.Contains(errText, "already closed") {
		t.Fatalf("第二条带回上一条收尾的残留错误(errs=%v)—— "+
			"第一条的 stdin 收尾污染了同一 daemon 上的后续请求", resp2.Errors)
	}
}

// sendRaw 发一帧请求并读响应,任何失败都以 error 返回(**不用 t.Fatalf**)。
//
// 为什么单独一个:下面那条测试要在 goroutine 里并发发请求,而 t.Fatalf 在非测试
// goroutine 里调用会 runtime.Goexit、把真正的失败原因埋掉。所以这里只返回 error,
// 由测试主体统一断言。
func sendRaw(addr, token, script string) (ipc.Response, error) {
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return ipc.Response{}, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(25 * time.Second))

	body, err := ipc.Encode(ipc.Request{
		Proto:  ipc.ProtoVersion,
		Token:  token,
		Script: script,
	})
	if err != nil {
		return ipc.Response{}, err
	}
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], uint32(len(body)))
	if _, err := c.Write(append(hdr[:], body...)); err != nil {
		return ipc.Response{}, err
	}
	raw, err := ipc.ReadMessage(c)
	if err != nil {
		return ipc.Response{}, err
	}
	return ipc.DecodeResponse(raw)
}

// ★ `--stop` 时**还排在队列里**的 job 必须得到一条明确答复,不能静默断开。
//
// 这条钉的是 serveSession 里那条「已出队但还没跑就发现正在退出」的分支与
// rejectQueued → rejectJob 那条链。补之前 daemon 包对 rejectJob 的覆盖率是 **0%** ——
// 也就是说「stop 时队列非空」这个场景**完全没测**,而它是真实场景:AI 并发往同一
// 会话发几条命令,然后 --stop。
//
// 实测行为(手工用裸 socket 确认过,不是推测):
//
//	占住 worker 的那条 → ok=true exit=0 out=""(被 ctx 取消,按既有语义报成功)
//	排队的那条        → ok=false exit=1 errs=["daemon: daemon 正在退出"]
//
// 这里只断言**排队那条**:它必须拿到那条明确的「正在退出」答复。若将来有人把
// rejectQueued 删掉,排队请求会变成"连接被直接关掉"——client 侧看到的是
// `read: EOF` 一类的传输层错误,使用者无从知道发生了什么。
func TestStopAnswersQueuedJobsExplicitly(t *testing.T) {
	ep := startDaemon(t, DefaultReadTimeout)
	addr, token := readEndpointFile(t, ep)

	type result struct {
		resp ipc.Response
		err  error
	}
	longDone := make(chan result, 1)
	queuedDone := make(chan result, 1)

	// 第一条占住这个会话的 worker(默认会话,同会话严格串行)。
	go func() {
		r, e := sendRaw(addr, token, "sleep 3")
		longDone <- result{r, e}
	}()
	// 给它一点时间真的开始跑,否则第二条可能反而抢到 worker。
	time.Sleep(400 * time.Millisecond)

	// 第二条:此刻队列里有它,但 worker 忙着。
	go func() {
		r, e := sendRaw(addr, token, "echo SHOULD-NOT-RUN")
		queuedDone <- result{r, e}
	}()
	// 等它确实进队列(submit 已把 running+1)。
	time.Sleep(400 * time.Millisecond)

	if err := client.Stop(ep); err != nil {
		t.Fatalf("--stop 失败: %v", err)
	}

	// 两条都必须**有结论**:既不能永远挂着,也不能静默断连。
	select {
	case r := <-queuedDone:
		if r.err != nil {
			t.Fatalf("排队的请求得到传输层错误(%v)—— "+
				"它应当收到一条明确的「正在退出」答复,而不是连接被直接关掉", r.err)
		}
		if r.resp.OK {
			t.Fatalf("排队的请求不该报成功:它**没有执行过**(ok=true exit=%d out=%q)",
				r.resp.Exit, r.resp.Out)
		}
		errText := strings.Join(r.resp.Errors, " ")
		if !strings.Contains(errText, "正在退出") {
			t.Fatalf("排队的请求没得到「正在退出」的答复,errs=%v", r.resp.Errors)
		}
		// 它不该被执行 —— 那是 rejectJob 存在的意义。
		if strings.Contains(string(r.resp.Out), "SHOULD-NOT-RUN") {
			t.Fatalf("排队的脚本居然被执行了:out=%q", r.resp.Out)
		}
	case <-time.After(25 * time.Second):
		t.Fatal("排队的请求在 --stop 之后 25s 仍没有结论 —— 它被静默挂住了")
	}

	select {
	case r := <-longDone:
		if r.err != nil {
			t.Fatalf("占住 worker 的那条得到传输层错误(%v)", r.err)
		}
	case <-time.After(25 * time.Second):
		t.Fatal("占住 worker 的那条在 --stop 之后 25s 仍没有结论")
	}
}

// ★ client 传来的 cwd 必须真的切过去。
//
// 修(补)之前 daemon 包对 req.Cwd 的覆盖率是 0% —— 而这条路径**每次**
// `winbash -c` 都在走(cmd/winbash 会传本进程 cwd)。它被 e2e 间接带着,
// 但 daemon 包自己的测试一条都没碰。
func TestCwdFromRequestTakesEffect(t *testing.T) {
	ep := startDaemon(t, DefaultReadTimeout)
	_, token := readEndpointFile(t, ep)

	dir := t.TempDir()
	// temp 目录在 Windows 上可能是 8.3 短名(如 TESTUS~1),而 pwd 给的是长名。
	// 两边都归一化再比,否则这条测试会因为"路径写法不同"而假红。
	wantAbs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := rawRequest(t, ep, ipc.Request{
		Proto:  ipc.ProtoVersion,
		Token:  token,
		Script: "pwd",
		Cwd:    dir,
	})
	if err != nil {
		t.Fatalf("没拿到响应: %v", err)
	}
	if !resp.OK {
		t.Fatalf("cwd 请求失败: exit=%d errs=%v", resp.Exit, resp.Errors)
	}
	got := strings.TrimSpace(string(resp.Out))
	// 只比较尾部路径段,避开盘符大小写与短名差异 —— 这条测试要钉的是
	// "切过去了",不是"路径字符串逐字相同"。
	if !strings.HasSuffix(strings.ToLower(filepath.ToSlash(got)), strings.ToLower(filepath.ToSlash(filepath.Base(wantAbs)))) {
		t.Fatalf("pwd 给 %q,期望末尾是 %q —— cwd 没切过去", got, filepath.Base(wantAbs))
	}
}

// ★ cwd 切换**失败**时必须带回一条错误行,不能静默在原目录执行。
//
// 这是本项目「不静默做错事」底线的直接验收点。修之前的实现丢掉了 cd 的退出码,
// 于是"cd 没成功"与"根本没请求切目录"在输出上**完全一样** —— 使用者以为自己在
// 目标目录里,后面所有相对路径都跑在别处。daemon 里那条 note(fmt.Sprintf(
// "client: 无法切换到 …"))就是为此加的,补上这条测试它才真的被钉住。
//
// ⚠ 这里刻意**不**断言「退出码非零」,因为当前设计不是那样,而本轮的任务是补测试、
// 不是改行为。真实语义(已在 daemon 注释里写明「失败**不致命**」):
//
//	cd 失败 → 记一条 note → **继续在原目录跑脚本** → 退出码仍取脚本的退出码
//
// 所以 `winbash -c 'echo x'` 在 cwd 不存在时会给 exit=0,但 stderr 里有
// 「cd: no such file…」与「无法切换到 …」。cmd/winbash 会把 Errors 逐行打到 stderr
// 再 return resp.Exit,也就是说:**错误可见、但退出码不反映**。
//
// 这是一处真实的取舍,已作为已知行为记进 README 的「已知差异与限制」;
// 若将来要改成 cd 失败即非零,这条测试的注释与 README 那条要一起更新。
func TestCwdFailureIsReportedNotSilent(t *testing.T) {
	ep := startDaemon(t, DefaultReadTimeout)
	_, token := readEndpointFile(t, ep)

	missing := filepath.Join(t.TempDir(), "definitely-no-such-dir")
	resp, err := rawRequest(t, ep, ipc.Request{
		Proto:  ipc.ProtoVersion,
		Token:  token,
		Script: "echo ran-anyway",
		Cwd:    missing,
	})
	if err != nil {
		t.Fatalf("没拿到响应: %v", err)
	}

	// 脚本本身仍然要跑(cd 失败不致命)—— 这是既定语义。
	if !strings.Contains(string(resp.Out), "ran-anyway") {
		t.Fatalf("cd 失败后脚本仍该继续执行,out=%q", resp.Out)
	}

	// 但**必须**有一条明确的错误行说明没切过去,并且点出目标路径。
	errText := strings.Join(resp.Errors, " ")
	if !strings.Contains(errText, "无法切换") {
		t.Fatalf("cd 失败没有报错行 —— 这就是静默错值(使用者以为在目标目录里):errs=%v", resp.Errors)
	}
	if !strings.Contains(errText, "definitely-no-such-dir") {
		t.Fatalf("报错没点出目标路径,使用者不知道该改哪个: %q", errText)
	}
	// 报错行还必须说明「已在原目录执行」,否则使用者不知道该去哪找产物。
	if !strings.Contains(errText, "原目录") {
		t.Fatalf("报错没说清楚实际在哪执行: %q", errText)
	}
}
