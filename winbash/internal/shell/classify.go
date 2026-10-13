package shell

import (
	"winbash/internal/builtins"
	"winbash/internal/winmgmt"
)

// layer 是四层路由里的一层,编号与 spec §4 保持一致,便于对着文档读代码。
type layer int

const (
	layerBuiltin layer = iota // ① 纯 Go 内建:ls / cat / grep …
	layerWinmgmt              // ② Go 内建,底层转常驻 pwsh:svc / reg / ps / sys
	layerPath                 // ③ 真实 PATH(含 Git Bash 的 usr\bin)
	layerMissing              // ④ 都没有:报错 + 可操作建议
)

// classify 判定一条命令该走哪一层。
//
// 判定顺序就是优先级,而且 ①/② 命中后就不再去看进程 PATH —— 这是刻意的:
// `ls` 永远是我们的 ls,不是某个碰巧排在前面的 exe。Windows 上 find / sort / where
// 这类重名冲突,根源就是「谁排在 PATH 前面谁说话」;这里的名字由我们自己定义。
//
// ③ 与 ④ 的区分需要搜索路径(Task 12 的 pathsearch),在此之前 classify 只回答
// 前两层的问题,落不到前两层的都先按 ③ 交给默认 handler。
func classify(cmd string) layer {
	if _, ok := builtins.Registry[cmd]; ok {
		return layerBuiltin
	}
	if _, ok := winmgmt.Registry[cmd]; ok {
		return layerWinmgmt
	}
	return layerPath
}
