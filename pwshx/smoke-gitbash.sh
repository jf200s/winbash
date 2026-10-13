#!/usr/bin/env bash
# Smoke test for pwshx, run from Git Bash (the target environment).
#   bash smoke-gitbash.sh
set -u

cd "$(dirname "$0")" || exit 1
EXE="$(pwd)/pwshx.exe"
fail=0

check() { # <name> <expected> <actual>
  if [ "$2" = "$3" ]; then
    printf 'ok   %-30s %s\n' "$1" "$3"
  else
    printf 'FAIL %-30s expected=%s actual=%s\n' "$1" "$2" "$3"
    fail=1
  fi
}

# --- basic invocation -------------------------------------------------------
check "version" "7.6.3" "$($EXE -c '$PSVersionTable.PSVersion.ToString()')"

# --- session state persists across separate invocations ---------------------
$EXE -c '$smoke = 41' >/dev/null
check "state persists" "42" "$($EXE -c '$smoke + 1')"

# --- the daemon follows the client's working directory ----------------------
cd /c/Windows || exit 1
check "cwd sync (C:/Windows)" 'C:\Windows' "$($EXE -c '(Get-Location).Path')"
cd /c/ || exit 1
check "cwd sync (C: root)" 'C:\' "$($EXE -c '(Get-Location).Path')"
cd "$(dirname "$0")" || exit 1

# --- usable in a pipeline ---------------------------------------------------
check "pipes into grep" "1" "$($EXE -c '1..5' | grep -c '^3$')"

# --- concurrent invocations share one daemon --------------------------------
out=$(for i in 1 2 3 4 5; do ( $EXE -c "$i * 10" ) & done; wait)
check "concurrent calls" "10 20 30 40 50" "$(printf '%s\n' $out | sort -n | tr '\n' ' ' | sed 's/ $//')"

# --- UTF-8 round trip (the thing reg.exe gets wrong) ------------------------
check "utf8 chinese" "中文测试 ✓" "$($EXE -c '"中文测试 ✓"')"

# --- machine-readable output ------------------------------------------------
check "json: range" "[1,2,3]" "$($EXE --json -c '1..3')"
check "json: object" '[{"Name":"x","Id":1}]' "$($EXE --json -c '[pscustomobject]@{Name="x";Id=1}')"
check "json: nothing" "[]" "$($EXE --json -c '$null')"

# --- script from stdin ------------------------------------------------------
check "stdin script" "30" "$(printf '$a = 5\n$b = 6\n$a * $b\n' | $EXE)"

# --- script from file -------------------------------------------------------
tmp=$(mktemp)
printf '"from file: %s"\n' 'ok' >"$tmp"
check "script from file" "from file: ok" "$($EXE -f "$tmp")"
rm -f "$tmp"

# --- errors surface with a non-zero exit code -------------------------------
$EXE -c 'Get-Item C:\definitely\not\here' >/dev/null 2>&1
check "exit code on error" "1" "$?"

# --- real Windows management, which is the point ----------------------------
# Win11 reports CurrentMajorVersionNumber = 10.
v=$($EXE --json -c '(Get-ItemProperty "HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion").CurrentMajorVersionNumber')
case "$v" in
  \[[0-9]*\]) printf 'ok   %-30s %s\n' "registry read" "$v" ;;
  *) printf 'FAIL %-30s actual=%s\n' "registry read" "$v"; fail=1 ;;
esac

# --- --stop really stops it (last: this kills the daemon) -------------------
$EXE --stop >/dev/null 2>&1
sleep 1
st=$($EXE --status 2>&1)
case "$st" in
  *"not running"*) printf 'ok   %-30s %s\n' "--stop stops daemon" "$st" ;;
  *) printf 'FAIL %-30s actual=%s\n' "--stop stops daemon" "$st"; fail=1 ;;
esac

if [ "$fail" -eq 0 ]; then
  echo "ALL PASS"
else
  echo "FAILURES"
fi
exit "$fail"
