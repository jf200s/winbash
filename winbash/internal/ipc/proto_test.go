package ipc

import (
	"bytes"
	"strings"
	"testing"
)

// TestRequestStdinRoundTrip 钉住 stdin 的二进制保真:含 0x00 与非法 UTF-8 的 stdin
// 经 Encode → DecodeRequest 必须原样往返。
//
// 这条等价于 Response.Out 那条既有回归。用 string 承载的话,json.Marshal 会把非法
// UTF-8 静默换成 U+FFFD —— 那正是「二进制管道被静默损坏」,本项目明确不接受。
func TestRequestStdinRoundTrip(t *testing.T) {
	bin := []byte{0x00, 0xff, 0xfe, 0x80, 'h', 'i', '\n'}
	b, err := Encode(Request{Op: "run", Script: "cat", Stdin: bin})
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeRequest(b)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Stdin, bin) {
		t.Fatalf("stdin 未原样往返：got %x want %x", got.Stdin, bin)
	}
}

// TestRequestWithoutStdinStaysNil 钉住「旧 client 的请求仍能解」:旧 client 不写
// stdin 也不写 proto，解出来必须是 Stdin=nil、Proto=0，且其余字段完好。
//
// 这是新旧混跑的旧侧分支 —— daemon 收到它应当当作「无 stdin」正常执行，
// 而不是报错或崩溃。
func TestRequestWithoutStdinStaysNil(t *testing.T) {
	// 手写一份旧 client 会发出的 JSON：没有 stdin、没有 proto 字段。
	old := `{"token":"t","op":"run","script":"echo hi","cwd":"C:\\","json":false}`
	got, err := DecodeRequest([]byte(old))
	if err != nil {
		t.Fatal(err)
	}
	if got.Stdin != nil {
		t.Fatalf("旧 client 的 stdin 应为 nil，得到 %x", got.Stdin)
	}
	if got.Proto != 0 {
		t.Fatalf("旧 client 的 Proto 应为 0，得到 %d", got.Proto)
	}
	if got.Script != "echo hi" || got.Cwd != `C:\` || got.Token != "t" {
		t.Fatalf("旧 client 的其余字段被破坏：%+v", got)
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

// TestMaxStdinBytesBelowFrameLimit 钉住两道界的关系：stdin 的界必须显著小于帧上限，
// 否则 stdin 会先撞帧上限、报的是「帧损坏」这种答非所问的错。
func TestMaxStdinBytesBelowFrameLimit(t *testing.T) {
	if MaxStdinBytes >= maxMessageBytes {
		t.Fatalf("MaxStdinBytes(%d) 必须小于 maxMessageBytes(%d)", MaxStdinBytes, maxMessageBytes)
	}
	// base64 膨胀 4/3，还要给脚本与 token 留位置。
	if MaxStdinBytes*4/3 >= maxMessageBytes {
		t.Fatalf("MaxStdinBytes(%d) base64 后仍会撞帧上限(%d)", MaxStdinBytes, maxMessageBytes)
	}
}
