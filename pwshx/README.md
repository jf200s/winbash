# pwshx

在 Git Bash 里用 bash 语法调 PowerShell,**不用付它 ~350ms 的启动成本**。

它不是一个新 shell,也不是对 Git Bash 的任何改造。它只是 PATH 里多出来的一个 exe —— 和
`ipconfig.exe` 完全同一种东西 —— 背后替你守着一个常驻的 `pwsh` 进程。

```bash
$ pwshx -c 'Get-Process | Sort-Object CPU -Descending | Select-Object -First 3 Name, CPU'
Name        CPU
----        ---
chrome    812.34
Code      401.02
explorer   88.11
```

## 为什么是这个形态

启动 `pwsh` 一次要 ~350ms,这是它的硬成本。Git Bash 慢也从来不是因为"它是 bash",而是因为
Windows 没有 `fork()`,MSYS2 得模拟。所以提速的着力点不是重写 shell,而是**别每次都启动解释器**。

一个常驻的 pwsh 进程 + 一个 15ms 的轻客户端,就能把成本从"每次 350ms"变成"一次 350ms"。

## 实测数据(本机 Windows 11)

下面这组是在 **PowerShell** 里测的。从 Git Bash 里调用时,MSYS 自己还要收一份 spawn 开销,
数字会不同 —— 见下一节。

| | 每次调用 |
|---|---|
| `pwsh -NoProfile -Command '1'` | **350 ms** |
| `powershell.exe`(5.1) | 265 ms |
| `reg.exe query`(原生 exe 参照) | 13 ms |
| **pwshx(daemon 已起)** | **17 ms** |
| └ 其中:Go 客户端进程启动 | 15 ms |
| └ 其中:命名管道往返 | 1–2 ms |
| pwshx(首次调用,含拉起 daemon) | 510 ms |

两点值得说清楚:

- **17ms 里 15ms 是 Windows 创建进程本身**,不是我们的代码慢。原生 `reg.exe` 也是 13ms。
  要再快就得连客户端进程也省掉,那收益很小而复杂度很高。
- **但在 Git Bash 里净提升是 ~9.5×,不是 20×**:MSYS 自己调外部 exe 也要收一份开销,
  351ms → 37ms。见下一节。
- 批量场景差距更大:因为 daemon 是**同一个会话**,一次调用就能办完所有事
  (见下面「一次调用做完所有事」)。

## 和 Git Bash、裸 pwsh 的对照

先确定一件事:**pwshx 就是 pwsh**,没多一个 cmdlet,也没少一个。它买到的只有一样东西:速度。
所以下面只比开销和输出,能力请查 pwsh 自己的文档。

### 在 Git Bash 里,你实际要付的代价

| 方式 | 每次调用 |
|---|---|
| `pwsh -NoProfile -Command '1'` | **351 ms** |
| `pwshx -c '1'`(daemon 已起) | **37 ms** |
| `ls`(MSYS2 自带) | 27 ms |
| `grep` | 48 ms |

pwshx 的开销落在 `ls` 和 `grep` 之间 —— 用它做 Windows 管理,贵不过敲一个 `grep`。

### 同一件事,三种写法

```bash
# 查一个服务
sc query W32Time
#   STATE : 1  STOPPED          ← 1 是常量码,要自己查表
pwsh -NoProfile -c 'Get-Service W32Time'
pwshx -c 'Get-Service W32Time'
#   Status: Stopped

# 看事件日志
wevtutil qe System //c:2 //f:text
#   Event[0] / Log Name: ... / 多行缩进文本,难解析
pwshx -c 'Get-WinEvent -LogName System -MaxEvents 2'

# 看进程
tasklist //FI "IMAGENAME eq pwsh.exe"
#   WorkingSet 是 "86,488 K" 这样的字符串
pwshx --json -c 'Get-Process pwsh | Select-Object Id, WorkingSet64'
#   [{"Id":32256,"WorkingSet64":88563712}]   ← 纯数字

# 查注册表
reg query "HKLM\SOFTWARE\Microsoft\Windows NT\CurrentVersion" //v CurrentMajorVersionNumber
#   CurrentMajorVersionNumber    REG_DWORD    0xa     ← 十六进制字符串
pwshx --json -c '(Get-ItemProperty "HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion").CurrentMajorVersionNumber'
#   [10]                                              ← 真数字
```

