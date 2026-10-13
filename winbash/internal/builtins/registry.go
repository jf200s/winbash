// Package builtins 是四层路由的 ① 层:纯 Go 实现、严格模仿 bash 文本语义的命令。
//
// 这一层存在的理由是「不再翻车」:不依赖进程 PATH、不依赖外部 exe、不受
// MSYS 参数改写影响,输出与 bash 逐字节可比。
package builtins

import (
	"context"
	"io"
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
// 新增 ① 层命令时在这里加一行。Task 5/6 会补上 cat 与 grep。
var Registry = map[string]Builtin{
	"ls": Ls,
}
