# winbash — 设计文档

日期:2026-10-13
状态:待评审

---

## 1. 背景:问题是被实测出来的,不是猜的

AI agent 在 Linux 上能可靠干活,在 Windows 上不行。根因不是"AI 不熟 Windows",而是
**Windows 不是一个环境,而是 N 个环境的重叠**,而 AI 的本事建立在"假设正确"之上。

以下全部在本机(Windows 11)实测:

| 事实 | 实测结果 |
|---|---|
| `bash` 是什么 | `C:\Windows\system32\bash.exe`,一个**已损坏的 WSL relay**:`bash --version` 直接报 `execvpe(/bin/bash) failed`。AI 的肌肉记忆 `bash script.sh` 在这里必然失败 |
| 真 Git Bash 在哪 | `C:\Program Files\Git\bin\bash.exe`(bash 5.3.9),**不在 PATH 里** |
| `pwsh` 在哪 | `C:\Users\Test User\Applications\PowerShell-7\pwsh.exe`,非标准路径 |
| 同时存在的解释器 | `cmd.exe` / `powershell.exe`(5.1)/ `pwsh.exe`(7.6.3)/ WSL / Git Bash —— 五种,语法互不兼容 |
| MSYS 参数改写 | `reg query … /v X` → `ERROR: Invalid syntax.`;必须写 `//v`。错误信息不提原因 |
| PowerShell 退出码 | non-terminating error **不改变退出码**,AI 会误判成功 |
| 值的类型丢失 | `0xa`(十六进制)/ `STATE: 1`(常量码)/ `86,488 K`(带千分位字符串) |
| pwsh 输出 | stdin 为终端时带 ANSI 色码 |
| 每次调用成本 | 冷启 `pwsh` ≈ 351 ms(Git Bash 里测得);`ls` ≈ 27 ms;`grep` ≈ 48 ms |
| 无会话连续性 | 每次调用是新进程,cwd 与变量全部丢失 |

关键推论:**AI 失败在"第一秒的方向"上,而不是"速度"上。**

## 2. 目标 / 非目标 / 成功判据

### 目标

做一个 **bash 语法 + pwsh 能力** 的 shell,名为 `winbash`,作为与 `bash`/`pwsh` 并列的
**第三个可选项**,由 agent 或用户自行选择使用。

### 成功判据(用户确认的优先级)

1. **不再翻车**(主判据)—— 不因为猜错环境而失败;错误信息可操作
2. **不等待** —— 一次任务上百条命令的总耗时从几十秒降到几秒
3. **拿到结构化数据** —— 值没有歧义(`10` 而不是 `0xa`)

**不是**判据:"有记忆"(会话状态保持)。状态保持是本设计的自然副产品,但不是验收目标。

### 非目标

- **不做 agent 集成层**。不写配置、不做 MCP、不改 agent 的 shell 工具。工具做好即可,
  用不用、怎么用是用户的选择。
- **不做交互式 REPL**。目标是非交互式执行。
- **不追求 100% bash 兼容**。目标是"AI 熟悉的那个子集",不保证跑别人的 `.sh` 脚本。
- **不做 pwsh 对象管道**。`|` 的语义是 bash 的字节流,不可协商。

## 3. 定位

`winbash` 让 AI **不必在"我熟悉的语法"和"够用的能力"之间做取舍** —— 这正是当前翻车的根源:
它熟悉 bash,但 bash 管不了 Windows;pwsh 管得了,但它不熟语法、而且慢。

## 4. 架构与执行模型

```
winbash -c 'ls -la | grep go'
   │
   ▼ 常驻 daemon（进程内，只启动一次）
mvdan/sh syntax 解析  →  mvdan/sh interp 执行
                          （管道/重定向/变量/$? 的 bash 语义已现成）
   │
   ▼ 每条命令过 ExecHandler，按顺序路由
   ├─ ① 纯 Go 内建        ls/cd/cat/echo/cp/mv/rm/mkdir/pwd/head/tail/wc …
   ├─ ② Go 内建，底层转 pwsh daemon
   │                      reg / svc / ps / event / task / cim / sys
   ├─ ③ 真实 PATH         git / go / curl …（以及 Git Bash 的 /usr/bin）
   └─ ④ 都没有            报错 + 可操作建议
```

### 决策 1:Windows 能力用 bash 风格命令名暴露,不转发 pwsh 语法

`svc list`、`reg get …`、`ps` —— 而不是让 AI 写 `Get-Service`。理由:

- AI 只需记**一套**语法,不必在两个世界间切换
- 命令名由我们定,从根上避开 Unix 重名冲突(`find`/`sort`/`where` 那个坑)
- 输出形状由我们决定,结构化是默认而非可选项

### 决策 2:② 层的语法是我们的,执行引擎是常驻 pwsh

`svc list` 的**语法**由我们定义,干活的是常驻 pwsh(`Get-Service | ConvertTo-Json`)。
开发快,覆盖面一开始就等于整个 PowerShell,而输出格式仍在我们手里。

### 与 pwshx 的关系

