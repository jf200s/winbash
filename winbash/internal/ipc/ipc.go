// Package ipc 定义 winbash 客户端与 daemon 之间的分帧协议。
//
// 帧格式:4 字节小端长度 + payload(UTF-8 JSON)。
package ipc

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// maxMessageBytes 是单帧 payload 的上限。超出视为协议损坏,而非合法请求。
const maxMessageBytes = 64 << 20

// ProtoVersion 是请求/响应携带的协议版本。
//
// 它存在的唯一理由是「daemon 是常驻的」:空闲默认 30 分钟才退,且 daemon 由 client
// 用**同一个 exe 路径**拉起 —— 于是「用户替换了 winbash.exe、旧 daemon 还活着」是
// 正常状态而不是意外。这种状态下新旧两侧对同一个 JSON 的理解会悄悄分叉。
//
// 判断新旧靠**响应**:旧 daemon 的 Response 里没有 Proto(值为 0),新 client 一看便知。
// 反过来(旧 client 连新 daemon)由 daemon 侧检查 Request.Proto —— 不匹配就报错,
// 不静默按旧语义跑。两端都响亮,是因为「静默吞掉 stdin」正是本次要修的 bug 形态,
// 修完却在另一个入口复现,等于没修。
//
// 版本历史:
//
//	1 一次性 stdin:Request.Stdin []byte 走 JSON base64,受 64MiB 帧上限约束。
//	2 流式 stdin:请求头之后跟**裸字节流**,不再缓存到内存(理由见 StdinMode)。
//	3 Request.Fresh:一条命令可以要求跑在**一次性会话**里(理由见 Fresh 字段)。
//	4 Request.Session:一条命令可以指定落到哪个**会话**里(理由见 Session 字段)。
//
// 1 → 2 必须**两端同时**升级:如果 client 切到流式、daemon 还是 1,那么 daemon
// 只解请求头就去执行,根本不读后面那串字节 —— 于是 stdin 静默丢失,而脚本照样
// exit 0。这正是本次要消灭的 bug 形态,所以在 CheckProto 处直接响亮拒绝。
//
// 2 → 3 同理必须同时升级:旧 daemon 不认识 fresh 字段,会认 JSON 里多出来的键
// 就忽略 —— 于是 `--fresh` 静默变成「复用会话」,调用方以为隔离了、其实没有。
// 这种「看起来生效、实际没生效」正是本项目所有版本检查要防的形态。
//
// 3 → 4 同样必须同时升级:旧 daemon 不认识 session 字段,会当没写 —— 于是
// `--session build` 静默退化成默认会话,调用方以为已经隔离了。与 2 → 3 是同一个理由。
//
// 改动协议时把它 +1,并在两端的检查处同步更新说明。
const ProtoVersion = 4

// StdinMode 的取值:一次请求的 stdin 走什么形态。
const (
	// StdinNone 表示本次没有 stdin —— daemon **不从连接读** stdin 字节,
	// session 保持空输入(立即 EOF)。交互终端下的调用走这条。
	StdinNone = "none"

	// StdinStream 表示 stdin 是请求头帧之后的**裸字节流**:没有帧头、没有 base64、
	// 不受 maxMessageBytes 约束。管道输入的主路径走这条。
	StdinStream = "stream"
)

// StdinLen 是 stdin 流的长度语义(StdinMode==StdinStream 时有意义)。
const (
	// StdinUntilEOF 表示读到半侧关闭(FIN)为止。管道输入本质没有确定长度,
	// 所以这是主路径 —— client 转发完上游就 CloseWrite()。
	StdinUntilEOF = -1
)

