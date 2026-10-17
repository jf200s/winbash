#!/usr/bin/env bash
# winbash 端到端回归 —— 从 Git Bash 运行,与这台机器上的真实 bash 对比。
#
#   cd winbash && go build -o winbash.exe ./cmd/winbash && ./smoke-gitbash.sh
#
# **不要从 winbash 会话里启动它**(嵌套保护会让 4 条 daemon 管理断言假红,脚本会自检拒绝)。
# 它用一个自己的临时端点,不会碰你正在用的那个 daemon。
#
# 四类断言对应 spec §10 的测试策略:
#   第一类 AI 常用命令集全部跑通 · 第二类 值正确性(本项目的灵魂) ·
#   第三类 管道一致性(与同一台机器上的 bash 对比) · 第四类 错误路径
#
# 末尾是 Review Focus 第 1 条(含空格与中文的路径)。
#
# 注意:换行必须是 LF。本仓库有 .gitattributes 锁 eol=lf;Git Bash 遇到 CRLF 的 .sh
# 会报 `\r: command not found`。
set -u
cd "$(dirname "$0")" || exit 1

EXE="$(pwd)/winbash.exe"
if [ ! -x "$EXE" ]; then
    echo "找不到 winbash.exe,先跑: go build -o winbash.exe ./cmd/winbash"
    exit 1
fi

# 入口自检:从 winbash 会话里启动本脚本会得到 4 条**假红**(那不是产品缺陷)。
#
# 那四条断言调 --status / --stop,而 client 见到 WINBASH_SESSION 就按嵌套保护拒绝
# ——它要停的正是自己所在的那个 daemon。四条会齐刷刷 FAIL,看着像 daemon 管理坏了,
# 实际是入口用错了。所以这里当场说清楚并以「用法错误」(2)退出,不产出误导性报告。
if [ -n "${WINBASH_SESSION:-}" ]; then
    echo "本脚本不能从 winbash 会话里启动:WINBASH_SESSION 已设。" >&2
    echo "那样 --status / --stop 会被嵌套保护拒绝,本脚本会给出 4 条假红。" >&2
    echo "怎么办: 从普通的 PowerShell 或 Git Bash 窗口直接跑,不要经 winbash 中转。" >&2
    exit 2
fi

# 端点必须隔离:默认端口文件按用户名共享,而本脚本会 --stop ——
# 用默认端点就等于「每跑一次 smoke,顺手停掉使用者正在用的那个 daemon」
# (会话里的变量、set -e、trap 跟着一起没),并行跑两条还会互踩。
# 同一条理由见 cmd/winbash/e2e_test.go 的 benchEndpoint。
# 所有调用统一走 wb();只有 timeout 那几处不能包函数(bash 的函数不是可执行文件),
# 它们在调用点显式写 --endpoint。
EP="${TMPDIR:-/tmp}/wb-smoke-$$.port"
wb() { "$EXE" --endpoint "$EP" "$@"; }

# 临时文件也带 PID:端点隔离之后两条 smoke 可以并行,而共用一个文件名就会互相
# 踩掉对方正在读的文件(表现为 cat 往返偶发 FAIL,看着像 ① 层的 bug)。
TXT="wb-smoke-$$.txt"
BIN="wb-smoke-$$.bin"

fail=0
check() {
    if [ "$2" = "$3" ]; then
        printf 'ok   %-34s %s\n' "$1" "$3"
    else
        printf 'FAIL %-34s expected=%s actual=%s\n' "$1" "$2" "$3"
        fail=1
    fi
}

cleanup() {
    rm -f "$TXT" "$BIN"
    wb --stop >/dev/null 2>&1
    rm -f "$EP"
}
trap cleanup EXIT

# 构建残留会让下面「数含 winbash 的条目」那两条断言多算一个:daemon 常驻时
# `go build` 覆盖运行中的 exe,Windows 会把旧的改名成 winbash.exe~ 留在原地
# (不是偶发)。它不是源码,清掉才算干净的基线;被占用时删不掉也不影响其它断言。
rm -f winbash.exe~ 2>/dev/null

echo "-- 第一类:AI 常用命令集"
check "ls 列出自己"        "1"     "$(wb -c 'ls' | grep -c '^winbash.exe$')"
check "cat 往返"            "hello" "$(wb -c "echo hello > $TXT; cat $TXT")"
check "管道 ls|grep"        "1"     "$(wb -c 'ls | grep -c winbash')"
check "cat 二进制安全"      "5"     "$(wb -c "printf '\000\377\376\001\200' > $BIN; cat $BIN" | wc -c | tr -d ' ')"
check "ls|head|wc 管链"     "3"     "$(wb -c 'ls | head -3 | wc -l' | tr -d ' ')"
check "svc list 三列"       "3"     "$(wb -c 'svc list --running' | head -1 | awk -F'\t' '{print NF}' | tr -d ' ')"

