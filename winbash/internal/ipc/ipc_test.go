package ipc

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestWriteReadMessageRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	payload := []byte(`{"script":"echo hi"}`)
	if err := WriteMessage(&buf, payload); err != nil {
		t.Fatal(err)
	}
	got, err := ReadMessage(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("got %q want %q", got, payload)
	}
}

func TestReadMessageRejectsZeroLength(t *testing.T) {
	var buf bytes.Buffer
	buf.Write([]byte{0, 0, 0, 0})
	if _, err := ReadMessage(&buf); err == nil {
		t.Fatal("expected error for zero length")
	}
}

func TestEncodeDecodeKeepsScriptVerbatim(t *testing.T) {
	// 中文与换行必须原样往返
	script := "cd 'D:\\work\\中文 目录'\necho 中文\n"
	b, err := Encode(Request{Script: script})
	if err != nil {
		t.Fatal(err)
	}
	var got Request
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Script != script {
		t.Fatalf("script mangled: %q", got.Script)
	}
}

func TestResponseOutSurvivesBinaryBytes(t *testing.T) {
	// Review Focus 第 4 条：非法 UTF-8 字节必须原样往返，不得被替换成 U+FFFD
	raw := []byte{0x00, 0xff, 0xfe, 0x80, 0x41}
	b, err := json.Marshal(Response{OK: true, Out: raw})
	if err != nil {
		t.Fatal(err)
	}
	var got Response
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Out, raw) {
		t.Fatalf("binary mangled: %v", got.Out)
	}
}

// 超长帧必须能被**识别**出来,而不只是一句文本错误。
//
// 为什么值得一个哨兵错误:client 侧要根据它决定"这条失败要不要重试"。超长帧
// 重发一次还是超长,而重试路径会先删端口文件、再 spawn 一个新 daemon ——
// 为一个必然失败的原因把健康的 daemon 干掉。靠字符串匹配判定太脆(文案一改
// 就静默失效),所以用 errors.Is。
func TestWriteMessageReportsOversizeAsSentinel(t *testing.T) {
	var buf bytes.Buffer
	// 比上限多一个字节。不真分配 64MiB 以上再写:WriteMessage 先判长度再写。
	huge := make([]byte, maxMessageBytes+1)
	err := WriteMessage(&buf, huge)
	if err == nil {
		t.Fatal("超过单帧上限的 payload 必须报错")
	}
	if !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("errors.Is(err, ErrPayloadTooLarge) = false, err = %v", err)
	}
	// 错误文本要保持修前的措辞(有测试/文档按它对照)。
	if !strings.Contains(err.Error(), "ipc: payload too large") {
		t.Fatalf("错误文本变了: %q", err.Error())
	}
	// 失败时一个字节都不该写出去 —— 否则对端会看到半帧。
	if buf.Len() != 0 {
		t.Fatalf("超长帧不该写出任何字节, got %d", buf.Len())
	}
}
