module winbash

go 1.26.4

require mvdan.cc/sh/v3 v3.14.1

require (
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/term v0.45.0 // indirect
)

// 本地补丁版 mvdan/sh —— 只差两个内建选项,见 third_party/mvdan-sh/WINBASH-PATCH.md:
//   read -d <delim>     上游 v3.14.1 缺失,而它是 pure-bash-bible 的标准写法,
//                        缺了会「报 invalid option 但退出码 0、变量留空」(静默错值)
//   mapfile -n <count>  同上;bible 的 head/tail/lines 三个函数全靠它
// 升级 mvdan/sh 时:把新版本源码覆盖进 third_party/mvdan-sh,再按该文档重打补丁。
replace mvdan.cc/sh/v3 => ./third_party/mvdan-sh
