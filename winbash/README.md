# winbash

bash 语法 + pwsh 能力的常驻 shell,给在 Windows 上干活的 AI 用。

> 解决的问题:AI 熟悉 bash 但 bash 管不了 Windows;pwsh 管得了但不熟、而且每次冷启约 480 ms。
> winbash 让 `ls | grep go` 这种命令照常跑,需要 Windows 能力时(服务、注册表、进程、系统信息)
> 不用换语法;上百条命令的总耗时从分钟级降到秒级。**连 `pwsh` 本身也被接管**,
> 所以 AI 顺手写的 `pwsh -Command '...'` 同样不再每次付 ~350 ms 的启动成本。

## 快速开始

它**不在 PATH 里**,要用全路径调用(路径含中文与空格,记得引号):

```powershell
& 'D:\work\中文 目录\winbash\winbash.exe' -c 'ls | grep go'
& 'D:\work\中文 目录\winbash\winbash.exe' -c 'svc list --running'
& 'D:\work\中文 目录\winbash\winbash.exe' --json -c 'sys mem'
```

- 第一次调用会自动拉起常驻 daemon(约 **70 ms** 一次性成本),之后每条命令约 **22 ms**。
- daemon 空闲 30 分钟后自行退出,不留僵尸进程;也可以显式管理:

```powershell
winbash.exe --status    # pid / cwd / 地址 / 引擎状态
winbash.exe --stop      # 停掉 daemon(不会为了停掉它而先把它拉起来)
```

## 命令行

| 参数 | 说明 |
|---|---|
| `-c` / `--script` | 要执行的脚本(也可以直接写位置参数:`winbash ls -l`) |
| `--json` / `-j` | 输出类型化 JSON 数组。**是整次调用的开关,写在 `-c` 之前** |
| `--serve` | 前台运行 daemon |
| `--stop` / `--status` | 停掉 / 查看 daemon(都不会把它拉起来) |
| `--endpoint` | 端口文件路径,默认 `%TEMP%\winbash-<用户名>.port`(按用户名区分,同机多用户不互踩) |
| `--idle` | 空闲多久后 daemon 退出,默认 `30m`(`0` = 永不) |
| `--timeout` | 单次请求超时,默认 `300s`(`0` = 不设限) |

## 四层路由

每条命令由 `ExecHandler` 判层,顺序如下。

### ① 层:纯 Go 内建(bash 文本语义)

| 命令 | 参数 |
|---|---|
| `ls` | `[-l -a -h] [路径...]` |
| `cat` | `[文件...]`;无参数 = 读 stdin,`-` 也是 stdin |
| `grep` | `[-i -n -v -c -l -r -E -F -q] PATTERN [文件...]`;无文件 = 读 stdin |
| `which` | `名字 [名字...]`;认识下面**四层**,①/② 层报名字,③ 层报绝对路径 |

`grep` 的方言:默认**字面匹配**(`grep '\d' x` 找的是 `\d` 这两个字符);`-E` 走 POSIX ERE。
`-E` 里的 Perl 简写(`\d \w \s` 等)**明确拒收**并给出 POSIX 替代写法(`[0-9]` / `[[:digit:]]` / `[[:alpha:]]` / `[[:space:]]`)
—— 真 bash 在这里是**静默**按字面处理的(`\d` 退化成 `d`),我们选择报错。

### ② 层:Go 内建(底下转常驻 pwsh 子进程)

| 命令 | 参数 |
|---|---|
| `svc` | `list [--running\|--stopped] [--header]` · `status <名称>` · `start\|stop\|restart <名称>` |
| `reg` | `get <键> [值名] [--header]` · `set <键> <值名> <数据> [--type T]` · `del <键> [值名]` · `keys <键>` |
| `ps` | `[名称或PID] [--header]` |
| `sys` | `[os\|cpu\|mem\|disk] [--header]` |
| `pwsh` | `-c '<脚本>'` · `-Command '<脚本>'` · `<脚本文件.ps1>`(可带 `-NoProfile` / `-NoLogo` / `-NonInteractive`) |

默认输出 TSV 且**列序固定**;`--header` 给表头。要类型化 JSON 用全局的 `--json`。

**`pwsh` 接管了 pwsh 本身**:AI 顺手写的 `pwsh -Command '...'` 走常驻引擎,不再每次付
~350 ms 启动成本。它与其它 ② 层命令有一处不同 —— 列**来自数据本身**(PowerShell 对象的属性),
所以列名按字母序固定,而不是事先声明:单个标量给裸值(不套列名)、标量数组一行一个、对象成表。

- 被接管的形式:**只有** `-c` / `-Command` / 脚本文件。其它形式(`-File` 之外的 `-EncodedCommand`、
  `-Version`、交互式)一律**报错并指路**,不悄悄换成原生 pwsh —— 两者差 20 倍启动成本,
  静默切换会让人以为"反正都一样"(不认识的 flag 必须报错,这条在 ② 层也同样成立)。
- 兜底是**原生 pwsh 的全路径**(路径:`where.exe pwsh`);要交互式 shell 就直接用它。
- bash 的 `export` 对它是可见的(每次调用同步环境)。所以写 `pwsh -c '$env:X'` 要**用单引号**:
  双引号会被 bash 先展开 `$env` —— 这一点与真 bash 完全一致。

