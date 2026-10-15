# winbash

bash 语法 + pwsh 能力的常驻 shell,给在 Windows 上干活的 AI 用。

> 解决的问题:AI 熟悉 bash 但 bash 管不了 Windows;pwsh 管得了但不熟、而且每次冷启约 480 ms。
> winbash 让 `ls | grep go` 这种命令照常跑,需要 Windows 能力时(服务、注册表、进程、系统信息)
> 不用换语法;上百条命令的总耗时从分钟级降到秒级。**连 `pwsh` 本身也被接管**,
> 所以 AI 顺手写的 `pwsh -Command '...'` 同样不再每次付 ~350 ms 的启动成本。

## 快速开始

它**目前不在 PATH 里**(还没做安装步骤,不是刻意设计成这样的),所以要用全路径调用 —— 路径含中文,记得引号:

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
| `--endpoint` | 端口文件路径,默认 `%TEMP%\winbash-<用户名>.port`(按用户名区分,同机多用户不互踩)。文件里是**两行**:地址 + 随机 token |
| `--idle` | 空闲多久后 daemon 退出,默认 `30m`(`0` = 永不) |
| `--timeout` | 单次请求超时,默认 `300s`(`0` = 不设限)。这个上限**同时**告诉 daemon:客户端放弃后它也要停手,否则一条无限循环会把后续命令全部饿死。**嵌套里也生效**(内层走的是本进程执行,同样受这个上限约束) |

## 四层路由

每条命令由 `ExecHandler` 判层,顺序如下。

### ① 层:纯 Go 内建(bash 文本语义)

| 命令 | 参数 |
|---|---|
| `ls` | `[-l -a -h -1] [路径...]`;默认按 POSIX **不列点文件**(要列用 `-a`) |
| `cat` | `[-n] [文件...]`;无参数 = 读 stdin,`-` 也是 stdin |
| `grep` | `[-i -n -v -c -l -r -E -F -q] PATTERN [文件...]`;无文件 = 读 stdin |
| `which` | `名字 [名字...]`;认识下面**四层**,①/② 层报名字,③ 层报绝对路径 |

`grep` 的方言:默认**字面匹配**(`grep '\d' x` 找的是 `\d` 这两个字符);`-E` 走 POSIX ERE。
`-E` 里的 Perl 简写(`\d \w \s` 等)、`(?i)` 内联开关、`*?` 非贪婪**明确拒收**,并给出 POSIX
替代写法(`[0-9]` / `[[:digit:]]` / `[[:alpha:]]` / `[[:space:]]`)—— 真 bash 在这里是**静默**按字面
处理的(`\d` 退化成 `d`),我们选择报错。

