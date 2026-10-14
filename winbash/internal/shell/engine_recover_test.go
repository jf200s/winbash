package shell

import (
	"context"
	"testing"
)

// ★ P1-1:② 层引擎坏掉之后必须能自愈。
//
// 修之前:pwsh.Conn 的 dead 标志只置位不清除,而 psEngine 是
// 「if s.engine != nil { return s.engine }」的永久缓存 —— 于是脚本里一句
// Write-Host 就会污染协议帧,报"响应帧长度不合理",此后该 daemon 的所有 ② 层
// 命令(svc/reg/ps/sys/pwsh)永久失效,只能重启 daemon(实测)。
func TestLayerTwoRecoversAfterEngineFailure(t *testing.T) {
	s := NewSession()
	defer s.Close()

	// 先把引擎用起来,确认 ② 层本来是好的
	if _, exit, errs := s.Run(context.Background(), "sys os", false); exit != 0 {
		t.Skipf("② 层在这台机器上不可用,跳过: %v", errs)
	}

	// 用它污染协议帧:Write-Host 往主机流写东西,读帧就会失步
	s.Run(context.Background(), `pwsh -c "Write-Host POLLUTION"`, false)

	// 再跑一条 ② 层命令 —— 应当重建引擎后成功,而不是永久报"会话已失效"
	out, exit, errs := s.Run(context.Background(), "sys os", false)
	if exit != 0 {
		t.Fatalf("引擎失效后没有重建,② 层永久瘫痪了: %v", errs)
	}
	if out == "" {
		t.Fatal("重建后没有输出")
	}
}