// Request 是客户端发给 daemon 的一条请求。
type Request struct {
	// Proto 是发端的协议版本。daemon 收到与 ProtoVersion 不符的值会响亮报错,
	// 不静默按旧语义执行(理由见 ProtoVersion 的注释)。
	Proto int `json:"proto,omitempty"`

	// Token 是 daemon 启动时生成、随端口文件一起发出的随机串。它挡的是
	// 「同机**其他**用户/沙箱进程读到端口文件,于是借 daemon 的身份执行命令」这条通道
	// —— 实测无 token 的裸 socket 请求会让 whoami 返回 daemon 属主名,还能把 daemon 打停。
	//
	// 它挡不住**同一个用户**的进程:那种进程本来就能读你的文件、注入你的进程,
	// 在 Windows 上无从防起,也不是本项目的威胁模型。别把 token 当万能钥匙。
	Token string `json:"token,omitempty"`

	Op     string `json:"op"`
	Script string `json:"script"`
	Cwd    string `json:"cwd"`
	JSON   bool   `json:"json"`

	// StdinMode 标明这次请求的 stdin 走什么形态(取值见 StdinNone/StdinStream)。
	// 空值等同 StdinNone —— 但既然要升版本,两端都应显式写上。
	StdinMode string `json:"stdin_mode,omitempty"`

	// StdinLen 是 stdin 流的长度语义(StdinMode==StdinStream 时有意义)。
	//
	// 当前**只支持** StdinUntilEOF(-1):读到对端半侧关闭(FIN)为止,管道输入的
	// 主路径走这条。
	//
	// >=0(读恰好 N 字节)**是协议预留、未实现** —— client 拿不到 os.Stdin 的长度
	// (要读到 EOF 才知道),没有合法来源会发它。daemon 收到即报错(退出码 2),
	// 而不是走一条**没有调用方、没有测试**的分支;真要做长度模式时(例:文件重定向
	// 可直接 f.Stat()),连同 client 侧的发送与测试一起加,并把 ProtoVersion 升一位。
	StdinLen int64 `json:"stdin_len,omitempty"`

	// TimeoutMS 是这次请求在 daemon 侧的执行上限(0 = 不设限)。
	//
	// daemon 必须自己看这个值:client 断开并不会让解释器停下来,而请求是在**会话
	// 队列**里串行执行的 —— 一条无限循环会一直占着**它那个会话**,把投给同一会话的
	// 后续命令饿死(实测第二条命令要等满自己的超时)。别的会话不受影响 —— 那是另一条队列。
	//
	// 流式 stdin 落地后它的含义扩大了:它**同时**是「这次调用读 stdin 的上限」。
	// 理由是 DefaultReadTimeout 只护「读请求头」那一步(读完就清掉,见 daemon.handleConn),
	// 执行阶段没有读预算;而 stdin 现在是持续读连接的 —— 没有这一条,一条
	// 「只连、不发、不关」的流式请求能把**它那个会话**的 worker 永久占住,投给该会话
	// 的后续命令全部饿死。
	TimeoutMS int `json:"timeout_ms,omitempty"`

	// Fresh 要求这次执行跑在**一次性会话**里:daemon 为它新建一个 shell.Session,
	// 跑完就 Close,长驻会话一格都不动。
	//
	// 为什么需要它:winbash 的会话是**跨命令持久**的(变量、函数、set -e、trap、
	// cwd 都在),这是它相对 `bash -c` 的特性;但对「每条命令彼此独立」的调用方,
	// 上一条留下的 `set -e` 或变量会静默影响下一条 —— 最容易出怪问题的地方。
	// 默认保持持久(不破坏既有行为),要隔离就显式声明。
	//
	// 隔离是**双向**的:之前的命令影响不到它,它设的东西也不留给后面的命令。
	// 代价:② 层的常驻 pwsh 引擎挂在 Session 上,所以 fresh 请求一旦用到 ② 层,
	// 要重新拉起一次(约 350ms);只用 ①/③ 层没有额外开销。
	Fresh bool `json:"fresh,omitempty"`

	// Session 是要落到的会话名。空串 = 默认会话(与引入本字段之前完全等价)。
	//
	// 同名会话内严格串行且会话状态(变量/函数/set -e/trap)照旧跨命令持久;不同
	// 会话名之间才真正并行。这样「跨命令状态持久」和「并发执行」都保住了 ——
	// 代价是状态边界从整个 daemon 缩到一个会话,而这是并行的必要条件:
	// shell.Session 里的 interp.Runner 本身不并发安全。
	//
	// 为什么必须显式传(而不是自动识别会话):client 每次调用都是**新进程**,没有
	// 稳定身份可用来自动路由。CLI 侧由 --session 或环境变量 WINBASH_SESSION_NAME
	// 提供(见 cmd/winbash/main.go)。
	Session string `json:"session,omitempty"`
}

