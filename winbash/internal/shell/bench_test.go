package shell

import (
	"context"
	"testing"
)

// BenchmarkSessionRun 在**进程内**度量一条命令的开销。
//
// 为什么不用端到端计时:本机启动一个进程约 700ms(`cmd.exe` 空转基线),它把
// winbash 真正的信号整个淹掉 —— 外部计时量到的是"创建进程有多慢",不是 winbash
// 本身快不快。把度量搬进进程里,测的就只是 interp 的一次 Run。
//
// 这正是审计报告对 README 那两行(首次 70ms / 之后 22ms)的批评:端到端计时
// 既证实不了也证伪不了它们。这个 benchmark 给的是能复现的数。
func BenchmarkSessionRun(b *testing.B) {
	s := NewSession()
	defer s.Close()

	ctx := context.Background()
	if _, exit, _ := s.Run(ctx, "echo warmup", false); exit != 0 {
		b.Fatal("warmup failed")
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, exit, _ := s.Run(ctx, "echo hello", false); exit != 0 {
			b.Fatal("run failed")
		}
	}
}

// BenchmarkSessionRunPipeline 度量一条真管道(两个内建 + 一次管道连接),
// 比裸 echo 更接近 AI 实际写出来的命令。
func BenchmarkSessionRunPipeline(b *testing.B) {
	s := NewSession()
	defer s.Close()

	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, exit, _ := s.Run(ctx, "echo hello | grep hell", false); exit != 0 {
			b.Fatal("pipeline failed")
		}
	}
}
