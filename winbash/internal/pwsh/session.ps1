# winbash ② 层的执行引擎:一个常驻的 pwsh 子进程。
#
# 为什么是「daemon 的子进程 + stdin/stdout 管道」而不是 pwshx 那种命名管道 daemon:
# pwsh 的 ~350ms 启动成本必须只付一次,而 winbash 的 daemon 本来就常驻,让它直接
# 持有这个子进程的两根管道,比再引入一个命名管道少一层(那次用管道是因为 client
# 是独立进程,谁也拿不到别人的 stdin)。代价:self 之外没人能连上这个 pwsh,
# 也就没有第二份会话状态可争。
#
# 协议(两个方向都是):4 字节小端长度 + 该长度的 UTF-8 JSON。
#   请求 : {"op":"run|ping","script":"...","cwd":"...","env":["KEY=VALUE",...]}
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

        # 准备阶段(cwd、env)是否都成功。任何一项失败就**不跑脚本**:
        # 在错误的目录/错误的环境下执行,会给出「成功但错」的结果,比报错更糟。
        $__wb_cwdOk = $true
        if ($__wb_req.PSObject.Properties.Match('cwd').Count -gt 0 -and $__wb_req.cwd) {
            try {
                # 必须先经 Get-Item 解析成真实路径,不能直接 Set-Location。
                #
                # 原因(实测):Set-Location 会把路径里的 `~` 当 home 展开,连
                # -LiteralPath 都挡不住。而 Windows 的 8.3 短名恰好含 `~` ——
                # daemon 的 cwd 常常落在 `C:\Users\TESTUS~1\AppData\Local\Temp`
                # 这种形态上,于是 Set-Location 报
                #   "An object at the specified path C:\Users\TESTUS~1 does not exist."
                # 而同一路径 Test-Path 明明为 True。这不是措辞问题,是 cmdlet 的行为。
                #
                # 为什么用 Get-Item 而不是 Resolve-Path / Convert-Path:后两个对短名
                # 原样返回(实测),只有 Get-Item 的 FullName 给长名。顺带它也把
                # 尾部分隔符与 `.`/`..` 规范化掉。
                # -ErrorAction Stop 保证路径不存在时**响亮失败**,而不是静默留在
                # 引擎原来的目录里 —— 那正是本次要消灭的形态。
                $__wb_real = (Get-Item -LiteralPath ([string]$__wb_req.cwd) -ErrorAction Stop).FullName
                Set-Location -LiteralPath $__wb_real -ErrorAction Stop
            } catch {
                $__wb_cwdOk = $false
                $__wb_errors += "cannot change directory to '$($__wb_req.cwd)': $($_.Exception.Message)"
            }
        }

        # 环境同步与 cwd 一样,必须是**协议字段**,不能拼成脚本前导文本。
        #
        # 原因(实测):PowerShell 要求 `param(...)`(以及 `#Requires`)是 scriptblock
        # 的**第一条**语句。任何前缀 —— 哪怕只是一句 SetEnvironmentVariable —— 都会
        # 让以 param 开头的脚本报
        #   "The term 'param' is not recognized as a name of a cmdlet..."
        # 而同一个文件用原生 `pwsh -File x.ps1` 完全正常。所以这件事只能在脚本
        # **之外**发生,也就是这一层。
        #
        # 为什么用 [Environment]::SetEnvironmentVariable 而不是 `$env:NAME = ...`:
        # Windows 的环境变量名不限于合法标识符(可以带括号、连字符),走 .NET API
        # 就不会因为名字里的字符而解析失败。
        if ($__wb_req.PSObject.Properties.Match('env').Count -gt 0 -and $__wb_req.env) {
            # @() 包一层:单元素数组经 ConvertFrom-Json 后形状不保证,
            # foreach 需要的是一个可枚举的东西。
            foreach ($__wb_entry in @($__wb_req.env)) {
                $__wb_kv = [string]$__wb_entry
                # 按**第一个** = 切:值里可能还带 =(连接串、base64 参数都是这种)。
                $__wb_i = $__wb_kv.IndexOf('=')
                # i <= 0 同时挡掉两种:空名,以及 Windows 上 `=C:` 这类内部项
                # (它们以 = 开头,既设不进去也不该设)。
                if ($__wb_i -le 0) { continue }
                $__wb_name = $__wb_kv.Substring(0, $__wb_i)
                try {
                    [Environment]::SetEnvironmentVariable($__wb_name, $__wb_kv.Substring($__wb_i + 1))
                } catch {
                    # 响亮失败:静默跳过会让脚本读到一个"看起来对"的旧值/空值,
                    # 那正是这一层存在的意义要消灭的形态。
                    $__wb_cwdOk = $false
                    $__wb_errors += "cannot set environment variable '$__wb_name': $($_.Exception.Message)"
                }
            }
        }

        $__wb_out = $null
        if ($__wb_cwdOk) {
            $Error.Clear()
            $__wb_terminating = ''
            try {
                # dot-source 进当前作用域,变量才会跨调用保持 —— 这正是常驻的意义。
                $__wb_sb = [scriptblock]::Create([string]$__wb_req.script)
                $__wb_out = . $__wb_sb
            } catch {
                # 终止性错误,例如 throw 或 -ErrorAction Stop。
                $__wb_terminating = $_.Exception.Message
            }
            # 非终止性错误:PowerShell 默认不改退出码,那是「看起来成功了」的典型。
            # 这里显式收集,交给 Go 侧判定失败(spec §7 第 1 条)。
            #
            # 排除掉终止性错误那一条:它同时也在 $Error 里,不去重的话同一句话会被报两遍
            # (实测 `Get-Service -Name 不存在 -ErrorAction Stop` 就是这样)。
            $__wb_errors += @($Error | ForEach-Object { $_.ToString() } | Where-Object { $_ -ne $__wb_terminating })
            if ($__wb_terminating) { $__wb_errors += $__wb_terminating }
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
