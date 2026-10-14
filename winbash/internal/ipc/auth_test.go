package ipc

import "testing"

// Request 要能携带鉴权 token 与每请求超时 —— 它们是 P0-1(零鉴权 RCE)与
// P1-3(daemon 侧无超时,一条慢命令饿死后续所有命令)的载体。
// 编解码是最基础的一环:字段丢了,后面的修复全是空的。
func TestRequestCarriesTokenAndTimeout(t *testing.T) {
	req := Request{Op: "run", Script: "echo x", Token: "s3cr3t", TimeoutMS: 1500}

	b, err := Encode(req)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeRequest(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.Token != "s3cr3t" {
		t.Fatalf("Token 没往返: %q", got.Token)
	}
	if got.TimeoutMS != 1500 {
		t.Fatalf("TimeoutMS 没往返: %d", got.TimeoutMS)
	}
	if got.Script != "echo x" || got.Op != "run" {
		t.Fatalf("原有字段被改坏: %+v", got)
	}
}

// 鉴权失败要能被 client 认出来,它才好给出"该换 daemon 还是该重试"的处置 ——
// 而不是把 401 当成"脚本跑失败了"。
func TestResponseDistinguishesAuthFailure(t *testing.T) {
	b, err := EncodeResponse(Response{OK: false, Auth: true, Errors: []string{"denied"}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeResponse(b)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Auth {
		t.Fatalf("Auth 标记没往返: %+v", got)
	}
}