// Response 是 daemon 返回给客户端的一条结果。
//
// Out 必须是 []byte 而不是 string:json.Marshal 会把 string 里的非法 UTF-8 字节
// 静默替换成 U+FFFD,而 []byte 走 base64 可以原样往返。这是"二进制管道不被静默
// 损坏"的实现基础,也是本设计"不静默做错事"承诺的一部分。
type Response struct {
	// Proto 是 daemon 的协议版本。新 client 靠它识别「连上的是旧 daemon」:
	// 旧 daemon 不写这个字段,值为 0。详见 ProtoVersion。
	Proto int `json:"proto,omitempty"`

	OK     bool     `json:"ok"`
	Out    []byte   `json:"out"`
	Errors []string `json:"errors"`
	Exit   int      `json:"exit"`

	// Auth 标明这次失败是**鉴权**失败 —— 客户端据此决定"换个 daemon 重连"
	// 还是"重试",而不是把 401 当成脚本本身跑失败了。
	Auth bool `json:"auth,omitempty"`
}

// CheckProto 判断对端的协议版本是否与本端一致,不一致返回可操作的错误。
//
// 把判断放在 ipc 而不是散在 client/daemon:两端的判据必须是同一份,
// 否则就会出现「client 认为兼容、daemon 认为不兼容」的分叉 —— 那正是
// ProtoVersion 要消灭的东西。
//
// 措辞刻意用中性的「本端」而不是「本客户端」:`what` 描述的是**对端**,而这
// 两处调用里对端既可能是 daemon 也可能是 client(见 client.go / daemon.go)。
// 写死"本客户端"会让 daemon 侧那条错误自相矛盾 —— 修之前正是这么写的,配合
// daemon 侧误传 what="daemon"(它收到的其实是 **client** 的版本),于是旧 client
// 连新 daemon 时会看到「daemon 的协议版本是 3,本客户端要 4」:3 是旧 client 的、
// 4 是新 daemon 的,两个数字全标错了对象。
func CheckProto(what string, got int) error {
	switch {
	case got == ProtoVersion:
		return nil
	case got == 0:
		return fmt.Errorf("%s 没有协议版本(它是旧版),与本端(%d)不兼容\n"+
			"怎么办: 运行 `winbash --stop` 停掉旧 daemon,下次调用会自动用新 exe 重建",
			what, ProtoVersion)
	default:
		return fmt.Errorf("%s 的协议版本是 %d,本端要 %d\n"+
			"怎么办: 两侧的 winbash.exe 版本不一致;统一后运行 `winbash --stop` 重建 daemon",
			what, got, ProtoVersion)
	}
}

// WriteMessage 写出 4 字节小端长度头,再写 payload。
func WriteMessage(w io.Writer, payload []byte) error {
	if len(payload) > maxMessageBytes {
		return fmt.Errorf("ipc: payload too large (%d bytes)", len(payload))
	}
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

// ReadMessage 读出一个完整的帧。长度 <=0 或超过上限都返回 error。
func ReadMessage(r io.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.LittleEndian.Uint32(hdr[:])
	if n == 0 || n > maxMessageBytes {
		return nil, fmt.Errorf("ipc: bad frame length %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// Encode 把请求序列化成 JSON。
func Encode(req Request) ([]byte, error) {
	return json.Marshal(req)
}

// DecodeResponse 解析 daemon 的响应。
func DecodeResponse(b []byte) (Response, error) {
	var resp Response
	err := json.Unmarshal(b, &resp)
	return resp, err
}

// EncodeResponse 把响应序列化成 JSON。
//
// Ruling R8：Task 1 只给了 Encode(请求) / DecodeResponse(响应)，方向不对称 ——
// daemon 没有请求解码器、client 没有响应编码器。Task 2 补齐这两个方向。
func EncodeResponse(resp Response) ([]byte, error) {
	return json.Marshal(resp)
}

// DecodeRequest 解析客户端发来的请求。
func DecodeRequest(b []byte) (Request, error) {
	var req Request
	err := json.Unmarshal(b, &req)
	return req, err
}
