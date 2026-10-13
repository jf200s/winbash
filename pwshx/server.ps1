# pwshx daemon — a persistent PowerShell session served over a named pipe.
#
# Protocol (both directions):
#   4-byte little-endian length, then that many bytes of UTF-8 JSON.
#
# Request : {"op":"run|ping|stop","script":"...","cwd":"...","json":false}
# Response: {"ok":bool,"out":"...","errors":[...],"meta":{"pid":N,"calls":N,"ms":N}}
#
# Configuration comes from the environment so that the launcher can pass it
# through `pwsh -EncodedCommand` (which cannot take positional parameters):
#   PWSHX_PIPE  pipe name (required)
#   PWSHX_IDLE  idle timeout in ms before the daemon exits (0 = never)

$ErrorActionPreference = 'Continue'
try { [Console]::OutputEncoding = [Text.UTF8Encoding]::new($false) } catch {}
try { $OutputEncoding = [Text.UTF8Encoding]::new($false) } catch {}
$ProgressPreference = 'SilentlyContinue'

$__pwshx_name = $env:PWSHX_PIPE
if (-not $__pwshx_name) { exit 2 }
# Accept either form; NamedPipeServerStream only takes the bare name.
if ($__pwshx_name.StartsWith('\\.\pipe\')) { $__pwshx_name = $__pwshx_name.Substring(9) }

$__pwshx_idle = 0
if ($env:PWSHX_IDLE) { [void][int]::TryParse($env:PWSHX_IDLE, [ref]$__pwshx_idle) }

$__pwshx_started = Get-Date
$__pwshx_calls = 0

while ($true) {
    try {
        $__pwshx_pipe = [System.IO.Pipes.NamedPipeServerStream]::new(
            $__pwshx_name,
            [System.IO.Pipes.PipeDirection]::InOut,
            1,
            [System.IO.Pipes.PipeTransmissionMode]::Byte,
            [System.IO.Pipes.PipeOptions]::None
        )
    } catch {
        # Another daemon already owns this pipe name. Not an error — but with
        # PWSHX_LOG set, say why, so a real failure is never silent again.
        if ($env:PWSHX_LOG) {
            Add-Content -LiteralPath $env:PWSHX_LOG -Value "pwshx: cannot create pipe '$__pwshx_name': $($_.Exception.Message)"
        }
        exit 0
    }

    try {
        if ($__pwshx_idle -gt 0) {
            $__pwshx_task = $__pwshx_pipe.WaitForConnectionAsync()
            if (-not $__pwshx_task.Wait($__pwshx_idle)) {
                $__pwshx_pipe.Dispose()
                exit 0
            }
        } else {
            $__pwshx_pipe.WaitForConnection()
        }
    } catch {
        try { $__pwshx_pipe.Dispose() } catch {}
        continue
    }

    $__pwshx_reader = [System.IO.BinaryReader]::new($__pwshx_pipe)
    $__pwshx_writer = [System.IO.BinaryWriter]::new($__pwshx_pipe)
    $__pwshx_quit = $false
    $__pwshx_shutdown = $false

    try {
        while (-not $__pwshx_quit) {
            $__pwshx_len = $__pwshx_reader.ReadInt32()
            if ($__pwshx_len -le 0) { break }

            $__pwshx_body = [Text.Encoding]::UTF8.GetString($__pwshx_reader.ReadBytes($__pwshx_len))
            $__pwshx_req = $__pwshx_body | ConvertFrom-Json

            $__pwshx_op = 'run'
            if ($__pwshx_req.PSObject.Properties.Match('op').Count -gt 0) {
                $__pwshx_op = [string]$__pwshx_req.op
            }

            $__pwshx_text = ''
            $__pwshx_errors = @()
            $__pwshx_ms = 0

            switch ($__pwshx_op) {
                'ping' {
                    $__pwshx_uptime = [int]((Get-Date) - $__pwshx_started).TotalSeconds
                    $__pwshx_text = "pwshx daemon`n  pid        : $PID`n  started    : $($__pwshx_started.ToString('yyyy-MM-dd HH:mm:ss'))`n  uptime     : ${__pwshx_uptime}s`n  calls      : $__pwshx_calls`n  pipe       : $__pwshx_name`n  psversion  : $($PSVersionTable.PSVersion)"
                }
                'stop' {
                    $__pwshx_text = "stopping pwshx daemon (pid $PID)"
                    $__pwshx_quit = $true
                    $__pwshx_shutdown = $true
                }
                default {
                    $__pwshx_sw = [Diagnostics.Stopwatch]::StartNew()

                    $__pwshx_cwdOk = $true
                    if ($__pwshx_req.PSObject.Properties.Match('cwd').Count -gt 0 -and $__pwshx_req.cwd) {
                        try {
                            Set-Location -LiteralPath ([string]$__pwshx_req.cwd) -ErrorAction Stop
                        } catch {
                            $__pwshx_cwdOk = $false
                            $__pwshx_errors += "cannot change directory to '$($__pwshx_req.cwd)': $($_.Exception.Message)"
                        }
                    }

                    $__pwshx_out = $null
                    if ($__pwshx_cwdOk) {
                        $Error.Clear()
                        try {
                            # Dot-source into THIS scope so user assignments persist
                            # across calls — that is the whole point of a daemon.
                            $__pwshx_sb = [scriptblock]::Create([string]$__pwshx_req.script)
                            $__pwshx_out = . $__pwshx_sb
                        } catch {
                            $__pwshx_errors += $_.Exception.Message
                        }
                        $__pwshx_errors += @($Error | ForEach-Object { $_.ToString() })
                        $Error.Clear()
                    }

                    if ($__pwshx_req.PSObject.Properties.Match('json').Count -gt 0 -and $__pwshx_req.json) {
                        if ($null -eq $__pwshx_out) {
                            $__pwshx_text = '[]'
                        } else {
                            # Serialize item by item and join. Feeding an array to
                            # ConvertTo-Json -InputObject treats it as ONE object,
                            # which nests the result ([ [..] ] instead of [ .. ]).
                            $__pwshx_items = @($__pwshx_out) | ForEach-Object {
                                if ($null -eq $_) { 'null' } else { $_ | ConvertTo-Json -Depth 10 -Compress }
                            }
                            $__pwshx_text = '[' + ($__pwshx_items -join ',') + ']'
                        }
                    } elseif ($null -ne $__pwshx_out) {
                        $__pwshx_text = ($__pwshx_out | Out-String)
                    }

                    $__pwshx_sw.Stop()
                    $__pwshx_ms = $__pwshx_sw.ElapsedMilliseconds
                    $__pwshx_calls++
                }
            }

            $__pwshx_resp = [ordered]@{
                ok     = ($__pwshx_errors.Count -eq 0)
                out    = $__pwshx_text
                errors = @($__pwshx_errors)
                meta   = [ordered]@{ pid = $PID; calls = $__pwshx_calls; ms = $__pwshx_ms }
            } | ConvertTo-Json -Depth 6 -Compress

            $__pwshx_bytes = [Text.Encoding]::UTF8.GetBytes($__pwshx_resp)
            $__pwshx_writer.Write([int]$__pwshx_bytes.Length)
            $__pwshx_writer.Write($__pwshx_bytes)
            $__pwshx_writer.Flush()
        }
    } catch {
        # Client went away mid-read.
    } finally {
        try { $__pwshx_reader.Dispose() } catch {}
        try { $__pwshx_writer.Dispose() } catch {}
        try { $__pwshx_pipe.Dispose() } catch {}
    }

    # 'stop' asked us to go away. Without this the outer loop would just
    # re-create the pipe and keep listening, so --stop would never work.
    if ($__pwshx_shutdown) { exit 0 }
}
