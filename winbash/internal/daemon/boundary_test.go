// 协议边界测试:daemon 收到"不认识 / 坏掉"的请求时,必须**明确拒绝**,不能静默。
//
// 这个文件的断言全部来自手工裸 socket 实测(实测输出抄在每条注释里)。
// 与 guard_test.go 的分工:那里测"鉴权与版本不匹配"(攻击面),这里测
// "参数与帧格式坏掉"(健壮性 + 项目底线「不静默做错事」)。
//
// 底线条款(AGENTS.md §7):不认识的 flag/参数/子命令一律非零退出,不得静默忽略。
// 所以这里每条都断言 exit 与**错误信息里点了什么**,而不是只看 ok=false。

package daemon

import (
	"encoding/binary"
	"errors"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"winbash/internal/client"
	"winbash/internal/ipc"
)

// queueCapacity 是每个会话的队列容量 —— daemon.go 里 make(chan job, 64) 的那个 64。
// 端到端测试看不到那个 chan,所以在这里复述一遍,并把"两边要一致"写进断言:
// 错误信息里也必须说 64,对不上测试就会红。
const queueCapacity = 64

// sendRawBytes 发一帧**任意字节**(不经 ipc.Encode),用于制造坏帧/未知取值。
//
// 返回 (响应, 是否拿到了响应)。没拿到响应时 err 非 nil —— 有些坏帧是有意
// 不回响应的(见 TestBadFrameDoesNotWedgeDaemon),所以这里不能直接 Fatalf。
func sendRawBytes(ep string, payload []byte) (ipc.Response, error) {
	addr, _ := readEndpointFileNoFatal(ep)
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return ipc.Response{}, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))

	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], uint32(len(payload)))
	if _, err := c.Write(append(hdr[:], payload...)); err != nil {
		return ipc.Response{}, err
	}
	raw, err := ipc.ReadMessage(c)
	if err != nil {
		return ipc.Response{}, err
	}
	return ipc.DecodeResponse(raw)
}

// readEndpointFileNoFatal 与 readEndpointFile 相同,但返回 error 而不是 Fatalf ——
// 上面的 helper 要能在子 goroutine / 非测试主体里用。
func readEndpointFileNoFatal(ep string) (addr, token string) {
	b, err := os.ReadFile(ep)
	if err != nil {
		return "", ""
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) > 0 {
		addr = strings.TrimSpace(lines[0])
	}
	if len(lines) > 1 {
		token = strings.TrimSpace(lines[1])
	}
	return addr, token
}