注意那两个 `//`:**Git Bash 里调原生 exe 时,单斜杠参数会被 MSYS 当成路径改写**,
`/v` 和 `/c:2` 必须写成 `//v`、`//c:2`(或者 `MSYS2_ARG_CONV_EXCL='*'`),
否则 `reg query` 只会回你一句 `ERROR: Invalid syntax.`。这不是 pwshx 的问题,是 Git Bash 固有的。

### pwsh 和 pwshx 之间,只有两个区别

| | pwsh(裸) | pwshx |
|---|---|---|
| 每次调用 | 351 ms | 37 ms |
| 输出 | 带 ANSI 色码 | 纯文本 |
| 循环 50 次 | ~17.5 s | ~1.9 s |

ANSI 那条对管道是好事:daemon 没有终端,`Out-String` 就不上色,输出可以直接喂给 `grep`/`jq`。

**什么时候还是直接用 pwsh**:就想跑一次、要看彩色输出、或者要进交互式 REPL。
**什么时候用 pwshx**:bash 脚本里、循环里、输出要进管道。

所以真正的三方关系不是三个工具,而是两个世界:Git Bash 的**文本世界**,和 PowerShell 的
**对象世界**。pwshx 没造第三个世界,它只是让第二个在 bash 里变得负担得起。

## 构建

```bash
cd pwshx
go build -o pwshx.exe .
```

单文件、零依赖(只用 Go 标准库)。`server.ps1` 通过 `go:embed` 编进 exe,所以 exe 可以单独拷走。

## 用法

```bash
pwshx -c '<script>'        # 执行脚本
pwshx '<script>'           # 同上(位置参数)
pwshx -f script.ps1        # 从文件读
echo '<script>' | pwshx    # 从 stdin 读
pwshx --json -c '...'      # 输出 JSON(机器可读)
pwshx --status             # 看 daemon 状态(pid / uptime / 调用次数)
pwshx --stop               # 停掉 daemon
```

| Flag | 说明 |
|---|---|
| `-c, --script <s>` | 要执行的脚本 |
| `-f, --file <path>` | 从文件读脚本 |
| `-j, --json` | 输出 JSON 数组,而不是格式化文本 |
| `--cwd <path>` | 指定运行目录(默认:调用方的当前目录) |
| `--no-cwd-sync` | 不改变 daemon 的工作目录 |
| `--timeout <s>` | 单次请求超时(默认 300,0 = 永久) |
| `--idle <min>` | daemon 空闲多久后退出(默认 30,0 = 永不) |
| `--serve` | 前台运行 daemon(调试用) |
| `-q, --quiet` | 不打印 daemon 启动提示 |

想少打几个字:

```bash
alias ps='pwshx.exe'
# 或者把 pwshx.exe 放进 PATH 里的某个目录
```

## 一个 pwsh 做不到的事,和一件 daemon 会弄丢、这里补回来的事

**1. 变量跨调用保持 —— pwsh 做不到。** daemon 是一个会话,用户脚本被 dot-source 进同一个作用域:

```bash
$ pwshx -c '$cfg = Get-Content config.json | ConvertFrom-Json'
$ pwshx -c '$cfg.name'          # 上一次的变量还在
my-service
```

**2. 工作目录跟随 bash —— 直接调 pwsh 本来就自动继承,但常驻进程会弄丢。**
daemon 只有一个工作目录,不处理的话它会永远停在启动时那个目录。所以每次调用都把调用方的
cwd 同步过去:

```bash
cd /d/some/project
$ pwshx -c '(Get-Location).Path'
D:\some\project
```

不想要这个行为就用 `--no-cwd-sync`。

## JSON 输出

`--json` 把结果序列化成 JSON 数组。它解决的是**类型丢失**:`reg.exe`、`sc.exe`、`tasklist`
给你的值是 `0xa`(十六进制)、`1`(常量码)、`86,488 K`(带千分位的字符串),都得自己解析;
`--json` 给的是真正的数字。它**总是**输出数组,消费端可预测:

