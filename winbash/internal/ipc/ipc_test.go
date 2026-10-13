package ipc

import (
	"bytes"
	"encoding/json"
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
