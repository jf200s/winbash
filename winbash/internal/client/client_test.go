package client

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 本文件补的是"凭据读取"与"陈旧端口文件的删除条件"这两段 —— 纯文件级、不需要 daemon。
//
// 复审指出它们在本包口径与跨包口径下**都是 0%**。更值得记的是为什么会被漏掉:它们是
// 新增鉴权重试链路里唯一能用单元测试覆盖的部分(另一段是"读完 token 后旧 daemon 退出、
// 新 daemon 改写端口文件"的竞态,得靠集成测试构造时序),而 `guard_test.go` 从 daemon
// 包那一侧覆盖到的只是完整往返 —— 于是它们在包内口径下一直是 0,看上去像"没人管"。

// readEndpoint 必须解析出两行;缺 token 要**照常返回**(空 token),由 daemon 拒绝。
func TestReadEndpointParsesAddressAndToken(t *testing.T) {
	dir := t.TempDir()
	ep := filepath.Join(dir, "wb.port")
	if err := os.WriteFile(ep, []byte("127.0.0.1:62615\nabc123\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	info, err := readEndpoint(ep)
	if err != nil {
		t.Fatalf("readEndpoint: %v", err)
	}
	if info.addr != "127.0.0.1:62615" {
		t.Errorf("addr = %q, 期望 127.0.0.1:62615", info.addr)
	}
	if info.token != "abc123" {
		t.Errorf("token = %q, 期望 abc123", info.token)
	}
}

// 只有地址、没有 token(老 daemon 或外部工具写的)要能读出来,token 为空。
//
// 这是**有意的降级方向**:缺少凭据应该是"被拒",不是"放行"。所以这里不能报错,
// 否则 client 会去拉起一个新 daemon,反而把已有的那个丢掉。
func TestReadEndpointToleratesMissingToken(t *testing.T) {
	dir := t.TempDir()
	ep := filepath.Join(dir, "wb.port")
	if err := os.WriteFile(ep, []byte("127.0.0.1:62615\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	info, err := readEndpoint(ep)
	if err != nil {
		t.Fatalf("readEndpoint: %v", err)
	}
	if info.addr != "127.0.0.1:62615" || info.token != "" {
		t.Errorf("addr=%q token=%q, 期望 addr=127.0.0.1:62615 token=\"\"", info.addr, info.token)
	}
}

func TestReadEndpointRejectsEmptyFile(t *testing.T) {
	dir := t.TempDir()
	ep := filepath.Join(dir, "wb.port")
	if err := os.WriteFile(ep, []byte("\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := readEndpoint(ep); err == nil {
		t.Fatal("空端口文件应当报错,而不是给一个空地址")
	}
}

// forgetStaleEndpoint 只在文件**仍指向 addr** 时才删 —— 否则会删掉新 daemon 的端口文件,
// 把它变成"谁也找不到"的孤儿(与 daemon 侧 removeIfOwned 是同一个理由)。
func TestForgetStaleEndpointOnlyRemovesItsOwnAddress(t *testing.T) {
	t.Run("地址匹配就删", func(t *testing.T) {
		dir := t.TempDir()
		ep := filepath.Join(dir, "wb.port")
		if err := os.WriteFile(ep, []byte("127.0.0.1:1\ntok-old\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		forgetStaleEndpoint(ep, "127.0.0.1:1")

		if _, err := os.Stat(ep); !os.IsNotExist(err) {
			t.Fatalf("地址匹配时应当删掉端口文件,stat err = %v", err)
		}
	})

	t.Run("地址不匹配就不删", func(t *testing.T) {
		dir := t.TempDir()
		ep := filepath.Join(dir, "wb.port")
		// 这就是那个窗口:文件已经被**新** daemon 改写过了。
		if err := os.WriteFile(ep, []byte("127.0.0.1:2\ntok-new\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		forgetStaleEndpoint(ep, "127.0.0.1:1")

		b, err := os.ReadFile(ep)
		if err != nil {
			t.Fatalf("地址不匹配时不该删: %v", err)
		}
		if got := string(b); got != "127.0.0.1:2\ntok-new\n" {
			t.Errorf("文件被改动了: %q", got)
		}
	})

	t.Run("文件不存在也不能炸", func(t *testing.T) {
		forgetStaleEndpoint(filepath.Join(t.TempDir(), "nope.port"), "127.0.0.1:1")
	})
}

// trustEndpoint 是「删不掉的端口文件让 endpoint 永久不可用」那条缺陷的判据本身,
// 所以四个分支都必须钉住。为什么值得单独测:它在生产里只表现为"连不上之后再连一次
// 还是连不上",没有任何日志会把"你又信任了那个死地址"说出来 —— 回归了也没人看得见。
func TestTrustEndpoint(t *testing.T) {
	const dead = "127.0.0.1:62615"

	t.Run("读不到就不信任", func(t *testing.T) {
		// 文件不存在 / 空文件走的都是这一条。
		if trustEndpoint(endpointInfo{}, errors.New("没有这个文件"), "") {
			t.Fatal("err != nil 时不该信任")
		}
	})

	t.Run("仍是那个死地址就不信任", func(t *testing.T) {
		// 这正是缺陷现场:文件还在、地址还是刚才连不上的那个。
		if trustEndpoint(endpointInfo{addr: dead, token: "t"}, nil, dead) {
			t.Fatalf("addr == deadAddr 时不该信任 —— 否则会一直信任同一个死地址,永不恢复")
		}
	})

	t.Run("换成新地址就信任", func(t *testing.T) {
		// 新 daemon 覆盖写过了:地址变了,这就是"修好了"的样子。
		if !trustEndpoint(endpointInfo{addr: "127.0.0.1:62616", token: "t"}, nil, dead) {
			t.Fatal("addr != deadAddr 时应当信任")
		}
	})

	t.Run("没有已知坏地址时读到就信任", func(t *testing.T) {
		// deadAddr == "" 是首次启动与管理操作的形态:没有"坏地址"可比,不拦。
		if !trustEndpoint(endpointInfo{addr: dead, token: "t"}, nil, "") {
			t.Fatal("deadAddr 为空时应当信任")
		}
	})
}

// serveArgs 决定"自动拉起的 daemon 拿到什么命令行"。**这是本轮修的那条**:
// 原先它只拼 `--serve --endpoint`,使用者写的 `--idle` 被静默丢掉,自动拉起的
// daemon 永远用默认的 30 分钟(实测:不转发时 `winbash --idle 3s -c 'echo x'`
// 等 7s 后 daemon 仍在、端口文件仍在)。
//
// 抽成纯函数就是为了能在这里钉住它 —— spawnDaemon 自己必须构建真 exe 才跑得起来
// (测试进程里 os.Executable() 是 winbash.test,它不认识 --serve),于是这条最容易
// 出错的一环原先完全没有回归网。
func TestServeArgsForwardsIdle(t *testing.T) {
	t.Run("给了 idle 就转过去", func(t *testing.T) {
		got := serveArgs("C:/tmp/wb.port", 3*time.Second)
		want := []string{"--serve", "--endpoint", "C:/tmp/wb.port", "--idle", "3s"}
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("serveArgs = %q, 期望 %q", got, want)
		}
	})

	t.Run("idle<=0 不传这个开关", func(t *testing.T) {
		// `--idle 0` 是另一个意思("永不退出"),不能当成"没指定"传过去。
		for _, idle := range []time.Duration{0, -time.Second} {
			got := serveArgs("C:/tmp/wb.port", idle)
			for _, a := range got {
				if a == "--idle" {
					t.Errorf("idle=%v 时不该传 --idle: %q", idle, got)
				}
			}
			if len(got) != 3 {
				t.Errorf("idle=%v 时 args = %q, 期望 3 项", idle, got)
			}
		}
	})
}

// isSendFailure 是「能不能安全重试」的唯一判据,所以它本身要有回归。
//
// 两个方向都要钉住:
//   - 发送阶段失败 → true(请求没送出去,daemon 不会执行);
//   - 读响应失败   → false(请求已送达,命令可能已经跑了)。
//
// 第二个方向是最要紧的那条:把它判成 true 就等于恢复旧的「非超时一律重放」,
// 而同一条脚本跑两遍正是本项目反复在消灭的失败形态。
func TestIsSendFailureDistinguishesSendFromRead(t *testing.T) {
	send := &sendFailure{err: errors.New("wsarecv: connection reset")}
	if !isSendFailure(send) {
		t.Fatal("发送阶段的失败必须被判为可安全重试")
	}
	// 包了一层也要能认出来(errors.As 的意义)。
	if !isSendFailure(fmt.Errorf("wrapped: %w", send)) {
		t.Fatal("包装过的发送失败同样要认出来")
	}

	// 读响应失败是普通 error —— 绝不能当成可重试。
	read := errors.New("wsarecv: An existing connection was forcibly closed by the remote host")
	if isSendFailure(read) {
		t.Fatal("读响应失败不是发送失败:判 true 会让脚本被重放")
	}
	if isSendFailure(nil) {
		t.Fatal("nil 不该被当成发送失败")
	}

	// 超时走的是另一条判据(isTimeout),但它可能出现在任一阶段;
	// 这里只确认两条判据互不吞掉:超时错误不会被 isSendFailure 误判。
	timeout := &net.OpError{Op: "read", Err: &timeoutError{}}
	if isSendFailure(timeout) {
		t.Fatal("裸超时不是发送失败")
	}
}

// timeoutError 是最小的 net.Error 实现,给上面那条分类测试用。
type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }
