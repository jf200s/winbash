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

// Request 是客户端发给 daemon 的一条请求。
type Request struct {
	Op     string `json:"op"`
	Script string `json:"script"`
	Cwd    string `json:"cwd"`
	JSON   bool   `json:"json"`
}

// Response 是 daemon 返回给客户端的一条结果。
//
// Out 必须是 []byte 而不是 string:json.Marshal 会把 string 里的非法 UTF-8 字节
// 静默替换成 U+FFFD,而 []byte 走 base64 可以原样往返。这是"二进制管道不被静默
// 损坏"的实现基础,也是本设计"不静默做错事"承诺的一部分。
type Response struct {
	OK     bool     `json:"ok"`
	Out    []byte   `json:"out"`
	Errors []string `json:"errors"`
	Exit   int      `json:"exit"`
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