// ★ 不认识的 `stdin_mode` 必须报错,**不能**当成「没有 stdin」。
//
// 当成没有 stdin 的后果是静默丢输入:脚本看到立即 EOF、退出码 0,调用方无从分辨
// "输入本来就是空的"和"输入被丢了"。
//
// 实测(裸 socket 发 stdin_mode="bogus"):
//
//	{"ok":false,"errors":["daemon: 不认识的 stdin_mode \"bogus\"\n怎么办: …
//	  运行 `winbash --stop` 后重试"],"exit":2}
//
// exit=2 是用法错误(见项目退出码约定),这里必须断言它,不能只断言 ok=false。
func TestUnknownStdinModeIsRejected(t *testing.T) {
	ep := startDaemon(t, DefaultReadTimeout)
	_, token := readEndpointFile(t, ep)

	body, err := ipc.Encode(ipc.Request{
		Proto:     ipc.ProtoVersion,
		Token:     token,
		Op:        "", // 空 op = 执行脚本
		Script:    "echo SHOULD-NOT-RUN",
		StdinMode: "bogus",
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := sendRawBytes(ep, body)
	if err != nil {
		t.Fatalf("不认识的 stdin_mode 应当得到明确答复,却拿到传输层错误: %v", err)
	}

	if resp.OK {
		t.Fatalf("不认识的 stdin_mode 被放行了: %+v", resp)
	}
	if resp.Exit != 2 {
		t.Fatalf("用法错误应当 exit=2,得到 %d(errors=%v)", resp.Exit, resp.Errors)
	}
	errText := strings.Join(resp.Errors, " ")
	if !strings.Contains(errText, "不认识的 stdin_mode") {
		t.Fatalf("错误信息没点明是 stdin_mode 不认识: %v", resp.Errors)
	}
	// 必须点名那个值本身 —— 否则使用者不知道是哪一端发错了什么。
	if !strings.Contains(errText, "bogus") {
		t.Fatalf("错误信息没带上那个不认识的值: %v", resp.Errors)
	}
	// 必须指路(错误模型的硬要求)。
	if !strings.Contains(errText, "--stop") {
		t.Fatalf("错误信息没有指路: %v", resp.Errors)
	}
	// 脚本绝不能被执行。
	if strings.Contains(string(resp.Out), "SHOULD-NOT-RUN") {
		t.Fatalf("被拒的请求居然执行了: out=%q", resp.Out)
	}
}

// ★ 坏帧(不是合法 JSON)**不拖垮 daemon**。
//
// 实测:daemon 对坏帧的处置是 **closeConnGracefully 且不回任何响应** ——
// client 侧看到的是 EOF,而不是一条明确的错误。这是一个已知取舍:
// 坏帧意味着对端根本不是在讲这个协议,给每个垃圾帧回一条"你的 JSON 坏了"
// 没有意义(而且能被人拿来放大流量)。真正的保证是"一条坏帧不能影响别人"。
//
// 所以这条测试断言的不是"有没有错误信息",而是那条**真正重要的契约**:
//  1. 这条连接必须有结论(EOF 也行),不能挂着;
//  2. daemon 必须继续服务 —— 紧接着的正常请求要成功。
func TestBadFrameDoesNotWedgeDaemon(t *testing.T) {
	ep := startDaemon(t, DefaultReadTimeout)

	// 1) 坏帧:必须很快有结论(EOF 或响应都算有结论),不能挂到超时。
	done := make(chan error, 1)
	go func() {
		_, err := sendRawBytes(ep, []byte(`{"op":"", this is not json`))
		done <- err
	}()
	select {
	case <-done:
		// 有结论就行(实测是 read: EOF)。
	case <-time.After(15 * time.Second):
		t.Fatal("坏帧让连接挂住了 —— 它必须立刻有结论")
	}

	// 2) daemon 必须还活着:紧跟一条正常请求。
	_, token := readEndpointFile(t, ep)
	body, err := ipc.Encode(ipc.Request{
		Proto:  ipc.ProtoVersion,
		Token:  token,
		Script: "echo STILL-ALIVE",
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := sendRawBytes(ep, body)
	if err != nil {
		t.Fatalf("坏帧之后 daemon 不再服务: %v", err)
	}
	if !resp.OK || !strings.Contains(string(resp.Out), "STILL-ALIVE") {
		t.Fatalf("坏帧之后 daemon 的服务异常: %+v", resp)
	}
}

// ★ 会话队列满时,client 必须收到**明确错误**,不能挂住、不能静默丢。
//
// 实测(裸 socket:一条 `sleep 6` 占住 worker,再并发发 70 条 echo):
//
//	成功 64(队列容量),被拒 6,传输层异常 **0**
//	被拒的响应:{"ok":false,"errors":["daemon: 会话 \"\" 的队列已满(每个会话最多排 64 条)。
//	  怎么办: 等前面的命令跑完,或者改用另一个会话名并发"],"exit":1}
//	sleep 结束后 daemon 仍正常(echo AFTER 成功)
//
// 为什么这条必须端到端:白盒已经覆盖了 `submit` 的队列满分支(state_test.go),
// 但**把那条错误回写给 client** 的是 handleConn 里那段(506-513),白盒碰不到。
// 而使用者实际看到的正是那一段:若它漏了回写,连接会被直接关掉,client 侧只看到
// `read: EOF` —— 与"daemon 崩了"无法区分。
//
// 为什么不 flaky:队列容量是常量 64,worker 被一条 5 秒的命令占住,70 条在毫秒级
// 内发完 —— 排满这件事是确定的,不靠时序运气。断言只要求"至少一条被拒",
// 不要求恰好 6 条(并发下具体条数会浮动,那不是契约的一部分)。
func TestQueueFullIsReportedNotSilentlyDropped(t *testing.T) {
	ep := startDaemon(t, DefaultReadTimeout)
	addr, token := readEndpointFile(t, ep)

	// 1) 占住这个会话的 worker。
	blockerDone := make(chan struct{})
	go func() {
		defer close(blockerDone)
		_, _ = sendRaw(addr, token, "sleep 5")
	}()
	time.Sleep(300 * time.Millisecond)

	// 2) 并发塞超容量。每条都用自己的连接(与真实并发调用一致)。
	const extra = 6
	type res struct {
		resp ipc.Response
		err  error
	}
	results := make(chan res, queueCapacity+extra)
	var wg sync.WaitGroup
	for i := 0; i < queueCapacity+extra; i++ {
		wg.Add(1)
		go func(ii int) {
			defer wg.Done()
			r, e := sendRaw(addr, token, "echo q"+strconv.Itoa(ii))
			results <- res{r, e}
		}(i)
	}
	wg.Wait()
	close(results)

	var accepted, rejected, transport int
	var firstRejection []string
	for r := range results {
		switch {
		case r.err != nil:
			transport++
		case r.resp.OK:
			accepted++
		default:
			rejected++
			if firstRejection == nil {
				firstRejection = r.resp.Errors
			}
		}
	}

	// 3) 核心断言:**没有一条**落到传输层错误上。
	if transport != 0 {
		t.Fatalf("%d 条请求得到传输层错误 —— 队列满时必须回一条明确错误,而不是断连",
			transport)
	}
	// 必须真的有被拒的:否则说明队列没被塞满,这条测试什么都没验到。
	if rejected == 0 {
		t.Fatalf("队列没被塞满(%d 条全成功),这条测试没验证到队列满的分支", accepted)
	}
	errText := strings.Join(firstRejection, " ")
	if !strings.Contains(errText, "队列已满") {
		t.Fatalf("被拒的请求没说明原因: %v", firstRejection)
	}
	// 必须指路,并且说清上限是多少(使用者要据此决定怎么重试)。
	if !strings.Contains(errText, "64") {
		t.Fatalf("错误信息没说清队列上限: %v", firstRejection)
	}
	if !strings.Contains(errText, "怎么办") {
		t.Fatalf("错误信息没有指路: %v", firstRejection)
	}

	// 4) 队列排空后 daemon 必须仍然正常(sleep 5 跑完即可)。
	<-blockerDone
	resp, err := sendRaw(addr, token, "echo AFTER-QUEUE-FULL")
	if err != nil {
		t.Fatalf("队列排空后 daemon 不再服务: %v", err)
	}
	if !resp.OK || !strings.Contains(string(resp.Out), "AFTER-QUEUE-FULL") {
		t.Fatalf("队列排空后 daemon 服务异常: %+v", resp)
	}
}

// ★ 具名会话数到上限时,client 必须收到**明确错误**,而不是挂住或静默。
//
// 实测(建满 16 个具名会话后建第 17 个):
//
//	rc=1  stderr="daemon: 具名会话数已达上限 16(每个会话一个独立 shell 会话,
//	  用到 ② 层时还会各持一个常驻 pwsh)。\n怎么办: 先 `winbash --stop` 清空,
//	  或者复用已有的会话名"
//
// 同时这条顺带钉住 `statusText` 的**具名会话明细行**格式:
//
//	session=n0 pwsh=not-started idle=0s
//
// ——那几行原先零覆盖(daemon 包测试从没建过具名会话再查 --status)。
func TestNamedSessionLimitIsReportedAndStatusListsSessions(t *testing.T) {
	ep := startDaemon(t, DefaultReadTimeout)
	cwd := t.TempDir()

	// 先建**默认会话**再建具名会话 —— 这一顺序让 statusText 里"跳过默认会话"
	// 那个分支(name == "")真的被执行到。同时它端到端验证了"默认会话不占名额":
	// 若它占名额,下面第 16 个具名会话就会被拒。
	//
	// 为什么值得专门排这个顺序:默认会话与具名会话并存是**最常见的真实状态**
	// (任何一次不带 --session 的调用都会建出默认会话),而 --status 必须把它
	// 归在上面的 `cwd=` 那几行里、不重复列成 session= 行。
	if _, exit, ok := run(t, ep, cwd, "echo default-session"); !ok || exit != 0 {
		t.Fatalf("默认会话不该建不起来: exit=%d", exit)
	}

	// 建满 maxSessions 个具名会话。实测总耗时约 0.4s,可以放心全建。
	for i := 0; i < maxSessions; i++ {
		name := "n" + strconv.Itoa(i)
		if _, exit, ok := run(t, ep, cwd, "echo ok", client.Session(name)); !ok || exit != 0 {
			t.Fatalf("第 %d 个具名会话(%s)不该失败: exit=%d", i+1, name, exit)
		}
	}

	// 第 maxSessions+1 个必须被明确拒绝。
	resp, err := client.RunTimeout(ep, "echo ok", cwd, false,
		freshTestTimeout, freshTestIdle, client.Session("one-too-many"))
	if err != nil {
		t.Fatalf("超限时应当拿到明确错误,却拿到传输层错误: %v", err)
	}
	if resp.OK {
		t.Fatalf("已到上限 %d 却还接受了新具名会话", maxSessions)
	}
	if resp.Exit != 1 {
		t.Fatalf("超限是运行期错误,应当 exit=1,得到 %d", resp.Exit)
	}
	errText := strings.Join(resp.Errors, " ")
	for _, want := range []string{"上限", strconv.Itoa(maxSessions), "怎么办", "--stop", "复用"} {
		if !strings.Contains(errText, want) {
			t.Fatalf("错误信息缺 %q,指路不完整: %v", want, resp.Errors)
		}
	}

	// --status 必须把具名会话逐个列出来(默认会话已在上面几行,不重复列)。
	out, err := client.Status(ep)
	if err != nil {
		t.Fatalf("--status 失败: %v", err)
	}
	listed := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "session=") {
			listed++
			if !strings.Contains(line, "idle=") {
				t.Fatalf("会话明细行缺 idle 字段(排错时要用): %q", line)
			}
			// 默认会话**不该**出现在明细行里 —— 它的 cwd/pwsh 已经在上面那几行。
			// 若出现,使用者会看到一条名字空白的 `session= pwsh=…`,纯噪音。
			if strings.HasPrefix(line, "session= ") {
				t.Fatalf("默认会话被重复列成明细行(应当只在上面的 cwd/pwsh 行里): %q", line)
			}
		}
	}
	if listed != maxSessions {
		t.Fatalf("--status 应当列出 %d 条具名会话,实际 %d 条:\n%s", maxSessions, listed, out)
	}
	// sessions= 报的是**会话总数**,默认会话也算一条 —— 所以是 maxSessions+1。
	// 这与"默认会话不占名额"不矛盾:上限管的是具名会话,而 status 报的是实际条数。
	wantTotal := maxSessions + 1
	if !strings.Contains(out, "sessions="+strconv.Itoa(wantTotal)+" ") {
		t.Fatalf("sessions= 计数不对(应当是 %d = %d 具名 + 1 默认):\n%s",
			wantTotal, maxSessions, out)
	}
}

// ★ 未鉴权的**管理命令**同样必须被拒,而且被拒之后 daemon 必须还活着。
//
// 为什么单独一条:已有 TestRequestWithoutTokenIsRejected 只覆盖了 `op:""`(执行脚本)。
// 而注释里那句「实测裸 socket 发 {"op":"stop"} 就能把别人的 daemon 打停」讲的正是
// op 分派**之前**的那道鉴权 —— 阻的是**管理能力**,不只是执行能力。这条断言若被
// 挪到 op 分派之后,执行路径的用例照样全绿,而任何人仍能把 daemon 打停。
//
// status 一并测:它只读信息,但同样是"以 daemon 的身份"回答 —— 会话名、cwd、
// 引擎状态都在里面,不该给外人看。
func TestUnauthenticatedManagementOpsAreRejected(t *testing.T) {
	ep := startDaemon(t, DefaultReadTimeout)
	_, token := readEndpointFile(t, ep)

	cases := []struct {
		name string
		req  ipc.Request
	}{
		{"无 token 的 stop", ipc.Request{Op: "stop"}},
		{"错 token 的 stop", ipc.Request{Op: "stop", Token: "not-the-real-one"}},
		{"无 token 的 status", ipc.Request{Op: "status"}},
		{"错 token 的 status", ipc.Request{Op: "status", Token: "not-the-real-one"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, err := rawRequest(t, ep, c.req)
			if err != nil {
				t.Fatalf("应当明确拒绝(而不是直接断连接,那样调用方分不清"+
					"\"鉴权失败\"和\"daemon 崩了\"): %v", err)
			}
			if resp.OK {
				t.Fatalf("OK=true,说明放行了: %+v", resp)
			}
			if !resp.Auth {
				t.Fatalf("应当标记成鉴权失败, got %+v", resp)
			}
			if len(resp.Out) > 0 {
				t.Fatalf("被拒的管理命令不该带回任何信息(那本身就是泄漏): %q", resp.Out)
			}
		})
	}

	// 最关键的一条:四条拒绝之后 daemon 必须还在服务。
	// 若 stop 真的被执行了,这里会连不上 —— 而**前四条会全部通过**,
	// 只是因为它们都"被拒绝了"。
	resp, err := rawRequest(t, ep, ipc.Request{Proto: ipc.ProtoVersion, Token: token, Script: "echo STILL-HERE"})
	if err != nil {
		t.Fatalf("鉴权被拒之后 daemon 不再服务(它多半已经被未鉴权的 stop 打停了): %v", err)
	}
	if !resp.OK || !strings.Contains(string(resp.Out), "STILL-HERE") {
		t.Fatalf("daemon 状态异常: %+v", resp)
	}
}

// ★ 帧长度边界:0 长度与超上限都必须被拒,且 daemon 不能被打死。
//
// 这两个值在 daemon.handleConn 里走的是 ipc.ReadMessage 的同一个判断
// (`n == 0 || n > maxMessageBytes`),但那是**单元**层面的。端到端要证明的是
// 另一件事:daemon 收到这种帧之后**不会**把它当成一条 0 字节或超大请求往下跑,
// 也不会因此挂住或退出。
//
// 构造时要只写帧头、不写 payload:超上限那种如果真写 64 MiB 出来,用例会白白
// 慢一大截,而 daemon 在读头之后就该拒绝。
func TestFrameLengthBoundariesAreRejected(t *testing.T) {
	ep := startDaemon(t, DefaultReadTimeout)
	addr, token := readEndpointFile(t, ep)

	for _, c := range []struct {
		name string
		n    uint32
	}{
		{"0 长度", 0},
		{"超上限(64MiB+1)", (64 << 20) + 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.Close()
			// **不设** 5s 读 deadline:那会让「daemon 挂住」伪装成「有结论」——
			// 超时也是 error,而下面的 select 只关心"有没有返回"。修之前正是这样:
			// 5s deadline 先到期,外层 10s 保护分支永远走不到,测试对挂死是假绿。
			// 现在只留外层 select 当兜底,并且断言拿到的必须是**连接被关**而不是超时。

			var hdr [4]byte
			binary.LittleEndian.PutUint32(hdr[:], c.n)
			if _, err := conn.Write(hdr[:]); err != nil {
				t.Fatalf("写帧头: %v", err)
			}

			// 必须**立刻**有结论:daemon 读完 4 字节帧头就该判定这帧不可用并关连接。
			// 判据是拿到 EOF,而不是"随便什么错误"——后者把超时也算进去,
			// 而超时恰恰是"daemon 没在拒绝、只是没反应"的形态。
			done := make(chan error, 1)
			go func() {
				_, rerr := ipc.ReadMessage(conn)
				done <- rerr
			}()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("daemon 居然回了一个响应 —— 坏帧不该被当成一条正常请求")
				}
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					t.Fatal("读等了整整 10s 才超时 —— daemon 没拒绝这个坏帧,只是没反应")
				}
			case <-time.After(10 * time.Second):
				t.Fatal("坏帧让连接挂住了:它必须立刻有结论")
			}
		})
	}

	// daemon 必须还活着,而且能正常执行。
	resp, err := rawRequest(t, ep, ipc.Request{Proto: ipc.ProtoVersion, Token: token, Script: "echo ALIVE"})
	if err != nil {
		t.Fatalf("帧长度边界之后 daemon 不再服务: %v", err)
	}
	if !resp.OK || !strings.Contains(string(resp.Out), "ALIVE") {
		t.Fatalf("帧长度边界之后 daemon 状态异常: %+v", resp)
	}
}