### ③ 层:真实工具

搜索顺序 = **Git Bash 的 `usr\bin` 在最前**,后面跟真实 PATH。

- 所以 `sed`、`awk`、`xargs`、`sort`、`find`、`head`、`tail`、`wc`、`tr`、`cut` 都是 **Unix 语义**
  (Git Bash 版),不会撞上 `C:\Windows\System32` 里同名的 Windows 程序
  —— `find . -name '*.go'` 在 System32 那个 `find.exe`(在文件里找字符串)上会直接报参数错误。
- `git`、`go`、`node` 这些走系统 PATH,与你在终端里跑的一致。
- 子进程拿到的是**同一条** PATH,所以 `xargs`、`find -exec`、`make` 这类"子进程再 exec"也能找到工具集。

### ④ 层:兜底

找不到的命令给**三行式错误**(问题 + 怎么办)并返回 `127`;已识别的坑(bash 指向损坏的 WSL relay、
MSYS 的 `/v` 参数改写、把 POSIX 路径喂给 Windows 工具)会在原错误之后补一条翻译,原错误不改写。

## 值保证与输出

- **值保证**:`0xa` → `10`、`STATE: 1` → `Stopped`、`86,488 K` → `86488064`;任何输出**无 ANSI 色码**。
- **退出码**:`0` 成功 · `1` 一般错误 · `2` 用法错误 · `126` 不可执行 · `127` 命令未找到。
- **不认识的 flag 一律报错退出**,不会静默忽略。

## 已知差异与限制

与真 bash 相比的差异(全部有测试钉住,见 `internal/shell/corpus_test.go`):

| 现象 | 说明 |
|---|---|
| `pwd` 给 `D:\...` | 刻意:这一层的返回值要能直接喂给 Windows 工具(bash 给 `/d/...`) |
| glob 给 `.\x` | 同理,Windows 原生形式;①/②/③ 层两种写法都吃 |
| `${v/#前缀/替换}`、`${v/%后缀/替换}` | **不支持,报错 `2`**。mvdan/sh 没实现,而且会静默原样输出——我们宁可报错 |
| `printf %q`、`printf -v` | 同上:不支持但显式报错,并给替代写法 |
| `command -v find` / `type find` | 这两个是 mvdan/sh 的内建,按进程 PATH 回答(给 System32 那个)。准确答案用 `which find` |
| `set -u` / `${v:?}` | 退出码 `1`(bash 的 `-c` 给 `127`),错误文本不带 `bash: line N:` 前缀——都非零、都点名变量 |
| `${#关联数组[@]}` | mvdan/sh 的缺口:恒返回 `1`(键列表 `${!m[@]}` 与取值都是对的) |
| `more` | Git Bash 里没有,我们用了 Windows 的——多一个工具,不是缺陷 |
| `grep -E '\d'` | 真 bash **静默**按字面处理(`\d` 退化成 `d`);我们**明确拒收**并给 POSIX 替代 |
| `pwsh` 只有三种形式被接管 | `-c` / `-Command` / 脚本文件;其余形式报错并指路到原生 pwsh(不静默回落) |
| `pwsh -c "$env:X"` | 双引号里的 `$env` 会被 bash 先展开成空 —— **与真 bash 一致**,要写单引号 |
| `pwsh` 的列名按字母序 | 真实 PowerShell 会按对象属性定义顺序排版;我们排序是为了让两次运行的列序一致 |

**会话级状态**(常驻会话的必然结果,不是 bug):`set -e`、`trap`、`alias`、变量、`cd` 都**跨命令持久**。
`bash -c` 每条都是新进程,所以这一点与它不同——一条命令里的 `set -e` 会影响**后续所有命令**。

**当前限制**:

- **不转发 stdin**:`echo x | winbash -c 'cat'` 拿到的是空输入(不是 `x`)。管道进 winbash 的数据会被丢弃。
- **② 层没有命令级 `--json`**:`sys mem --json` 会报"不认识的参数"。JSON 开关在整次调用上:`winbash --json -c 'sys mem'`。
- 不做交互式 REPL,不追求 100% bash 兼容,不做 pwsh 对象管道。

## 开发

```powershell
cd winbash
go build -o winbash.exe ./cmd/winbash
go test ./... -count=1                      # 全量(含与真 Git Bash 的逐条语料对照)

# 端到端冒烟:要从真 Git Bash 里跑,它拿 Git Bash 当对照
& 'C:\Program Files\Git\bin\bash.exe' ./smoke-gitbash.sh

# 延迟报告(默认跳过:数字依赖机器负载)
$env:WINBASH_BENCH=1; go test ./cmd/winbash/ -run Latency -v
```

约束:**唯一直接依赖**是 `mvdan.cc/sh/v3`(实测带入 `golang.org/x/sys`、`golang.org/x/term` 两个间接依赖)。
依赖镜像要设 `GOPROXY=https://goproxy.cn,direct`(`proxy.golang.org` 在本机不通)。
设计文档与实现计划在 `docs/superpowers/` 下。
