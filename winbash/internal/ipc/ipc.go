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
// 改动协议时把它 +1,并在两端的检查处同步更新说明。
const ProtoVersion = 1

// MaxStdinBytes 是单次请求可携带的 stdin 上限。
//
// 为什么不是 maxMessageBytes:stdin 走 JSON 的 []byte,序列化成 base64 会膨胀约
// 1/3,再算上脚本与 token 等字段,贴着 64MiB 走会先撞上帧上限、报的是「帧损坏」
// 这种答非所问的错。单独给 stdin 一道更小的界,才能在**读 stdin 的当下**就报
// 「stdin 过大」,而不是读完、编码完、发出去之后才失败。
//
// 超限**必须响亮报错,不得静默截断** —— 截断后的输出看起来是成功的,正是本项目
// 反复强调要消灭的那种失败形态。
const MaxStdinBytes = 16 << 20

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

	// Stdin 是这条脚本的标准输入。为**空的非 nil** 与**未设置**是两回事:前者
	// (旧 client、或本次调用确实没有 stdin)是「立即 EOF」,后者是未接通 stdin 通道。
	//
	// 用 []byte 而不是 string:json.Marshal 会把 string 里的非法 UTF-8 静默替换
	// 成 U+FFFD,而 []byte 走 base64 原样往返 —— 与 Response.Out 同一条理由,
	// 二进制管道不能被静默损坏。上限见 MaxStdinBytes。
	Stdin []byte `json:"stdin,omitempty"`

	// TimeoutMS 是这次请求在 daemon 侧的执行上限(0 = 不设限)。
	//
	// daemon 必须自己看这个值:client 断开并不会让解释器停下来,而 handle 是
	// 串行调用的 —— 一条无限循环会一直占着它,把后续所有命令饿死(实测第二条
	// 命令要等满自己的超时)。
	TimeoutMS int `json:"timeout_ms,omitempty"`
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
func CheckProto(what string, got int) error {
	switch {
	case got == ProtoVersion:
		return nil
	case got == 0:
		return fmt.Errorf("%s 没有协议版本(它是旧版),与本客户端(%d)不兼容\n"+
			"怎么办: 运行 `winbash --stop` 停掉旧 daemon,下次调用会自动用新 exe 重建",
			what, ProtoVersion)
	default:
		return fmt.Errorf("%s 的协议版本是 %d,本客户端要 %d\n"+
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
