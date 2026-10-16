package daemon

import (
	"encoding/binary"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"winbash/internal/client"
	"winbash/internal/ipc"
)

// readEndpointFile 取出 (addr, token)。端口文件是两行:地址与 token。
func readEndpointFile(t *testing.T, ep string) (addr, token string) {
	t.Helper()
	b, err := os.ReadFile(ep)
	if err != nil {
		t.Fatalf("读端口文件: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) == "" {
		t.Fatalf("端口文件里没有地址: %q", string(b))
	}
	addr = strings.TrimSpace(lines[0])
	if len(lines) > 1 {
		token = strings.TrimSpace(lines[1])
	}
	return addr, token
}

// rawRequest 绕过 client,自己按协议发一帧 —— 这正是攻击者会做的事。
func rawRequest(t *testing.T, ep string, req ipc.Request) (ipc.Response, error) {
	t.Helper()
	addr, _ := readEndpointFile(t, ep)
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))

	body, err := ipc.Encode(req)
	if err != nil {
		t.Fatal(err)
	}
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], uint32(len(body)))
	if _, err := c.Write(append(hdr[:], body...)); err != nil {
		t.Fatalf("write: %v", err)
	}
	raw, err := ipc.ReadMessage(c)
	if err != nil {
		return ipc.Response{}, err
	}
	return ipc.DecodeResponse(raw)
}

func startDaemon(t *testing.T, readTimeout time.Duration) string {
	t.Helper()
	ep := uniqueEndpoint(t)
	go func() { _ = Serve(ep, time.Minute, readTimeout) }()
	waitForEndpoint(t, ep, 10*time.Second)
	t.Cleanup(func() { _ = client.Stop(ep) })
	return ep
}

// ★ P0-1:不带 token 的请求绝不能被执行。
//
// 修之前:Request 里没有 token,端口文件就是明文地址 —— 任意本机进程裸 socket
// 发一帧就能以 daemon 属主身份执行任意命令(实测 whoami 返回属主名),
// 也能发 {"op":"stop"} 把别人的 daemon 打停。
func TestRequestWithoutTokenIsRejected(t *testing.T) {
	ep := startDaemon(t, DefaultReadTimeout)

	for _, tc := range []struct {
		name string
		req  ipc.Request
	}{
		{"没有 token", ipc.Request{Op: "", Script: "echo PWNED"}},
		{"token 是错的", ipc.Request{Op: "", Script: "echo PWNED", Token: "not-the-real-one"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := rawRequest(t, ep, tc.req)
			if err != nil {
				t.Fatalf("居然没得到响应(应当明确拒绝): %v", err)
			}
			if len(resp.Out) > 0 && strings.Contains(string(resp.Out), "PWNED") {
				t.Fatalf("请求被执行了! out=%q", resp.Out)
			}
			if resp.OK {
				t.Fatalf("OK=true,说明放行了: %+v", resp)
			}
			if !resp.Auth {
				t.Fatalf("应当标记成鉴权失败,好让客户端给出正确处置: %+v", resp)
			}
		})
	}
}

// ★ 长度模式(StdinLen >= 0)是协议预留、未实现:daemon 必须**响亮拒绝**,
// 而不是跑一条零调用方、零测试的分支。
//
// 这条测试替代了「给死分支补正向测试」那个选项。原先 daemon 里有
// `if n >= 0 { io.CopyN(pw, conn, n) }`,但 client 永远只发 StdinUntilEOF ——
// os.Stdin 是流,**读之前根本不知道长度**,所以没有任何合法来源会发 >=0。
// 给它补一条正向用例,等于测一个永远不会被生产的形态;改成「明确不支持 + 拒绝
// 路径的回归」,协议能力就与实现一致了,而且这条是**真打 daemon** 的
// (rawRequest 绕过 client 直接发帧,同攻击者视角)。
func TestUnsupportedStdinLenIsRejectedByDaemon(t *testing.T) {
	ep := startDaemon(t, DefaultReadTimeout)
	_, token := readEndpointFile(t, ep)

	for _, n := range []int64{0, 1, 1 << 20} {
		resp, err := rawRequest(t, ep, ipc.Request{
			Proto:     ipc.ProtoVersion, // 版本要对,否则会被 CheckProto 先挡住(那是另一条路径)
			Token:     token,            // 鉴权要过,否则走不到 stdin 检查
			Op:        "run",
			Script:    "cat",
			StdinMode: ipc.StdinStream,
			StdinLen:  n,
		})
		if err != nil {
			t.Fatalf("stdin_len=%d 没得到响应(应当明确拒绝): %v", n, err)
		}
		if resp.Exit != 2 {
			t.Fatalf("stdin_len=%d 期望用法错误 2,得到 %d (out=%q errors=%v)",
				n, resp.Exit, resp.Out, resp.Errors)
		}
		msg := strings.Join(resp.Errors, "\n")
		if !strings.Contains(msg, "stdin_len") {
			t.Fatalf("stdin_len=%d 错误没点名字段: %v", n, resp.Errors)
		}
		// 三行式:必须带「怎么办」——报错要可操作,这是项目错误模型的硬要求。
		if !strings.Contains(msg, "--stop") {
			t.Fatalf("stdin_len=%d 错误没有指路: %v", n, resp.Errors)
		}
	}
}

