// Package builtins 是四层路由的 ① 层:纯 Go 实现、严格模仿 bash 文本语义的命令。
//
// 这一层存在的理由是「不再翻车」:不依赖进程 PATH、不依赖外部 exe、不受
// MSYS 参数改写影响,输出与 bash 逐字节可比。
package builtins

import (
	"context"
	"io"

	"mvdan.cc/sh/v3/interp"
)

// Builtin 是 ① 层命令的**唯一**形状。
//
// 只有一个形状很重要:注册表里所有命令都必须是它,类型才对得上。
// 需要 stdin 的命令(目前只有 grep)在自己的签名里多接一个 in io.Reader,
// 注册时用适配器从 interp.HandlerCtx(ctx) 取出 stdin 注入 —— 而不是把
// Builtin 放宽成"差不多能兼容"的形状,那会让 Registry 失去意义。
type Builtin func(ctx context.Context, cwd string, argv []string, out io.Writer, errw io.Writer) int

// Registry 把命令名映射到实现。
//
// 名字**故意**用 bash 的命令名而不是 pwsh 的:这样从根上避开了 find/sort/where
// 这类重名冲突 —— 使用者敲 find 时拿到的语义由这里定义,而不是由一个碰巧在
// PATH 里排得更前面的 exe 决定。
//
// 新增 ① 层命令时在这里加一行。
var Registry = map[string]Builtin{
	"ls":   Ls,
	"cat":  catBuiltin,
	"grep": grepBuiltin,
}

// catBuiltin / grepBuiltin 把 Registry 里统一的 Builtin 形状(没有 stdin)
// 补上这两个命令需要的那一个参数。
//
// stdin 从 interp.HandlerCtx 取 —— 也就是「注册表里的命令只会由 ExecHandler 调用」
// 这个前提(routeExec 本来就是这么用 HandlerCtx 的)。
// 有了它,`cat x | grep y`、`echo hi | cat` 的管道完全在 daemon 进程内流转,
// 不落盘、不 spawn。
//
// 两个命令各自需要一个薄适配器,而不是把 Builtin 放宽成"差不多能兼容"的形状 ——
// 后者会让 Registry 失去意义(见 registry.go 顶部的说明)。
func catBuiltin(ctx context.Context, cwd string, argv []string, out, errw io.Writer) int {
	return Cat(ctx, cwd, argv, interp.HandlerCtx(ctx).Stdin, out, errw)
}

func grepBuiltin(ctx context.Context, cwd string, argv []string, out, errw io.Writer) int {
	return Grep(ctx, cwd, argv, interp.HandlerCtx(ctx).Stdin, out, errw)
}
