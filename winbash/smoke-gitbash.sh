#!/usr/bin/env bash
# winbash 端到端回归 —— 从 Git Bash 运行,与这台机器上的真实 bash 对比。
#
#   cd winbash && go build -o winbash.exe ./cmd/winbash && ./smoke-gitbash.sh
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
    rm -f wb-smoke.txt wb-smoke.bin
    "$EXE" --stop >/dev/null 2>&1
}
trap cleanup EXIT

echo "-- 第一类:AI 常用命令集"
check "ls 列出自己"        "1"     "$($EXE -c 'ls' | grep -c '^winbash.exe$')"
check "cat 往返"            "hello" "$($EXE -c 'echo hello > wb-smoke.txt; cat wb-smoke.txt')"
check "管道 ls|grep"        "1"     "$($EXE -c 'ls | grep -c winbash')"
check "cat 二进制安全"      "5"     "$($EXE -c 'printf "\000\377\376\001\200" > wb-smoke.bin; cat wb-smoke.bin' | wc -c | tr -d ' ')"
check "ls|head|wc 管链"     "3"     "$($EXE -c 'ls | head -3 | wc -l' | tr -d ' ')"
check "svc list 三列"       "3"     "$($EXE -c 'svc list --running' | head -1 | awk -F'\t' '{print NF}' | tr -d ' ')"

echo "-- 第二类:值正确性(本项目的灵魂)"
check "DWORD 是十进制 10"   "1"     "$($EXE -c 'reg get "HKLM\SOFTWARE\Microsoft\Windows NT\CurrentVersion" CurrentMajorVersionNumber' | grep -c '	10$')"
check "输出里没有 0x"       "0"     "$($EXE -c 'reg get "HKLM\SOFTWARE\Microsoft\Windows NT\CurrentVersion"' | grep -c '0x')"
check "内存是原始字节"      "1"     "$($EXE -c 'sys mem' | grep -cE '^TotalBytes	[0-9]{9,}$')"
check "sys disk 无单位后缀" "0"     "$($EXE -c 'sys disk' | grep -cE '\b(GB|MB|KB)\b')"
check "服务状态是可读词"    "1"     "$($EXE -c 'svc status W32Time' | grep -cE '^W32Time	(Stopped|Running)	')"
check "StartType 不是数字"  "0"     "$($EXE -c 'svc list' | awk -F'\t' '{print $3}' | grep -cE '^[0-9]+$')"
check "ps 内存是纯字节数"   "1"     "$($EXE -c 'ps pwsh' | head -1 | grep -cE '^[0-9]+	pwsh	[0-9]+$')"
check "输出无 ANSI 色码"    "0"     "$($EXE -c 'svc list --running' | grep -c $'\x1b')"

echo "-- 第三类:管道一致性(与同一台机器上的 bash 对比)"
BASH_BIN="/c/Program Files/Git/bin/bash.exe"
if [ -x "$BASH_BIN" ]; then
    check "wc -l 与 bash 一致"    "$("$BASH_BIN" -c 'cat go.mod | wc -l' | tr -d ' ')" "$($EXE -c 'cat go.mod | wc -l' | tr -d ' ')"
    check "grep -c 与 bash 一致"  "$("$BASH_BIN" -c 'cat go.mod | grep -c require' | tr -d ' ')" "$($EXE -c 'cat go.mod | grep -c require' | tr -d ' ')"
    check "grep -E 与 bash 一致"  "$("$BASH_BIN" -c "cat go.mod | grep -Ec 'mvdan|golang'" | tr -d ' ')" "$($EXE -c "cat go.mod | grep -Ec 'mvdan|golang'" | tr -d ' ')"
else
    echo "skip: 找不到 $BASH_BIN,跳过与 bash 的对比"
fi
check "Git Bash 的 sed 可用" "world" "$($EXE -c 'echo hello | sed s/hello/world/')"

echo "-- 第四类:错误路径"
check "未知命令 → 127"       "127"   "$($EXE -c 'definitely-not-a-command-xyz' >/dev/null 2>&1; echo $?)"
check "grep 无匹配 → 1"      "1"     "$($EXE -c 'ls | grep zzz' >/dev/null 2>&1; echo $?)"
check "svc 不存在 → 非零"    "1"     "$($EXE -c 'svc status wb-no-such-service' >/dev/null 2>&1; echo $?)"
check "错误是三行式"         "3"     "$($EXE -c 'definitely-not-a-command-xyz' 2>&1 >/dev/null | wc -l | tr -d ' ')"
check "stderr 无 exit status 噪音" "0" "$($EXE -c 'ls | grep zzz' 2>&1 >/dev/null | grep -c 'exit status')"
check "printf -v 被拦下"     "1"     "$($EXE -c "printf -v x '%s' a" >/dev/null 2>&1; echo $?)"
check "进程替换被拦下"       "2"     "$($EXE -c 'diff <(echo a) <(echo a)' >/dev/null 2>&1; echo $?)"

echo "-- 生命周期:--stop 之后必须能立刻继续用"
$EXE --stop >/dev/null 2>&1
check "stop 之后立刻可用"    "ok"    "$($EXE -c 'echo ok' 2>&1)"
check "stop 之后状态正确"    "0"     "$($EXE --stop >/dev/null 2>&1; echo $?)"
check "没在跑时 status 报错" "1"     "$($EXE --status >/dev/null 2>&1; echo $?)"

echo "-- stdin 语义(cat 与 grep 必须一致;这条曾经不一致,而且会让 daemon 崩)"
check "无参数的 cat 立即结束" "0"   "$($EXE -c 'cat' >/dev/null 2>&1; echo $?)"
check "cat - 读管道"          "hi"  "$($EXE -c 'echo hi | cat -')"
check "echo | cat"            "hi"  "$($EXE -c 'echo hi | cat')"
check "cat 串在管链中间"      "3"   "$($EXE -c 'printf "a\nb\nc\n" | cat | wc -l' | tr -d ' ')"
check "grep 无文件不崩 daemon" "1"  "$($EXE -c 'grep zzz' >/dev/null 2>&1; echo $?)"
check "崩过之后 daemon 还在"  "0"   "$($EXE --status >/dev/null 2>&1; echo $?)"

echo "-- Review Focus 第 1 条:含空格与中文的路径"
check "中文路径可列"         "1"     "$($EXE -c 'ls "D:/work/中文 目录"' | grep -c '^winbash$')"
check "含空格路径可列"       "0"     "$($EXE -c 'ls "C:/Users/Test User"' >/dev/null 2>&1; echo $?)"
check "POSIX 路径可列"       "1"     "$($EXE -c 'ls /c/Users' | grep -c '^Test User$')"

echo
if [ "$fail" = 0 ]; then
    echo "全部通过"
else
    echo "有失败项(见上面 FAIL 行)"
fi
exit "$fail"