`D:\work\中文 目录\pwshx` 已完成"常驻 pwsh + 命名管道 + JSON 协议"并跑通 15 项测试。
winbash 的 ② 层**复用它的 daemon 脚本与协议**,不重写。pwshx 作为独立工具保留。

## 5. 命令分层

### 5.1 mvdan/sh 已免费提供的(不写)

`mvdan.cc/sh/v3 v3.14.1` 的 `interp.IsBuiltin` 已实现:

- 目录/变量:`cd` `pwd` `export` `set` `unset` `local` `declare` `readonly` `shift`
- 控制流:`eval` `exec` `exit` `return` `trap` `break` `continue` `.` `source` `command`
- 输入输出:`echo` `printf` `read` `test` `[`
- 其他:`kill` `type` `alias` `umask` `wait` `pushd` `popd` `history` `help`

另有 `HandlerContext.Builtin(ctx, args)`,允许 handler 包装或组合内建调用。

### 5.2 ① 层:纯 Go 内建

**入选标准(三条同时满足)**:AI 高频写 · 实现简单 · 在 Windows 上没有干净现成选择。

| 类别 | 命令 |
|---|---|
| 文件与目录 | `ls` `cat` `cp` `mv` `rm` `mkdir` `rmdir` `touch` `ln` `stat` `find` |
| 文本 | `grep` `head` `tail` `wc` `sort` `uniq` `cut` `tr` `tee` |
| 系统 | `date` `env` `which` `seq` |

**第一批只做 `ls` `cat` `grep`** —— AI 最高频的三个。

**参数支持范围以"AI 最常用的那组"为界**:`ls` = `-l -a -h`;`grep` = `-i -n -v -c -l -r -E`;
`cat` = 文件名列表。**不认识的 flag 必须报错退出,不得静默忽略** —— 静默忽略会让 AI 以为
命令生效了,那正是"翻车"的定义。

### 5.3 ② 层:Windows 管理

第一批做 `reg` `svc` `ps` `sys`(Git Bash 最缺的四个)。

```
reg get <key> [name]                    # 有 name 取单值;无 name 列出键下全部值
reg set <key> <name> <data> [--type T]  # T ∈ REG_SZ|REG_DWORD|REG_EXPAND_SZ|REG_MULTI_SZ
reg del <key> [name]                    # 无 name 删整个键
reg keys <key>                          # 枚举子键

svc list [--running|--stopped]
svc status <name>
svc start|stop|restart <name>

ps [<name-or-pid>]                      # 无参数列出全部;有参数即过滤

sys                                     # 概览
sys os|cpu|mem|disk                     # 单项

event <logname> [--max N] [--level L]
task [<name>] | task run|enable|disable <name>
cim <class> [--filter <WQL>]            # 通用 CIM/WMI 逃生口
```

`ps` 的名额是特意选的:bash 的 `ps` 本来就是"列进程",语义完全吻合,**不引入新词**。
`kill` 已由 mvdan/sh 内建提供,故不重复。

**② 层的 TSV 输出约定**:列顺序**固定**,并提供 `--header` 在首行输出列名(便于 AI 自描述)。
例如 `svc list` 固定为 `Name` / `Status` / `StartType`,`reg get` 固定为 `Name` / `Type` / `Data`。
不固定列序 TSV 等于没有结构。

### 5.4 ③ 层:转发,不重写

`sed` `awk` `xargs` 是硬骨头,重写不划算。搜索路径中**加入 Git Bash 的 `/usr/bin`**
(若存在),让它们照常工作。本机必定存在 Git Bash(AI 要跑 git)。

**原则:内建 = 高频 + 简单;转发 = 复杂 + 低频。**

**机制**:daemon 在内部构造搜索路径(真实 `PATH` + Git Bash 的 `usr\bin`),
**不修改用户的环境变量** —— 避免污染从 winbash 里启动的其他进程。

### 5.5 ④ 层:兜底必须可操作

见第 7 节错误模型。

## 6. 输出模型

### 6.1 三种模式

| 命令来源 | 默认输出 | 理由 |
|---|---|---|
| ① 层(模仿 bash) | **严格模仿 bash 的文本语义** | AI 的肌肉记忆与其写的管道(`ls \| grep x`、`awk '{print $5}'`)直接可用 |
| ② 层(我们新造) | **TSV**:每行一条记录,字段 Tab 分隔 | 没有 bash 传统要照顾,TSV 可 `grep`、可 `cut -f2`、可读 |
| 任意命令 + `--json` | 类型化 JSON 数组 | 需要真类型时 |

`--json` 可作为全局开关:`winbash --json -c '…'`。

### 6.2 值保证(本设计的底线)

无论哪种模式,**值本身**必须无歧义。此条优先于"要不要输出 JSON":

| 现状 | winbash 承诺 |
|---|---|
| `0xa` | `10` |
| `STATE: 1` | `Stopped` |
| `86,488 K` | `86488064` |
| ANSI 色码 | 无 |

用户要的是"没有歧义",不是特定序列化格式。`--json` 是锦上添花,值干净是底线。

## 7. 错误模型