`-E` 的字符类按 **ERE 原意**处理:`[\d]` 是「反斜杠或字母 d」,不是 Perl 的数字类。这条值得写出来,
因为 Go 的 `regexp.CompilePOSIX` **不是**完整的 ERE —— 它不认字符类里的 `\`,直接报语法错。而 `[\d]`
是**合法**的 ERE,按"Perl 简写"拒掉会让脚本凭空失败(真 bash 给的是 exit 1「无匹配」)。
内部做法是把字符类内的 `\X` 归一化成 `\\X` 再交给 POSIX 引擎 —— 精确改写,而不是退回 Perl 引擎
(那样 `[\w]` 会变成词字符、语义静默改变,只在内容里正好有词字符时才显形)。

归一化要守住两个边界,**两个都踩过一次**:`\\` 是**已经转义**的反斜杠,不能再转一次(否则
`[\\]` 会变成未闭合的 `[\\\]`、`[\\d]` 会直接报语法错);`[` 或 `[^` 后**紧跟**的 `]` 是字面量,
不结束字符类(否则 `[\]]` 被截断成空集合,**静默**返回"无匹配")。这类改动的坏表现都是哑的 ——
一种像"你的模式写错了",一种像"真的没有东西匹配" —— 所以现在有 11 个 pattern 的 golden 表钉着,
期望值由真 Git Bash 生成,不手写。

`[:name:]` 这类**括号子结构**也算在内:`[[:alpha:]\d]` 是合法 ERE,类内的 `\` 按字面处理。
字符类的边界规则只有**一份**实现(`classEnd`),"这个位置在类内还是类外"这件事只判定一次 ——
早先 `normalizeEreClasses` 与 `firstPerlClass` 各写一套,于是 `[:alpha:]` 的收尾 `]` 被当成类的
结束,`\d` 落到"类外"而把一整段合法 ERE 响亮拒掉了。

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

## 安全边界

daemon 监听 `127.0.0.1`,端口写在一个**端口文件**里 —— 两行:地址 + 一个 128 位随机 token,
每个请求都要带对 token。端口文件的 ACL 在启动时会被收成**只有当前用户**
(`icacls /inheritance:r /grant:r <用户>:(R,W)`)。

它挡的是:**同机其他用户**,以及**沙箱进程**借 daemon 的身份执行命令。
修之前端口文件只有一行明文地址、ACL 里还继承着 `CodexSandboxUsers:(I)(M)`,
于是一个低权限沙箱进程读一下文件就能裸 socket 发一帧,以 daemon 属主身份跑任意命令
(实测 `whoami` 返回属主名),也能发 `{"op":"stop"}` 把别人的 daemon 打停。

两件它**不挡**的事,说清楚免得误解:

- **同一个用户的进程**。那种进程本来就能读你的文件、注入你的进程 —— 在 Windows 上无从
  防起,也不是本项目的威胁模型。**token 不是万能钥匙。**
- **端口文件可被改写**。能写它的进程可以把它指向自己的监听器;ACL 收紧后那也仅限于当前用户,
  与上一条同源。

若 `icacls` 收紧失败(权限或本地化问题),daemon 会在 stderr 打一行警告并**照常启动** ——
token 仍在、daemon 仍可用,只是这层加固没生效,而使用者有权知道。

## 嵌套调用

在 winbash 里再调 `winbash` 是**支持**的,行为与真 bash 的嵌套 `bash` 一致:内层是独立进程,
两边的 `cd` 与变量互不影响。

为什么值得专门说一句:daemon 是**一条连接串行处理**的,内层若去连**同一个** daemon,
它和外层就会互相等到 `--timeout`(默认 300 s)才散场 —— 表现是"卡住五分钟"。所以 ③ 层
派生给子进程的环境里带一个 `WINBASH_SESSION` 标记,client 见到它就改走**本地一次性会话**,
不连 daemon。代价是这一次丢掉了常驻的速度(新建 ① 层状态;用到 ② 层时引擎按需再起,约 350 ms)
—— 嵌套不是热路径,这里**正确**优先于快。

附带两条:

- 嵌套里的 `--status` / `--stop` 会**报错**:它们要看/停的正是"自己所在的那个 daemon"。请从外面执行。
- **非嵌套调用完全不受影响**,照旧走常驻 daemon。

## 值保证与输出

- **值保证**:`0xa` → `10`、`STATE: 1` → `Stopped`、`86,488 K` → `86488064`;任何输出**无 ANSI 色码**。
  这些转换实际由 **PowerShell 侧**的 `.ToString()` 完成(数据在源头就是干净的),不是 Go 侧再洗一遍。
  唯一的 Go 侧兜底是 **ANSI 色码**:`sanitizeCell` / `cellText` 每个单元格都过一遍 `StripANSI`
  —— 目前 pwsh 子进程用 `CREATE_NO_WINDOW`、检测不到终端所以不会上色,但那是环境使然,不是保证。
- **退出码**:`0` 成功 · `1` 一般错误 · `2` 用法错误 · `126` 不可执行 · `127` 命令未找到。
- **不认识的 flag 一律报错退出**,不会静默忽略。

**性能怎么量**:端到端计时在本机**测不出信号** —— 启动一个进程约 700 ms(`cmd.exe` 空转基线),
它把 winbash 真正的开销整个淹掉。上面"首次 70 ms / 之后 22 ms"属于**早期观察**,既难证实也难证伪。
能复现的是进程内 benchmark:

```powershell
go test ./internal/shell/ -run '^$' -bench BenchmarkSessionRun -benchtime=200x
```

它量的是 interp 的一次 `Run`(本机:纯 `echo` 约 **6.4 µs/op**,`echo hello | grep hell` 约 **119 µs/op**)。
端到端那 20 ms 量级里,绝大部分是**进程创建与 IPC 往返**,不是 winbash 本身。

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
| `IFS` 给**多字符** | mvdan/sh 把整个串当分隔符,真 bash 只取**第一个字符**(`IFS=BC` 等价 `IFS=B`)。**静默**差异,光看输出看不出对错 —— 要按多字符分,请用 `IFS=,` 这类单字符,或先 `tr` 转换 |
| `printf '%.0s'` 这类宽度为 0 的格式 | mvdan/sh 的内建 printf 不认(打 `invalid format char`),而且**退出码仍是 0** —— 唯一"有 stderr 但退出码不非零"的形态,靠退出码判断成败的调用方要当心 |
| `more` | Git Bash 里没有,我们用了 Windows 的——多一个工具,不是缺陷 |
| `grep -E '\d'`、`(?i)`、`*?` | 真 bash **静默**按字面处理(`\d` 退化成 `d`);我们**明确拒收**并给 POSIX 替代 |
| `grep -E '[\d]'` | **支持**,按 ERE 原意匹配「反斜杠或字母 d」。Go 的 `regexp.CompilePOSIX` 不认字符类里的 `\`,内部归一化成 `\\` 再编译,而不是退化成 Perl 语义。两个边界同样按 ERE 处理:**已转义**的 `\\` 不会再转一次(`[\\d]`、`[\\]` 仍合法),`[` / `[^` 后**紧跟**的 `]` 是字面量(`[\]]` 不被截断)。这 8 个形状有 golden 表钉住,期望值由真 Git Bash 生成(`go test ./internal/builtins/ -update-grep-golden`),不手写 |
| `ls` 默认不列点文件 | **与 bash 一致**(POSIX 语义)。要列用 `-a`;`-a` 时 `.` / `..` **参与同一个排序**(与 bash 一致),不是恒在最前 |
| `ls -l` 只有四列 | 权限 大小 修改时间 名字;没有属主/组/链接数(Windows 上没有 POSIX 属主),时间格式也不同 |
| `grep -r` 的输出路径 | 基于**你写的那个路径**算(`grep -r b .` → `./d.txt:b`),与 bash 一致 |
| `cat -n` 的行号 | `%6d<TAB>`,与 bash 相同;**多文件时跨文件累加**(`cat -n f1 f2` 里 f2 接 f1 的编号,与 GNU cat 一致);`-A`/`-b`/`-e` 等控制字符可视化开关**报错"未实现"**而不是装作支持 |
| `pwsh` 只有三种形式被接管 | `-c` / `-Command` / 脚本文件;其余形式报错并指路到原生 pwsh(不静默回落) |
| `pwsh -c "$env:X"` | 双引号里的 `$env` 会被 bash 先展开成空 —— **与真 bash 一致**,要写单引号 |
| `pwsh` 的列名按字母序 | 真实 PowerShell 会按对象属性定义顺序排版;我们排序是为了让两次运行的列序一致 |
| 超时**不会**重放脚本 | 客户端超时后直接报错。早先它会重拉 daemon 把同一个请求原样重跑,于是 `echo RUN >> f; sleep 6` 配 `--timeout 2s` 会让文件出现**两行** —— 对写文件类脚本是数据损坏 |
| 坏连接最多挡住 5 秒 | daemon 串行处理连接,一个只连不说话的连接会让后续命令排队;它会在 5 秒后丢掉这条连接。修之前没有这个上限,能一直挡到客户端自己超时 |

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
