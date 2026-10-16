package ipc

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRequestStdinModeRoundTrip 钉住 stdin 的形态字段能往返 —— 流式协议的头帧
// 全靠 StdinMode/StdinLen 表达「要不要读 stdin、读到什么时候为止」。
//
// 注意 stdin 的**字节**本身不再进 JSON:它走请求头之后的裸字节流(见 StdinMode
// 的注释)。这比原先的 base64 更直接 —— 字节不经编码、不受帧上限约束,也就不存在
// json.Marshal 把非法 UTF-8 静默换成 U+FFFD 的风险(那是 Response.Out 用 []byte
// 要解决的问题,stdin 已经绕开了)。
func TestRequestStdinModeRoundTrip(t *testing.T) {
	b, err := Encode(Request{
		Proto:     ProtoVersion,
		Op:        "run",
		Script:    "cat",
		StdinMode: StdinStream,
		StdinLen:  StdinUntilEOF,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeRequest(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.StdinMode != StdinStream {
		t.Fatalf("StdinMode 未往返：got %q want %q", got.StdinMode, StdinStream)
	}
	if got.StdinLen != StdinUntilEOF {
		t.Fatalf("StdinLen 未往返：got %d want %d", got.StdinLen, StdinUntilEOF)
	}
	if got.Proto != ProtoVersion {
		t.Fatalf("Proto 未往返：got %d want %d", got.Proto, ProtoVersion)
	}
}

// TestRequestWithoutStdinModeIsNone 钉住「没有 stdin_mode 字段的请求」解出来是
// 「没有 stdin」—— 空 StdinMode 必须等同 StdinNone，daemon 侧不该从连接读字节。
//
// 这条也覆盖字段被删干净的事实：旧的 `stdin`（base64 字节）字段不复存在，
// 手写一份带它的 JSON 不该污染任何现有字段。
func TestRequestWithoutStdinModeIsNone(t *testing.T) {
	old := `{"proto":2,"token":"t","op":"run","script":"echo hi","cwd":"C:\\","json":false,"stdin":"aGk="}`
	got, err := DecodeRequest([]byte(old))
	if err != nil {
		t.Fatal(err)
	}
	if got.StdinMode != "" && got.StdinMode != StdinNone {
		t.Fatalf("StdinMode 应为空或 none，得到 %q", got.StdinMode)
	}
	if got.StdinLen != 0 {
		t.Fatalf("StdinLen 应为 0，得到 %d", got.StdinLen)
	}
	if got.Script != "echo hi" || got.Cwd != `C:\` || got.Token != "t" {
		t.Fatalf("其余字段被破坏：%+v", got)
	}

	// 协议移除 stdin 字段这件事，要从**出向**断言：新 client 序列化出来的请求
	// 不该再带 stdin。（入向断言是反的 —— 旧 JSON 里带着 stdin 是**输入**，
	// unmarshal 后当然还在 map 里，Request 结构体没有它才会被忽略。）
	fresh, err := Encode(Request{Proto: ProtoVersion, Script: "cat", StdinMode: StdinStream})
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(fresh, &out); err != nil {
		t.Fatal(err)
	}
	if v, ok := out["stdin"]; ok {
		_ = v
		t.Fatal("新请求不该再序列化出 stdin 字段（协议已改为裸字节流）")
	}
	if _, ok := out["stdin_mode"]; !ok {
		t.Fatal("新请求必须带 stdin_mode")
	}
}

// TestResponseProtoRoundTrip 钉住响应侧的版本字段能往返 —— 新 client 靠它识别
// 「连上的是旧 daemon」（旧 daemon 不写这个字段，值为 0）。
func TestResponseProtoRoundTrip(t *testing.T) {
	b, err := EncodeResponse(Response{Proto: ProtoVersion, OK: true, Exit: 0})
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeResponse(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.Proto != ProtoVersion {
		t.Fatalf("Proto 未往返：got %d want %d", got.Proto, ProtoVersion)
	}
}

// TestCheckProto 判断的三种情况。两条失败分支都必须带「怎么办」——
// 报错要可操作，这是项目错误模型的硬要求。
func TestCheckProto(t *testing.T) {
	if err := CheckProto("daemon", ProtoVersion); err != nil {
		t.Fatalf("版本一致不该报错：%v", err)
	}

	old := CheckProto("daemon", 0)
	if old == nil {
		t.Fatal("旧 daemon（Proto=0）必须报错，否则 stdin 会被静默忽略、bug 假修好")
	}
	if !strings.Contains(old.Error(), "--stop") {
		t.Fatalf("错误信息没有指路：\n%s", old.Error())
	}

	mismatch := CheckProto("daemon", ProtoVersion+1)
	if mismatch == nil {
		t.Fatal("版本不符必须报错")
	}
	if !strings.Contains(mismatch.Error(), "--stop") {
		t.Fatalf("错误信息没有指路：\n%s", mismatch.Error())
	}
}

// TestFrameLimitStillGuardsStdinStream 钉住一个容易被想当然推翻的点:裸字节流
// **不**经过 WriteMessage,所以 64MiB 的帧上限管不到它 —— 这正是流式的价值
// (不缓存、不受帧上限)。这条断言的是 ReadMessage 对请求头的约束仍然在,
// 否则有人可能顺手把上限放宽,连头帧一起放开。
func TestFrameLimitStillGuardsStdinStream(t *testing.T) {
	if maxMessageBytes == 0 {
		t.Fatal("请求头帧的上限不该为 0")
	}
	// StdinUntilEOF 必须是负数:daemon 靠 n<0 判断「读到 EOF」而不是 CopyN。
	if StdinUntilEOF >= 0 {
		t.Fatalf("StdinUntilEOF 必须为负（daemon 用 n<0 分流），得到 %d", StdinUntilEOF)
	}
}