echo "-- 第二类:值正确性(本项目的灵魂)"
check "DWORD 是十进制 10"   "1"     "$(wb -c 'reg get "HKLM\SOFTWARE\Microsoft\Windows NT\CurrentVersion" CurrentMajorVersionNumber' | grep -c '	10$')"
check "输出里没有 0x"       "0"     "$(wb -c 'reg get "HKLM\SOFTWARE\Microsoft\Windows NT\CurrentVersion"' | grep -c '0x')"
check "内存是原始字节"      "1"     "$(wb -c 'sys mem' | grep -cE '^TotalBytes	[0-9]{9,}$')"
check "sys disk 无单位后缀" "0"     "$(wb -c 'sys disk' | grep -cE '\b(GB|MB|KB)\b')"
check "服务状态是可读词"    "1"     "$(wb -c 'svc status W32Time' | grep -cE '^W32Time	(Stopped|Running)	')"
check "StartType 不是数字"  "0"     "$(wb -c 'svc list' | awk -F'\t' '{print $3}' | grep -cE '^[0-9]+$')"
check "ps 内存是纯字节数"   "1"     "$(wb -c 'ps pwsh' | head -1 | grep -cE '^[0-9]+	pwsh	[0-9]+$')"
check "输出无 ANSI 色码"    "0"     "$(wb -c 'svc list --running' | grep -c $'\x1b')"

echo "-- 第三类:管道一致性(与同一台机器上的 bash 对比)"
BASH_BIN="/c/Program Files/Git/bin/bash.exe"
if [ -x "$BASH_BIN" ]; then
    check "wc -l 与 bash 一致"    "$("$BASH_BIN" -c 'cat go.mod | wc -l' | tr -d ' ')" "$(wb -c 'cat go.mod | wc -l' | tr -d ' ')"
    check "grep -c 与 bash 一致"  "$("$BASH_BIN" -c 'cat go.mod | grep -c require' | tr -d ' ')" "$(wb -c 'cat go.mod | grep -c require' | tr -d ' ')"
    check "grep -E 与 bash 一致"  "$("$BASH_BIN" -c "cat go.mod | grep -Ec 'mvdan|golang'" | tr -d ' ')" "$(wb -c "cat go.mod | grep -Ec 'mvdan|golang'" | tr -d ' ')"
else
    echo "skip: 找不到 $BASH_BIN,跳过与 bash 的对比"
fi
check "Git Bash 的 sed 可用" "world" "$(wb -c 'echo hello | sed s/hello/world/')"