```bash
$ pwshx --json -c 'Get-ChildItem *.go | Select-Object Name, Length'
[{"Name":"main.go","Length":11305}]

$ pwshx --json -c '1..3'
[1,2,3]

$ pwshx --json -c '$null'
[]

$ pwshx --json -c '(Get-ItemProperty "HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion").CurrentMajorVersionNumber'
[10]
```

配合 `jq`:

```bash
pwshx --json -c 'Get-Service | Where-Object Status -eq Running' | jq -r '.[].DisplayName'
```

## 它是怎么工作的

```
bash ──► pwshx.exe ──┐
bash ──► pwshx.exe ──┤   named pipe   ┌──────────────┐
bash ──► pwshx.exe ──┼───────────────►│ pwsh daemon  │  一个常驻会话
                      ┘  \\.\pipe\    └──────────────┘
                         pwshx-<user>
```

1. 客户端先尝试连 `\\.\pipe\pwshx-<USERNAME>`。
2. 连不上且不是"忙"就拉起 daemon(`pwsh -EncodedCommand <server.ps1>`,无窗口),
   然后重试到连上。
3. 请求/响应都是 **4 字节小端长度 + UTF-8 JSON**。
4. daemon 用**裸管道名**建管道 —— `NamedPipeServerStream` 不接受 `\\.\pipe\` 前缀,这一点
   弄错会静默失败(设 `PWSHX_LOG` 可以看到具体原因)。
5. daemon 每次连接结束就重建管道并重新 accept,所以客户端进程退出不影响它。

**并发**:管道实例数为 1,请求天然串行。第二个并发调用会遇到 `ERROR_PIPE_BUSY`,它会**等待**
而不是再拉起一个 daemon(否则白白烧掉一个 350ms 的 pwsh 启动)。

**空闲退出**:默认 30 分钟没有请求就自己退出,不留僵尸进程。

## 限制(重要)

- **`|` 仍然是字节流,不是 PowerShell 的对象管道。** 这是 bash 的语义,不该也不能改。要结构化
  数据用 `--json`,而不是指望 `Select-Object` 穿过管道。这是本方案与 pwsh 的**根本差别**。
- **需要 pwsh 7。** 只在 PowerShell 7.6.3 上验证过,不支持 Windows PowerShell 5.1。
- **daemon 按用户名命名并共享。** 同一用户下所有 Git Bash 窗口共用同一个会话,也就共享变量和
  当前目录 —— 这是特性也是坑(两个窗口互相覆盖变量)。要隔离就自己改 `pipeName()`。
- **脚本里 `exit` 会杀掉 daemon。** 下次调用会重启,状态丢失。
- **每次调用前会清空 `$Error`。** 那是用来收集非终止错误的。
- **只验证过 Windows 11。** 进程创建成本(Git Bash 里 37ms)和管道行为都跟平台强相关。

## 一次调用做完所有事

因为 daemon 是一个会话,循环调用很可能是错的写法:

```bash
# 慢:1000 次进程启动(37ms 起步)
for f in *.log; do pwshx -c "(Get-Item '$f').Length"; done

# 快:一次调用,几百个文件几毫秒
pwshx --json -c 'Get-ChildItem *.log | Select-Object Name, Length'
```

## 开发

```bash
# 回归测试(从 Git Bash 跑,15 项断言)
bash smoke-gitbash.sh

# daemon 起不来时看日志
PWSHX_LOG=/tmp/pwshx.log pwshx -c '1'
cat /tmp/pwshx.log

# 前台跑 daemon,直接看它输出
pwshx --serve
```

文件就四个:

| 文件 | 作用 |
|---|---|
| `main.go` | 客户端:CLI、协议、拉起 daemon |
| `server.ps1` | daemon:accept 循环、执行脚本、序列化结果(被 embed 进 exe) |
| `smoke-gitbash.sh` | 从 Git Bash 跑的回归测试 |
| `go.mod` | 没有第三方依赖 |
