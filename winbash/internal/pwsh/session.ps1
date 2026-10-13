# winbash ② 层的执行引擎:一个常驻的 pwsh 子进程。
#
# 为什么是「daemon 的子进程 + stdin/stdout 管道」而不是 pwshx 那种命名管道 daemon:
# pwsh 的 ~350ms 启动成本必须只付一次,而 winbash 的 daemon 本来就常驻,让它直接
# 持有这个子进程的两根管道,比再引入一个命名管道少一层(那次用管道是因为 client
# 是独立进程,谁也拿不到别人的 stdin)。代价:self 之外没人能连上这个 pwsh,
# 也就没有第二份会话状态可争。
#
# 协议(两个方向都是):4 字节小端长度 + 该长度的 UTF-8 JSON。
#   请求 : {"op":"run|ping","script":"...","cwd":"..."}
#   响应 : {"ok":bool,"out":"...","errors":[...],"meta":{"pid":N,"calls":N,"ms":N}}
#
# out 一律是 JSON 文本(见文件末尾的包装段),具体怎么渲染由 Go 侧决定 ——
# 这一层只负责「拿到干净的值」,不负责好看。
#
# 约定:② 层的脚本不得用 Write-Host / Write-Output 直接写主机流。
# 那会与本协议的帧交错,污染后续所有响应。脚本应该把对象 return 出来。
# Go 侧对此有防线(帧长不合理即判定连接失效并报错),但最好是不要发生。

$ErrorActionPreference = 'Continue'
try { [Console]::OutputEncoding = [Text.UTF8Encoding]::new($false) } catch {}
try { $OutputEncoding = [Text.UTF8Encoding]::new($false) } catch {}
$ProgressPreference = 'SilentlyContinue'

$__wb_reader = [IO.BinaryReader]::new([Console]::OpenStandardInput())
$__wb_writer = [IO.BinaryWriter]::new([Console]::OpenStandardOutput())
$__wb_calls = 0

while ($true) {
    try {
        # 长度为 0 或读不动 = 父进程关了管道,直接退出(不留僵尸 pwsh)。
        $__wb_len = $__wb_reader.ReadInt32()
        if ($__wb_len -le 0) { break }
        $__wb_body = [Text.Encoding]::UTF8.GetString($__wb_reader.ReadBytes($__wb_len))
        $__wb_req = $__wb_body | ConvertFrom-Json
    } catch {
        break
    }

    $__wb_op = 'run'
    if ($__wb_req.PSObject.Properties.Match('op').Count -gt 0) {
        $__wb_op = [string]$__wb_req.op
    }

    $__wb_ok = $true
    $__wb_errors = @()
    $__wb_text = 'null'
    $__wb_ms = 0

    if ($__wb_op -eq 'ping') {
        $__wb_text = ([ordered]@{ pid = $PID; calls = $__wb_calls; psversion = "$($PSVersionTable.PSVersion)" } | ConvertTo-Json -Compress)
    } else {
        $__wb_sw = [Diagnostics.Stopwatch]::StartNew()

        $__wb_cwdOk = $true
        if ($__wb_req.PSObject.Properties.Match('cwd').Count -gt 0 -and $__wb_req.cwd) {
            try {
                Set-Location -LiteralPath ([string]$__wb_req.cwd) -ErrorAction Stop
            } catch {
                $__wb_cwdOk = $false
                $__wb_errors += "cannot change directory to '$($__wb_req.cwd)': $($_.Exception.Message)"
            }
        }

        $__wb_out = $null
        if ($__wb_cwdOk) {
            $Error.Clear()
            try {
                # dot-source 进当前作用域,变量才会跨调用保持 —— 这正是常驻的意义。
                $__wb_sb = [scriptblock]::Create([string]$__wb_req.script)
                $__wb_out = . $__wb_sb
            } catch {
                # 终止性错误,例如 throw。
                $__wb_errors += $_.Exception.Message
            }
            # 非终止性错误:PowerShell 默认不改退出码,那是「看起来成功了」的典型。
            # 这里显式收集,交给 Go 侧判定失败(spec §7 第 1 条)。
            $__wb_errors += @($Error | ForEach-Object { $_.ToString() })
            $Error.Clear()
        }

        # 逐项序列化再拼,而不是把整个数组交给 ConvertTo-Json:
        # 后者对**单元素**数组会退化成对象([{...}] → {...}),调用方拿到的形状
        # 会随数据条数变化 —— 那正是「值保证」要消灭的东西。
        # 逐项还有个附带好处:一个坏元素不会带走其余好元素(与 output.JSONArray 同一理念)。
        if ($null -ne $__wb_out) {
            if ($__wb_out -is [System.Array]) {
                $__wb_items = @($__wb_out) | ForEach-Object {
                    if ($null -eq $_) { 'null' } else { $_ | ConvertTo-Json -Depth 10 -Compress }
                }
                $__wb_text = '[' + ($__wb_items -join ',') + ']'
            } else {
                $__wb_text = $__wb_out | ConvertTo-Json -Depth 10 -Compress
            }
        }

        $__wb_sw.Stop()
        $__wb_ms = $__wb_sw.ElapsedMilliseconds
        $__wb_calls++
    }

    if ($__wb_errors.Count -gt 0) { $__wb_ok = $false }

    $__wb_resp = [ordered]@{
        ok     = $__wb_ok
        out    = $__wb_text
        errors = @($__wb_errors)
        meta   = [ordered]@{ pid = $PID; calls = $__wb_calls; ms = $__wb_ms }
    } | ConvertTo-Json -Depth 6 -Compress

    try {
        $__wb_bytes = [Text.Encoding]::UTF8.GetBytes($__wb_resp)
        $__wb_writer.Write([int]$__wb_bytes.Length)
        $__wb_writer.Write($__wb_bytes)
        $__wb_writer.Flush()
    } catch {
        break
    }
}

# 走到这里只有两种可能:父进程关了管道(正常关闭),或者读帧失败。
# 两种都不是错误,所以显式 exit 0 —— 否则 PowerShell 会因为最后一条语句的
# $? 为 false 而给出退出码 1,让 Go 侧的 Close() 误报「引擎异常退出」。
exit 0