echo "-- 语料扫描发现的缺口(逐条与真 bash 对照,自动化版见 internal/shell/corpus_test.go)"
check "find 来自 Git Bash 的 usr/bin" "1" "$(wb -c 'which find' | grep -c 'usr.bin.find')"
check "find -name 不被 MSYS 再展开"   "./go.mod" "$(wb -c "find . -maxdepth 1 -name '*.mod'")"
check "sort 是 GNU 的(不是 System32 的)" "1" "$(wb -c 'sort --version 2>&1 | head -1 | grep -c GNU')"
check "printf | sort 不乱码"          "a"  "$(wb -c "printf 'b\na\n' | sort | head -1")"
check "xargs 能 exec echo"            "2"  "$(wb -c 'echo a b | xargs -n1 echo | wc -l' | tr -d ' ')"
check "command -v 认得 ① 层内建"      "ls" "$(wb -c 'command -v ls')"
check "which 认得四层"                "3"  "$(wb -c 'which ls svc git | wc -l' | tr -d ' ')"
check "锚定替换被拦下"                "2"  "$(wb -c 'v=prefix-x; echo "${v/#prefix/root}"' >/dev/null 2>&1; echo $?)"

echo "-- 第四类:错误路径"
check "未知命令 → 127"       "127"   "$(wb -c 'definitely-not-a-command-xyz' >/dev/null 2>&1; echo $?)"
check "grep 无匹配 → 1"      "1"     "$(wb -c 'ls | grep zzz' >/dev/null 2>&1; echo $?)"
check "svc 不存在 → 非零"    "1"     "$(wb -c 'svc status wb-no-such-service' >/dev/null 2>&1; echo $?)"
check "错误是三行式"         "3"     "$(wb -c 'definitely-not-a-command-xyz' 2>&1 >/dev/null | wc -l | tr -d ' ')"
check "stderr 无 exit status 噪音" "0" "$(wb -c 'ls | grep zzz' 2>&1 >/dev/null | grep -c 'exit status')"
check "printf -v 被拦下"     "1"     "$(wb -c "printf -v x '%s' a" >/dev/null 2>&1; echo $?)"
check "trap DEBUG 被拦下"    "1"     "$(wb -c "trap 'true' DEBUG" >/dev/null 2>&1; echo $?)"
check "trap INT 被拦下"      "1"     "$(wb -c "trap '' INT" >/dev/null 2>&1; echo $?)"
check "trap EXIT 照常工作"   "bye"   "$(wb -c "trap 'echo bye' EXIT; echo hi" | tail -1)"
check "callback 含 DEBUG 不误拦" "ok" "$(wb -c "trap 'echo DEBUG-hit' EXIT; echo ok" | head -1)"
check "进程替换被拦下"       "2"     "$(wb -c 'diff <(echo a) <(echo a)' >/dev/null 2>&1; echo $?)"

echo "-- 生命周期:--stop 之后必须能立刻继续用"
wb --stop >/dev/null 2>&1
check "stop 之后立刻可用"    "ok"    "$(wb -c 'echo ok' 2>&1)"
check "stop 之后状态正确"    "0"     "$(wb --stop >/dev/null 2>&1; echo $?)"
check "没在跑时 status 报错" "1"     "$(wb --status >/dev/null 2>&1; echo $?)"

echo "-- stdin 语义(cat 与 grep 必须一致;这条曾经不一致,而且会让 daemon 崩)"
check "无参数的 cat 立即结束" "0"   "$(wb -c 'cat' >/dev/null 2>&1; echo $?)"
check "cat - 读管道"          "hi"  "$(wb -c 'echo hi | cat -')"
check "echo | cat"            "hi"  "$(wb -c 'echo hi | cat')"
check "cat 串在管链中间"      "3"   "$(wb -c 'printf "a\nb\nc\n" | cat | wc -l' | tr -d ' ')"
check "grep 无文件不崩 daemon" "1"  "$(wb -c 'grep zzz' >/dev/null 2>&1; echo $?)"
check "崩过之后 daemon 还在"  "0"   "$(wb --status >/dev/null 2>&1; echo $?)"

# 流式 stdin(请求头之后跟裸字节流)有两处能悄悄把**串行** handle 占死,
# 而那表现为「后面的命令全都慢/卡」,极难归因 —— 所以都用 timeout 兜着:
# 挂住就返回 124,check 当场 FAIL,而不是让整个 smoke 卡死。
echo "-- 流式 stdin(上游永不 EOF / 命令不读 stdin;这两处会占死串行 handle)"
check "管道内容经 cat 往返"   "hello-from-stdin" "$(printf 'hello-from-stdin\n' | timeout 10 "$EXE" --endpoint "$EP" -c 'cat')"
check "head -1 配 yes 不挂"   "y"                 "$(yes | timeout 10 "$EXE" --endpoint "$EP" -c 'head -1')"
check "命令不读 stdin 不反压" "never-reads"       "$(yes | timeout 10 "$EXE" --endpoint "$EP" -c 'echo never-reads')"
check "大输入经 wc -l"        "30000"             "$(seq 1 30000 | timeout 10 "$EXE" --endpoint "$EP" -c 'wc -l' | tr -d ' ')"
check "流式跑完 daemon 仍活"  "0"                 "$(wb --status >/dev/null 2>&1; echo $?)"

# 嵌套(内层带 WINBASH_SESSION,走 runLocal 本地会话、**不连 daemon**)的 stdin 链路:
# 内层读的是它自己的 os.Stdin,而那是外层 ③ 层派生子进程时接上的 reader。漏了这条
# 就是「外层通、内层空」—— 半能用比完全不能用更费人,所以钉在这里。
echo "-- 嵌套 winbash 的 stdin(内层本地会话,不连 daemon)"
check "嵌套也拿到管道内容"     "nested" "$(printf 'nested\n' | timeout 15 "$EXE" --endpoint "$EP" -c "$EXE -c cat" 2>&1)"
check "嵌套无 stdin 立即结束"   "0"      "$(timeout 15 "$EXE" --endpoint "$EP" -c "$EXE -c cat" </dev/null >/dev/null 2>&1; echo $?)"
check "嵌套之后 daemon 仍活"   "0"      "$(wb --status >/dev/null 2>&1; echo $?)"

echo "-- Review Focus 第 1 条:含空格与中文的路径"
check "中文路径可列"         "1"     "$(wb -c 'ls "D:/work/中文 目录"' | grep -c '^winbash$')"
check "含空格路径可列"       "0"     "$(wb -c 'ls "C:/Users/Test User"' >/dev/null 2>&1; echo $?)"
check "POSIX 路径可列"       "1"     "$(wb -c 'ls /c/Users' | grep -c '^Test User$')"

echo
if [ "$fail" = 0 ]; then
    echo "全部通过"
else
    echo "有失败项(见上面 FAIL 行)"
fi
exit "$fail"