1. **退出码必须可靠** —— 修掉 PowerShell "non-terminating error 仍返回 0" 的行为。
   约定(沿用 shell 惯例):`0` 成功 · `1` 一般错误 · `2` 用法错误 · `126` 不可执行 ·
   `127` 命令未找到。
2. **错误信息固定三行**:哪个命令 · 什么错了 · **下一步怎么办**。
3. **主动翻译本机已知坑**,而非让 AI 猜:

```
winbash: bash: 这台机器上 'bash' 指向已损坏的 WSL relay（execvpe(/bin/bash) failed）
         你要的多半是 Git Bash，已用它执行；固定路径：C:\Program Files\Git\bin\bash.exe
```

4. **不静默失败**。找不到命令、路径不存在、权限不足,都必须带建议退出。

## 8. 路径处理

AI 会写 `/c/Users/Test User` 也可能写 `C:\Users\Test User`。文件层通过
`OpenHandler`/`ReadDirHandler` 拦截解析,**两种写法都接受**。

副作用(正向):MSYS 的自动参数改写**永远不会发生**,因为执行路径根本不经过 MSYS。

## 9. 常驻机制

```
winbash -c '…'      ← client 进程，每次新建，~15ms
      │  named pipe（复用 pwshx 协议：4 字节小端长度 + UTF-8 JSON）
      ▼
winbash daemon       ← 常驻，进程内持有 mvdan/sh interp 实例
      ├─ interp 状态：变量、cwd
      ├─ ① 层内建 → 纯 Go，进程内
      ├─ ③ 层 → spawn 真实工具（git/go/…）
      └─ pwsh 子进程 ← 常驻，② 层的执行引擎
```

- 端到端约 **17 ms**(15 ms 进程创建 + 1–2 ms 往返),相对 `pwsh` 的 351 ms 是 ~20×
- 空闲超时后自行退出,不留僵尸进程(复用 pwshx 的 `--idle` 机制)
- 提供 `--stop` / `--status`(pwshx 已验证过这两个的实现;注意 `--stop` 必须真正退出
  外层 accept 循环,这是 pwshx 上已修过的 bug)

**已知边界**:client 进程的 15 ms 是 Windows 的物理下限,消不掉——除非 agent 的 shell 工具
本身支持持久会话。那不属于本工具范围。

## 10. 测试策略

本项目的核心承诺是"不翻车",因此它**必须可被测试抓住**,否则只是口号。四类回归:

| 类别 | 断言什么 | 例 |
|---|---|---|
| **AI 常用命令集** | 一批 AI 典型会写的命令全部跑通且输出正确 | `ls -la \| grep go`、`cat x \| head -3`、`reg get …` |
| **值正确性** | 不出现有歧义的值 | 断言拿到 `10` 而非 `0xa`(第二类是灵魂) |
| **管道一致性** | ① 层命令在管道里的行为与 bash 一致 | `ls \| wc -l` 的数对得上 |
| **错误路径** | 退出码非零 + 错误三行式 | 不存在的命令、路径、权限不足 |

测试从 Git Bash 调用执行(沿用 pwshx 的 `smoke-gitbash.sh` 模式)。

## 11. 工程结构

- Go,单二进制,**唯一第三方依赖 `mvdan.cc/sh/v3`**
- PowerShell 侧脚本用 `go:embed` 编进 exe(沿用 pwshx 做法,exe 可单独拷走)
- 目录划分:`client` / `daemon` / `builtins`(① 层)/ `winmgmt`(② 层)/ `pwsh`(子进程管理)
- **依赖镜像必须配 `goproxy.cn`**:`proxy.golang.org` 在本机不通(实测超时)

## 12. 被否决的方案(记录理由)

| 方案 | 否决理由 |
|---|---|
| **全部翻译成 PowerShell** | **管道语义过不去**:`ls \| grep go` 在 bash 是字节流、在 pwsh 是对象流,同一个 `\|` 两种含义。变量、重定向、`$?` 同理。结果会产生"这条能跑那条不能"——恰是 AI 最讨厌的不可预测 |
| **薄壳(只做环境自描述 + 错误增强)** | 解决不了"不等待"(仍需 spawn);"不翻车"只解决一半,AI 仍得自己把命令名写对 |

## 13. 风险与已知限制

1. **① 层覆盖面增长慢** —— 靠 ③ 层兜底缓解,但 `sed`/`awk` 依赖 Git Bash 存在。
   若 Git Bash 缺失,这些命令会落到 ④ 层报错(明确提示,不静默)。
2. **② 层比纯 Win32 调用慢** —— 转 pwsh 的往返约 1–2 ms。可接受;若将来某个命令成为
   热点,可单独下移到纯 Go 实现,接口不变。
3. **`mvdan/sh` 的行为与真 bash 存在已知差异** —— 主判据是"AI 熟悉的子集",不承诺
   全兼容。差异若影响 AI 常用写法,以测试用例固化并逐个修。
4. **本机特有性** —— 路径、编码、进程启动成本都强依赖平台,只在 Windows 11 上验证过。