// 端口文件必须带着 token,而 token 不能是空串或可猜的常量。
func TestEndpointFileCarriesARandomToken(t *testing.T) {
	ep := startDaemon(t, DefaultReadTimeout)

	_, token := readEndpointFile(t, ep)
	if token == "" {
		t.Fatal("端口文件里没有 token")
	}
	if len(token) < 16 {
		t.Fatalf("token 太短,不像随机值: %q", token)
	}

	// 换个 daemon,token 必须不同(排除写死的常量)。
	ep2 := startDaemon(t, DefaultReadTimeout)
	_, token2 := readEndpointFile(t, ep2)
	if token == token2 {
		t.Fatalf("两个 daemon 的 token 相同,说明不是随机的: %q", token)
	}
}

// 正常路径不能被鉴权改坏:client 仍然能跑通,而且它把 token 带上了。
func TestAuthorizedClientStillWorks(t *testing.T) {
	ep := startDaemon(t, DefaultReadTimeout)

	resp, err := client.Run(ep, "echo hi", "", false)
	if err != nil {
		t.Fatalf("带 token 的正常调用失败了: %v", err)
	}
	if got := strings.TrimSpace(string(resp.Out)); got != "hi" {
		t.Fatalf("out=%q want hi", got)
	}
}

// ★ P0-2:只连上、一个字节都不发的连接,不能再把 daemon 挂死。
//
// 修之前:handle 里的 ipc.ReadMessage 没有 ReadDeadline,而 handle 是同步串行调用的
// —— 一个这样的连接会让后续**所有**命令排在它后面(实测要等到 client 侧超时)。
func TestSilentConnectionDoesNotWedgeTheDaemon(t *testing.T) {
	// 给一个很短的单连接读预算,好在测试里真的观察到它被放弃。
	ep := startDaemon(t, 300*time.Millisecond)

	addr, _ := readEndpointFile(t, ep)
	silent, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer silent.Close()

	done := make(chan error, 1)
	go func() {
		_, err := client.Run(ep, "echo ok", "", false)
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("被那个静默连接挡住了: %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("daemon 被一个只连不说话的连接挂死了(修 P0-2 之前就是这个症状)")
	}
}

// ★ P1-3:client 放弃之后,daemon 侧也要停下来。
//
// 修之前:daemon 侧用的是 context.Background(),没有超时也没有取消 ——
// 一条无限循环会一直占着 handle,后续命令全部排队(实测第二条命令等满自己的超时)。
func TestTimedOutRequestStopsRunningInDaemon(t *testing.T) {
	ep := startDaemon(t, DefaultReadTimeout)

	// 无限循环,客户端 600ms 就放弃
	_, _ = client.RunTimeout(ep, "while :; do :; done", "", false, 600*time.Millisecond, 0)

	// daemon 侧应当在请求自带的超时到点后自己收手,于是下一条命令很快就能跑
	start := time.Now()
	if _, err := client.RunTimeout(ep, "echo second", "", false, 10*time.Second, 0); err != nil {
		t.Fatalf("第二条失败了: %v", err)
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("第二条被饿死了,耗时 %s（daemon 没有按请求超时收手）", el)
	}
}

// ★ P1-2:超时后绝对不能重放脚本。
//
// 修之前:client 超时后会清掉端口文件、拉起新 daemon、把同一个请求原样重发 ——
// 于是带副作用的脚本被执行两遍(实测 `echo RUN >> f; sleep 6` 配 --timeout 2s,
// 文件里出现 2 行)。对 mkdir / git commit / 写文件这类脚本是数据损坏级别的。
func TestTimeoutDoesNotReplayTheScript(t *testing.T) {
	ep := startDaemon(t, DefaultReadTimeout)

	marker := filepath.Join(t.TempDir(), "runs.txt")
	script := "echo RUN >> '" + marker + "'; sleep 4"

	_, _ = client.RunTimeout(ep, script, "", false, 800*time.Millisecond, 0)
	// 等 daemon 侧也跑完(它会因为请求超时而收手,但给足时间以免误判)
	time.Sleep(6 * time.Second)

	b, _ := os.ReadFile(marker)
	if n := strings.Count(string(b), "RUN"); n != 1 {
		t.Fatalf("脚本被执行了 %d 遍,应当只有 1 遍(超时重放)", n)
	}
}
