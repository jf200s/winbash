# winbash

bash 语法 + pwsh 能力的常驻 shell,给在 Windows 上干活的 AI 用。

> 解决的问题:AI 熟悉 bash 但 bash 管不了 Windows;pwsh 管得了但不熟、而且每次冷启约 480 ms。
> winbash 让 `ls | grep go` 这种命令照常跑,需要 Windows 能力时(服务、注册表、进程、系统信息)
> 不用换语法;上百条命令的总耗时从分钟级降到秒级。**连 `pwsh` 本身也被接管**,
> 所以 AI 顺手写的 `pwsh -Command '...'` 同样不再每次付 ~350 ms 的启动成本。

## 快速开始

**从源码构建** —— 新 clone 没有 `winbash.exe`(构建产物不进库),需要 Go 工具链:

```powershell
cd winbash
$env:GOPROXY = 'https://goproxy.cn,direct'   # 必须(海外网络可省):proxy.golang.org 在国内不通
go build -o winbash.exe ./cmd/winbash
```

构建产物就在 `winbash\` 下,直接跑:

```powershell
.\winbash.exe -c 'ls | grep go'          # bash 语义,但 ls/grep 是我们自己的实现
.\winbash.exe -c 'svc list --running'    # Windows 服务,不用切到 pwsh 语法
.\winbash.exe --json -c 'sys mem'        # 类型化 JSON 输出
```

- 第一次调用会自动拉起常驻 daemon,之后每条命令:**冷启动 63 ms / 已热 15 ms**
  (2026-10-17 实测,见下一行的口径)。
  原先这里写的是「约 70 ms / 约 22 ms」并标注**早期观察、本机无法证实** ——
  用 PowerShell 的 `Stopwatch` **直调进程**复测后,数字与那个量级**对得上了**,
  所以去掉「无法证实」的措辞;详见「性能怎么量」里的口径说明。
- daemon 空闲 30 分钟后自行退出,不留僵尸进程;也可以显式管理:

```powershell
.\winbash.exe --status    # pid / cwd / 地址 / 引擎状态 + sessions=N running=M 与各具名会话的明细
.\winbash.exe --stop      # 停掉 daemon(不会为了停掉它而先把它拉起来)
```

想把 `winbash` 当命令名用,把 `winbash\` 目录加进用户 `PATH` 即可 —— 没有安装器、没有服务、
没有注册表项,卸载就是从 `PATH` 里删掉那一条。(**本机附注**:作者机器 2026-10-17 已这么配
—— 把 `winbash\` 追加进了 `HKCU\Environment` 的用户 `Path`,所以那台机器上直接写命令名就能跑;
这是本机配置,不是开箱状态。)

**③ 层有个硬前提**:真实工具(`sed`/`awk`/`find`/`head`/`tail`/`wc`/`sort`…)来自 Git for Windows
的 `usr\bin`(③ 层搜索路径把它排在最前)。没装 Git for Windows 时这些命令会 `127`,
`find`/`sort` 会落到 `C:\Windows\System32` 的同名程序、拿到完全不同的语义
(Windows 的 `find` 是"在文件里找字符串"、`sort` 按本地代码页)。

## 命令行

| 参数 | 说明 |
|---|---|
| `-c` / `--script` | 要执行的脚本(也可以直接写位置参数:`winbash ls -l`)。**-c 后面的位置参数**按真 bash 语义绑成 $0/$1…(`winbash -c 'echo $0 $1' foo bar` 输出 `foo bar`,与 `bash -c` 及脚本文件路径的绑法一致;修之前它们被静默扔掉)。参数按参数传,引号与空格原样保留 |
| `--json` / `-j` | 输出类型化 JSON 数组。**是整次调用的开关,写在 `-c` 之前** |
| `--serve` | 前台运行 daemon |
| `--stop` / `--status` | 停掉 / 查看 daemon(都不会把它拉起来)。两者都有 **5 s 上限**(`client.manageTimeout`),但那是**最后的保险**,不是常态:并发化之后它们**不再排队** —— op 分派在会话队列**之前**(见 `daemon.handleConn`),所以 daemon 正被一条长命令占着时它们仍会**立刻**返回真状态。5 s 只在 daemon 卡在关停这类极端情况下才兑现(端到端测试 `TestManageCommandsDoNotQueueBehindLongCommand` 钉着「忙时立刻返回」)。**`--stop` 的执行语义**:先把监听关掉、端口文件删掉(client 收到 OK 时 daemon 已经"消失"),再给正在跑的命令 **5 s 宽限**(`daemon.stopGrace`)让它收尾,到点仍在跑的会被取消 |
| `--endpoint` | 端口文件路径,默认 `%TEMP%\winbash-<用户名>.port`(按用户名区分,同机多用户不互踩)。文件里是**两行**:地址 + 随机 token |
| `--idle` | 空闲多久后 daemon 退出,默认 `30m`(`0` = 永不) |
| `--timeout` | 单次请求超时,默认 `300s`(`0` = 不设限)。这个上限**同时**告诉 daemon:客户端放弃后它也要停手,否则一条无限循环会把投给**同一会话**的后续命令全部饿死(别的会话不受影响)。**嵌套里也生效**(内层走的是本进程执行,同样受这个上限约束) |
| `--fresh` / `--no-state` | 这条命令跑在**一次性会话**里:前面的变量 / `set -e` / `trap` / 函数影响不到它,它设的也不留给后面的命令。默认**不用**,会话照旧持久(见下文「会话级状态」) |
| `--session <名>` | 这条命令落到哪个**会话**。同名会话内严格 FIFO 串行、状态(变量 / 函数 / `set -e` / `trap`)照旧跨命令持久;不同名会话之间**真并行**。默认值取自环境变量 `WINBASH_SESSION_NAME`(并行子代理就不必每条命令都写这个 flag),空 = **默认会话**,与引入本选项之前完全等价。与 `--fresh` 不能同用(`exit 2`),也不能与 `--serve`/`--stop`/`--status` 同用(`exit 2`)。**嵌套调用里静默不生效**,见下文 |
| bash 的启动选项 | `-noprofile` / `--norc` / `--noediting` / `--posix` / `-l` / `-i` **接受但无作用**。把 winbash 设成默认 shell 后,宿主/工具链会按 bash 的约定拼命令行(实测 `winbash.exe -noprofile -c …`),不接受它们就等于**一条命令都跑不了**。它们要求的都是"别做额外的事"(不读 profile/rc、无行编辑),而 winbash 本来就不读 profile、没有交互 REPL,所以行为上已经满足 —— 不存在被忽略的副作用。要求**额外行为**的选项(如 `-s` 从 stdin 读脚本)不在名单里,照旧按未知 flag 报错 `2` |

## 升级与协议版本

client 与 daemon 之间有协议版本(当前 **5**,见 `internal/ipc/ipc.go` 的 `ProtoVersion`)。
daemon 是**常驻**的(默认 30 分钟才退),且它由 client 用**同一个 exe 路径**拉起 —— 所以
「你替换了 `winbash.exe`、而旧 daemon 还活着」是**正常状态**,不是意外。

这种情况下新 client 连旧 daemon **会明确报错,不静默执行**:

```text
winbash: client: daemon 没有协议版本(它是旧版),与本端(5)不兼容
怎么办: 运行 `winbash --stop` 停掉旧 daemon,下次调用会自动用新 exe 重建
```

为什么宁可报错也不静默降级:协议一旦变化(例如 2026-10-16 把 stdin 从「一次性缓存」
改成「流式转发」),旧 daemon 会**忽略**新 client 发来的 stdin —— 脚本照样 `exit 0`,
而输入其实丢了。那正是本项目「不静默做错事」要消灭的失败形态,不能在新入口复现。
同一理由适用于 **3**:旧 daemon 不认识 `fresh` 字段,会当成没写 —— 于是 `--fresh`
静默退化成「复用会话」,而调用方以为已经隔离了。

同一理由适用于 **4**:旧 daemon 不认识 `session` 字段,会当成没写 —— 于是 `--session build`
静默退化成默认会话,并行的几条命令全挤在同一个队列里排队,而调用方以为已经分开了。

同一理由适用于 **5**:旧 daemon 不认识 `args` 字段,会当成没写 —— 于是 `winbash -c 'echo $0 $1' foo bar`
里的 `foo bar` 被静默扔掉,而真 bash 把它们绑成 $0/$1。

处置只有一步:**`winbash --stop`**。`--stop` 与 `--status` **刻意不参与版本检查** ——
否则「版本不匹配时先 `--stop`」这个推荐处置会被版本检查自己挡掉,陷入「要停它得先连上它」的死锁。

同理,**不认识的 `op` 一律拒绝**(`exit 2`),不落到「执行脚本」路径上。修之前 `switch req.Op`
只列了 `stop`/`status` 两个 case、没有 `default`,于是 `op:"restart"` 这类值会**静默执行请求里的脚本**
—— 调用方以为发的是管理命令,实际跑了任意命令。

另外两个**同类陷阱**(都是本机实测过的,不是推测):
- **`WINBASH_SESSION_NAME` 只影响「执行脚本」那条路**。它给 `--session` 提供默认值,但**不会**
  让 `--serve`/`--stop`/`--status`/`--fresh` 变成用法错误 —— 判据是「用户有没有显式写这个 flag」,
  不是「值非空」。修之前按值判断,结果设了它连 daemon 都起不来,而 `--stop` 恰恰是上面那唯一的处置。
- **`--idle 0`(永不退出)只在 `--serve` 这条显式路径上成立**。自动拉起 daemon 时 `0` 不会被转发
  (它表达的是「不指定」而不是「永不」),所以用 `-c` 调起来的那只会落回默认的 30 分钟。
  真要常驻不退出,就显式跑 `winbash --serve --idle 0`。

另外两个环境变量,作用范围各不相同,别混:

| 变量 | 影响什么 | 默认 |
|---|---|---|
| `WINBASH_SESSION_NAME` | 只给 **`--session`** 提供默认值(不碰管理开关) | 空 = 默认会话 |
| `WINBASH_SESSION` | winbash **自己**派给 ③ 层子进程的标记,用来识别嵌套调用。使用者不该设它 | 不设 |
| `WINBASH_CMD_SHELL` | 认不出的 `.cmd`/`.bat` 是否交 `cmd /c` 代跑(代价:参数经 cmd 重新解析) | **关** |
| `WINBASH_BENCH` | `go test ./cmd/winbash/ -run Latency` 是否跑延迟报告 | 关 |

## 四层路由

每条命令由 `ExecHandler` 判层,顺序如下。

### ① 层:纯 Go 内建(bash 文本语义)

| 命令 | 参数 |
|---|---|
| `ls` | `[-l -a -h -1 -d] [路径...]`;默认按 POSIX **不列点文件**(要列用 `-a`)。`-d` 列**参数本身**而不展开目录:`ls -d */` 只给子目录名、`ls -ld 某目录` 只看那个目录的属性(与 bash 一致,也不打 `total` 行) |
| `cat` | `[-n] [文件...]`;无参数 = 读 stdin,`-` 也是 stdin |
| `grep` | `[-i -n -v -c -l -r -E -F -q -A N -B N -C N -o -w -e P] PATTERN [文件...]`;无文件 = 读 stdin,`-` 也是 stdin |
| `egrep` | `grep -E` 的历史同义词。前置的 `-E` 算**显式**方言:`egrep -F` 按冲突拒绝(`conflicting matchers specified`,GNU 同)。不走 ③ 层是有意的:usr\bin 里的 `egrep`/`fgrep` 是**无扩展名 wrapper 脚本**,Windows 的 CreateProcess 执行不了,所以映射到自己的 grep(同一套方言仲裁) |
| `fgrep` | `grep -F` 的历史同义词。前置的 `-F` 算**显式**方言(修之前这个前置 `-F` 被**静默丢掉**,fixed-string 从未生效,`fgrep 'a.c'` 连 `abc` 都匹配);单独再给一遍 `-F` 不算冲突 |
| `which` | `[-a\|--all] [--layers] 名字 [名字...]`;认识下面**四层**,①/② 层报名字,③ 层报绝对路径。`-a` 列出**所有**匹配位置(按 PATH 顺序)。`--layers` 是**诊断**开关:逐行报出 ①② 层各命中与否、③ 层的每个目录分别是否命中,全都没有时给一行「结论 所有层都没有」并以 `1` 退出 —— 它回答的是「为什么没落到」而不是「落到哪」,所以不打印最终位置。不认识的开关**响亮拒绝**(`exit 2`)而不是当名字搜 |

GNU grep 的两个高频选项 `--include` / `--exclude` **不支持**(① 层只有上表那些开关)。它们常被
写在 PATTERN **之后**(`grep -rn x --include='*.go' .`),而真 grep 靠 getopt 的 **permutation**
把选项挪到前面 —— 我们不做置换,所以这类写法里的 `--include` 会被当成**文件名**。修之前它的
行为是**静默半错**:stderr 报一句"路径不存在",stdout 却把全部文件都搜了,最后 `exit 0`
(过滤没生效而退出码说成功)。现在**响亮拒绝** `exit 2` 并给出替代写法:

```bash
find . -name '*.go' -exec grep -n PATTERN {} +     # 按文件名过滤
```

要 GNU 的完整方言就显式调真 grep(`<Git 安装根>\usr\bin\grep.exe`)。同理 `ls -R` 不支持 ——
错误信息直接指向 `find`(递归列目录在 bash 里的标准解法),不再只说"不认识的参数"。

`grep` 的方言仲裁(2026-10-31 起):**默认是 BRE**(真 grep 的默认方言,不是"整串字面")——
`.` `*` `[ ]` `^` `$` `\` 都是元字符,`\(...\)` 分组、`\{n,m\}` 区间重复,而裸的
`( ) { } + ? |` 是**字面**;`-E` 走 POSIX ERE;**`-F` 按字面匹配**。`-E` 与 `-F` 同给
(含 `egrep -F` / `fgrep -E` 这种经别名前置的)按 GNU **响亮拒绝**:
`conflicting matchers specified`、`exit 2` —— 修之前是静默按其中一个跑,而 `fgrep` 的
前置 `-F` 更是从未生效过(`fgrep 'a.c'` 连 `abc` 都匹配)。

高频选项都有真语义(输出格式逐字对齐真 grep,全部实测钉在 `grep_switches_test.go`):

- `-A N` / `-B N` / `-C N`:命中行之后/之前/两侧各带 N 行上下文。行号(`-n`)在上下文行
  用 `-` 后缀(命中行仍是 `:`);两组之间有没被覆盖的空档时补一条裸 `--`,相邻/重叠合并,
  **文件边界上也补**(游标是调用级的);`-A0`/`-B0`/`-C0` 显式给 0 也启用分组,完全不给
  上下文开关则永不打 `--`。某维度被 `-A`/`-B` 显式给过时 `-C` 对该维度让位(与顺序无关:
  `-A3 -C1` 与 `-C1 -A3` 都是 after=3/before=1)。`-c`/`-l`/`-q` 下上下文全部失效。
  非法的行数(`-Ax`、`-A 2x`、`-A -1`)按 GNU 报 `<值>: invalid context length argument`、
  `exit 2`。
- `-o`:只输出命中段,一段一行,行号/文件名前缀与整行同款;空命中不输出;与 `-v` 同给
  **一个字都不出**(被选中的行本来就不含命中段)但 exit 仍看有没有行被选中;与上下文同给
  时不打印上下文行但组间隔线照打;`-c`/`-l`/`-q` 优先。
- `-w`:全词匹配。词字符是字母、数字、下划线(实测 `abc_def` 不被 `-w abc` 命中);检查的
  是**命中段**两侧的词边界,不是给模式包 `\b`(模式以非词字符开头/结尾时两者不等价);
  对 `-F`、`-E`、`-o`、`-c` 都生效。
- `-e PATTERN`:多模式(OR);模式可以 `-` 开头(`grep -e -x`);给了 `-e` 之后**所有**
  位置参数都是文件名(实测 GNU 同款);多个 `-e` 的 `-o` 输出按**行内位置**排序,不按
  `-e` 给出顺序;`-e` 缺参数 `exit 2`。

BRE 这一条是修过的:**早先默认按整串字面匹配**,于是这几个高频写法全部**静默给错值**
(同一份 `abc\nabbbc\nxyz\n` 输入、`grep -c` 的结果):

| pattern | 旧实现 | 真 grep |
|---|---|---|
| `^abc` | 0 | 1 |
| `abc$` | 0 | 1 |
| `a.c` | 0 | 1 |
| `ab*c` | 0 | 2 |
| `[ab]` | 0 | 2 |

`grep -q '^foo' file` 是最常见的一类判断,静默不匹配会让 `if` 直接走错分支 —— 所以默认
方言现在与真 grep 一致。实现在 `breToEre`:先把 BRE 翻译成 ERE,再走与 `-E` 相同的编译
路径(`internal/builtins/grep.go`)。

`-E` 里的 Perl 简写(`\d \w \s` 等)、`(?i)` 内联开关**明确拒收**,并给出 POSIX
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
| `ps` | `[名称或PID] [--header]`;经典旗标(`aux` / `-ef` / `-e` / `-f` / `-l` / `-u` / `-p`)与其它 `-` 开头参数**响亮拒绝**并给等价写法 |
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
- bash 的 `export` 对它是可见的(每次调用同步环境,走协议字段 `env`)。所以写 `pwsh -c '$env:X'` 要**用单引号**:
  双引号会被 bash 先展开 `$env` —— 这一点与真 bash 完全一致。
  **为什么走协议而不是拼在脚本前面**(2026-10-18 修):任何文本前导都会让「脚本以
  `param(...)` 或 `#Requires` 开头」失效 —— 那两样必须是 scriptblock 的第一条语句,
  否则报 `The term 'param' is not recognized`(实测;原生 `pwsh -File` 正常)。
  `cwd` 与 `env` 因此都收进了协议,由 `session.ps1` 在脚本**之外**处理。
- **`pwsh` 子命令跟随会话的 `cd`**(2026-10-18 修)。引擎是常驻进程,它的 cwd 是**启动那一刻**
  继承的 daemon 目录,而会话的 `cd` 发生在那之后 —— 所以 `pwsh` 子命令每条都把会话 cwd
  随请求带过去(协议字段 `cwd`,不是拼文本)。修前 `cd /tmp; pwsh -c 'New-Item x'` 会把文件
  **静默写进 daemon 的 cwd**,`cd /tmp; pwsh rel.ps1` 会报"读不到脚本文件"。
  相对路径的**脚本文件**也按会话 cwd 解析(修前按进程 cwd,明明在`/tmp`里的文件读不到)。

  **范围要说准**:这条只覆盖 `pwsh` 子命令。`svc` / `reg` / `ps` / `sys` 是 ② 层的**固定脚本**,
  不接收 `cwd` / `env` 协议字段,仍在引擎启动时那个 cwd / 环境下执行 —— 它们本来也不读写
  会话的相对路径,所以行为是对的;但**别指望** `cd /tmp; svc list` 会落到 `/tmp`。
  要跨这两类共享 cwd,先把 `pwsh -c 'Set-Location ...'` 之后的命令写进同一条 `pwsh` 调用。

  这里有一个 Windows 特有的坑,写下来免得下一个人重踩:`Set-Location` 会把路径里的
  `~` 当 home 展开,**连 `-LiteralPath` 都挡不住**,而 8.3 短名恰好含 `~` ——
  daemon 的 cwd 常落在 `C:\Users\TESTUS~1\AppData\Local\Temp` 这种形态上,于是它报
  `An object at the specified path C:\Users\TESTUS~1 does not exist.`,而同一路径
  `Test-Path` 为真。所以切目录前先 `(Get-Item -LiteralPath ...).FullName` 拿长名;
  `Resolve-Path` / `Convert-Path` 对短名原样返回,只有 `Get-Item` 给长名(都实测过)。

### ③ 层:真实工具

搜索顺序 = **Git Bash 的 `usr\bin` 在最前**,后面跟真实 PATH。

- 所以 `sed`、`awk`、`xargs`、`sort`、`find`、`head`、`tail`、`wc`、`tr`、`cut` 都是 **Unix 语义**
  (Git Bash 版),不会撞上 `C:\Windows\System32` 里同名的 Windows 程序
  —— `find . -name '*.go'` 在 System32 那个 `find.exe`(在文件里找字符串)上会直接报参数错误。
- `git`、`go`、`node` 这些走系统 PATH,与你在终端里跑的一致。
- **自带路径的写法**(`./x.sh`、`/c/x/y.sh`、`C:\x\y.exe`)按**会话 cwd** 解析,不是进程 cwd
  —— 否则 `cd /tmp && ./t.sh` 会去 daemon 启动时那个目录里找。判"能不能直接执行"看的是
  **文件头是不是 PE**,不是扩展名:`.sh`/`.py` 这类带 shebang 的脚本因此能像在真 bash 里
  一样跑(`usr\bin` 的 `env.exe` 读 shebang 找解释器)。
- **`.cmd` / `.bat` 垫片会被翻译,而不是一律拒绝**(2026-10-19)。npm / pnpm / tsc 这些工具的
  入口都是批处理,一律拒绝等于把整个 Node 生态挡在门外。所以先认垫片的**形状**:只要它符合
  npm 系模板(收尾是 `"%_prog%" "%_cli%" %*`,变量值能一路展开到真实路径),就把「它要调哪个
  node 脚本」翻译成一次直接 exec —— **使用者传来的参数一个字节都不经过 cmd**,`&` `|` `^`
  `%VAR%` 原样到达(实测 `npm exec -- node -e ... 'c&d' 'e%F%g'` 四个参数逐字一致)。
  认不出来的批处理**保持拒绝**(`exit 126`)并给两条出路;想让 winbash 代劳可以显式
  `export WINBASH_CMD_SHELL=1` 走 `cmd /c` 兜底 —— 但那条路会把参数交给 cmd 重新解析,
  所以它**默认关**,不替人做主。
- 子进程拿到的是**同一条** PATH,所以 `xargs`、`find -exec`、`make` 这类"子进程再 exec"也能找到工具集。
- **参数怎么送进去,这两类目标不是同一套规则。** Windows 上"父→子"的参数是拼成一条命令行字符串
  传的,由**子进程自己**按它认的规则拆回 argv,而这个规则不是 ABI 钉死的:
  - 原生程序(`git`、`go`、`node`)走 MSVC CRT 规则,Go 默认的 `syscall.EscapeArg` 正是这一套,没问题;
  - **MSYS/Cygwin 系程序**(`usr\bin` 里的 `awk`、`sed`、`find`、`grep`、`sort`、`xargs`…)走的是
    bash 式 `build_argv`,里面 `\` **没有任何转义含义**。两套规则不兼容,所以凡是从我们(原生进程)
    启动它们,MSVC 风格的 `\"` 会被解析成"字面 `\` + 引号结束":

        awk 'NR==1{print "quoted-ok"}' f   →  awk: NR==1{print \quoted-ok\}  (backslash not last character)

    修法是不给它们走 MSVC 规则:按其自身规则改用**混合引号 + 相邻段拼接**(含 `'` 的段用 `"` 包,
    其余用 `'` 包),这条路不需要任何转义,任意字节串都能完整送达。判定靠"文件里有没有导入
    `msys-2.0.dll`/`cygwin1.dll`",结果按路径缓存且不进热路径。实现与实测表见
    `internal/shell/msysargv.go`。


### ④ 层:兜底

找不到的命令给**三行式错误**(问题 + 怎么办)并返回 `127`;已识别的坑(bash 指向损坏的 WSL relay、
MSYS 的 `/v` 参数改写、把 POSIX 路径喂给 Windows 工具)会在原错误之后补一条翻译,原错误不改写。

**路径错误用 POSIX 措辞,不泄漏 Win32 细节**(2026-10-18)。`ls` / `cat` / `grep` 遇到路径失败时,
"问题"那行与真 Git Bash 的**对应命令**逐字对齐 —— 注意**各命令格式并不相同**,
这是 GNU coreutils 本身的做法,不是我们随意统一:

```
ls nope.txt      winbash: ls        问题: cannot access 'nope.txt': No such file or directory
                 真 bash           ls: cannot access 'nope.txt': No such file or directory

cat nope.txt     winbash: cat       问题: nope.txt: No such file or directory
                 真 bash           cat: nope.txt: No such file or directory

grep x nope.txt  winbash: grep      问题: nope.txt: No such file or directory
                 真 bash           grep: nope.txt: No such file or directory
```

只差三行式外壳(`winbash: <命令>` / `问题:` / `怎么办:`)。**`cannot access` 是 ls 独有的措辞**;
实测 GNU 内部就不统一 —— `cat` / `grep` / `wc` 给裸的 `<路径>: <原因>`,`head` / `tail` 用
`cannot open 'x' for reading`,`sed` 用 `can't read`,`awk` 用 `fatal: cannot open file`。
所以**不要**把三者统一成一种:统一必然与其中两个对不上。`TestCatPathProblemUsesBareCoreutilsFormat` /
`TestGrepPathProblemUsesBareCoreutilsFormat` / `TestLsPathProblemKeepsCannotAccess` 逐命令钉着这件事
(含 `ls -d` 分支,那是另一处独立调用点)。

**两条有意不照搬 GNU**:`cat 大文件 | head` 在真 bash 里是被 SIGPIPE 直接杀掉(rc=0、stderr 空),
`cat f > /bad/x` 由 **shell 层**拦截、cat 根本没启动 —— 所以那两种情况 GNU 并没有"cat 的措辞"可对齐。
winbash 不会因 SIGPIPE 被杀(它自己管着输出端),因此仍显式报 `cannot write output: …` /
`cannot read standard input: …`。报出来比照抄一句"什么都没有"有用(`TestCatWriteAndStdinProblemsStayExplicit`)。

修之前是 `GetFileAttributesEx C:\Users\...\nope.txt: The system cannot find the file
specified.` —— 三个问题都直接影响「AI 能不能自己修」:**泄漏 Win32 API 名**(`GetFileAttributesEx`
/ `CreateFile`)、**把用户写的相对路径换成绝对路径**、**用 Win32 长句而不是 POSIX 措辞**。
措辞在 `internal/builtins/oserr.go`,七类错误(`No such file or directory` / `Not a directory` /
`Permission denied` / `Is a directory` / 非法字符 / `EISDIR` / 未知 errno 的 `I/O error (Win32 error N)`)。

**重定向与 `source` 走的是另一条路,也已对齐**(2026-10-18)。它们的失败由 mvdan/sh 的
`OpenHandler` 产生,不经过上面那些命令内建的措辞层,原先原样端出 Go 的错误:

```
echo x > /bad/y    修前: open C:\Program Files\Git\bad\y: The system cannot find the path specified.
                   修后: /bad/y: No such file or directory
                   真 bash: line 1: /bad/y: No such file or directory
source nope.sh     修后: source: nope.sh: No such file or directory
echo x > adir      修后: adir: Is a directory
echo x > /c/Windows/System32/drivers/etc/__nope
                   修后: /c/Windows/.../__nope: Permission denied
```

覆盖 `>` / `>>` / `<` / `2>` 与 `source`。**这需要给 mvdan/sh 打一个补丁**(把
`switch err.(type)` 换成 `errors.As`,即上游自己的 TODO),见
`third_party/mvdan-sh/WINBASH-PATCH.md` §3 —— 否则 handler 无法自定义文案而不改变
"重定向失败只让那条命令失败、脚本继续"这个语义。回归在
`internal/shell/openerr_test.go`,其中 `TestRedirectFailureDoesNotAbortScript`
盯的是**控制流**而不是字符串。

两处细节值得知道:
- **`Not a directory` 与 `No such file or directory` 在 Windows 上是同一个 errno(3)**,
  靠"路径里有没有把文件当目录用"的结构检查区分 —— 真 bash 分得很清,而它决定 AI 下一步是改结构还是找文件。
- **`cat 某目录`**:Windows 上 `os.Open` 一个目录会**成功**,失败要到第一次 `Read` 才发生(给
  `Incorrect function.`)。所以 `cat` 在 open 之后显式 stat 判目录,而不是靠错误码猜 —— 猜 `ERROR_INVALID_FUNCTION`
  是错的,那是通用错误码。

**`cd` 与 `>&` / `<&` 也补完了**(2026-10-18)。它们比上面那批更难,因为都在 mvdan/sh 的
**内建**里:`cd` 直接改 runner 的当前目录,连 `OpenHandler` 都不过;`>&` / `<&` 是语法层
的重定向处理。所以只能给上游打补丁(`WINBASH-PATCH.md` §4):

```
cd nosuch          修前: cd: no such file or directory: "nosuch"
                   修后: cd: C:\Program Files\Git\nosuch: No such file or directory
                   真 bash: cd: nosuch: No such file or directory
cd plain.txt       修前: cd: no such file or directory: "plain.txt"     ← 它明明存在
                   修后: cd: plain.txt: Not a directory
cd ""              修前: cd: empty directory path
                   修后: cd: null directory                             (真 bash 同)
cd a b             修前: usage: cd [dir]
                   修后: cd: too many arguments                         (真 bash 同)
unset OLDPWD; cd - 修前: cd: null directory      ← 把「你还没 cd 过」说成「你给了个空目录」
                   修后: cd: OLDPWD not set                             (真 bash 同)
echo x >& 99       修前: unhandled >& arg: "99"
                   修后: 99: Bad file descriptor                        (真 bash 同)
```

三处共性缺陷:路径被 `%q` 多包一对引号、消息首字母小写、**错误原因被丢掉**。
最要紧的是第二行 —— 上游把「stat 失败」与「不是目录」合成一个分支,于是 `cd` 一个
**存在的文件**会说"不存在"。这决定 AI 下一步是去建目录还是查自己的路径,所以必拆。

`cd ""` 与 `cd -`(OLDPWD 未设置)这两条**都是空串**,但真 bash 给的措辞不同
(`null directory` / `OLDPWD not set`)。所以区分只能在 `case "cd"` 里做 ——
那是唯一还知道「用户写的是 `-`」的地方;等空串流进 `changeDir` 就已经分不出来了。

`cd` 报错里仍是 **Windows 绝对路径**(不是用户写的 `nosuch`),根因是 winbash 的
`normalizeCdPath` 在参数进 interp **之前**就改写了,而 interp 没有 API 能把原串带回来。
这与 `pwd` 的既定设计一致(见 [已知差异与限制.md](已知差异与限制.md) 的 `pwd` 那行),所以本轮只对齐措辞。
回归在 `internal/shell/cdwording_test.go`(8 个 Test / 18 子用例;多出的 `TestCloseStdoutReportsWriteError` 是 `>&-` 那轮补的)。

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

为什么值得专门说一句:daemon 现在并发处理连接,但**会话内严格 FIFO 串行** —— 而这条正在跑
的命令正占着它自己那个会话。内层若去连同一个 daemon、又落到同一个会话,就会排在自己后面:
外层等内层退出、内层等外层让出队列,两边等到 `--timeout`(默认 300 s)才散场 —— 表现是
"卡住五分钟"。所以 ③ 层派生给子进程的环境里带一个 `WINBASH_SESSION` 标记,client 见到它就
改走**本地一次性会话**,不连 daemon。代价是这一次丢掉了常驻的速度(新建 ① 层状态;用到 ② 层
时引擎按需再起,约 350 ms)—— 嵌套不是热路径,这里**正确**优先于快。

附带三条:

- 嵌套里的 `--status` / `--stop` 会**报错**:它们要看/停的正是"自己所在的那个 daemon"。请从外面执行。
- **嵌套里 `--session` 静默不生效**(已知限制)。这条路走的是本进程内的一次性会话
  (`client.RunTimeout` 的嵌套分支),`Session` 选项在那条路径上**根本没被应用** —— 写
  `--session build` 不报错,但也不会真的落到 build 会话。要分会话就在**非嵌套**的调用里指定。
- **非嵌套调用完全不受影响**,照旧走常驻 daemon。

## 故障排查速查

只做索引:现象 → 去哪节看。细节都在指到的节里,这里不重写(指路用小节名,行号会漂)。

| 现象 | 去哪看 |
|---|---|
| 替换过 `winbash.exe` 之后报「daemon 没有协议版本(它是旧版),与本端(5)不兼容」 | **「升级与协议版本」**:旧 daemon 还活着是正常状态,处置只有一步 `winbash --stop`;`--stop`/`--status` 刻意不参与版本检查,否则「要停它得先连上它」会死锁 |
| 设了 `WINBASH_SESSION_NAME` 之后 `--stop`/`--status`(或拉 daemon)出问题 | **「升级与协议版本」的「同类陷阱」**:它只给 `--session` 提供默认值,**不碰** `--serve`/`--stop`/`--status`/`--fresh`(按值判断导致连 daemon 都起不来的旧 bug 已修;但这个变量只影响「执行脚本」那条路) |
| 嵌套调用(winbash 里再调 winbash)里写 `--session`,不报错但也没落到那个会话 | **「嵌套调用」**:嵌套走本进程一次性会话,`--session` 在那条路径上**静默不生效** —— 要分会话就在非嵌套的调用里指定;`--status`/`--stop` 在嵌套里则直接报错 |
| 从 winbash 会话里跑 `smoke-gitbash.sh`,以 `2` 退出 | **「开发」**:smoke 刻意拒绝从 winbash 会话启动 —— 那 4 条 daemon 管理断言会齐刷刷假红;请从真 Git Bash 跑(`& 'C:\Program Files\Git\bin\bash.exe' ./smoke-gitbash.sh`) |

## 值保证与输出

- **值保证**:`0xa` → `10`、`STATE: 1` → `Stopped`、`86,488 K` → `86488064`;任何输出**无 ANSI 色码**。
  这些转换实际由 **PowerShell 侧**的 `.ToString()` 完成(数据在源头就是干净的),不是 Go 侧再洗一遍。
  唯一的 Go 侧兜底是 **ANSI 色码**:`sanitizeCell` / `cellText` 每个单元格都过一遍 `StripANSI`
  —— 目前 pwsh 子进程用 `CREATE_NO_WINDOW`、检测不到终端所以不会上色,但那是环境使然,不是保证。
- **退出码**:`0` 成功 · `1` 一般错误 · `2` 用法错误 · `126` 不可执行 · `127` 命令未找到。
- **不认识的 flag 一律报错退出**,不会静默忽略。

**性能怎么量**:**先说口径 —— 口径决定能不能测出信号**。两种,别混:

| 口径 | 结果 | 什么时候用 |
|---|---|---|
| **PowerShell 直调进程**(`& $exe -c '…'` + `Stopwatch`) | 信号干净。2026-10-17 实测:冷启动(拉起 daemon + 执行)**63 ms**、已热**15 ms**、批量 12 条 **595 ms**(50 ms/条) | 报绝对值用这个 |
| **端到端**(Git Bash 里手测) | 本机**测不出信号** —— 启动一个进程约 700 ms(`cmd.exe` 空转基线),把 winbash 的开销整个淹掉 | 只做相对比较,别报绝对值 |

正因为在 PowerShell 直调口径下测出了信号,开头那对数字(冷 **63 ms** / 热 **15 ms**)已
不再标「早期观察」;**Git Bash 口径仍测不出**,所以别在那个口径下引用绝对值。

**量并发时不要用 PowerShell 的 `Start-Job`** —— 每个 job 起一个独立的 PowerShell 进程,
量出来全是 job 机制自己的开销:同一件事实测 `Start-Job` 给 **3205 ms**、Go 里直调给
**1077 ms**,差三倍。要并发就用 Go(测试里的并发用例已经够用),别拿 shell 的作业控制当基准。

能复现到微秒的是进程内 benchmark:

```powershell
go test ./internal/shell/ -run '^$' -bench BenchmarkSessionRun -benchtime=200x
```

它量的是 interp 的一次 `Run`。**2026-10-17 实测:纯 `echo` 约 13.9 µs/op、
`echo hello | grep hell` 约 146 µs/op**(AMD Ryzen 9 8945HX,16 核 32 线程,
`-benchtime=200x`)。

这里原先记的是 6.4 µs / 119 µs —— 但那条**没写测在哪台机器上**,而两次数值差约 2×,
于是分不清是版本退化还是换了机器(一次独立评审也量到 12.9 µs / 135 µs,同量级)。
所以从现在起 benchmark 数字必须**连日期与 CPU 一起记**:换机器先重测再比对,
别拿两台机器的数互相印证。

端到端那 20 ms 量级里,绝大部分是**进程创建与 IPC 往返**,不是 winbash 本身。

**① 层不比真 bash 快**(实测 20 次 `echo x`:winbash 0.783s vs 真 `bash -c` 0.689s,差异在噪声量级内)。
它的价值是**语义可控** —— `ls` 永远是自己的 `ls`,不被 PATH 上重名的 Windows exe 抢走 —— **不是快**。
省时间的地方在 **② 层**(吃掉 pwsh 的 ~350 ms 冷启动):**3.7 倍**是评估报告的口径
(daemon 冷、逐次摊),而 2026-10-17 在「引擎已热、连跑 5 次」下量到 **20.6×**
(2038 ms → 99 ms)。两个都真,口径不同 —— **别混着引用**,对外说法用「重复调用 ② 层比每次
冷启 pwsh 快一个数量级」。再加上「不用换语法」,别指望纯 bash 那层提速。

## 已知差异与限制

与真 bash 相比的差异整表、连同测试覆盖档案(哪些差异有测试钉住、哪些只是弱覆盖),
已原样搬到 **[已知差异与限制.md](已知差异与限制.md)**;下面保留会话状态与当前用法限制。

## 会话级状态与当前限制

**会话级状态**(常驻会话的必然结果,不是 bug):`set -e`、`trap`、**变量**、**函数**都**跨命令持久**。
`bash -c` 每条都是新进程,所以这一点与它不同——一条命令里的 `set -e` 会影响**后续所有命令**。

**状态的边界现在是「会话」** —— 并发化之后这件事才被显式写出来:不写 `--session` 就是**默认
会话**,与引入它之前完全一致(变量、`set -e`、`trap`、函数照旧跨命令累积);写了不同的会话名,
它们之间**互相隔离**,而这是并行必须付的代价 —— `shell.Session` 里的 interp runner 本身不
并发安全,一个会话只能有一个执行者。

- **同一会话内**:两条请求同时到就**排队**,严格 FIFO、不乱序。所以「先 `set -e` 或
  先定义函数,再发下一条用它」这类有顺序依赖的写法,靠「同一个会话」就能成立
  (`TestSameSessionKeepsStateAndStaysSerial`;注意 `cd` 例外 —— 它每次都按调用方的 cwd 覆盖回去,见下)。
  **队列有界**:每个会话最多排 **64** 条,满了会以 `exit 1` 明确拒绝并提示「等前面跑完或换一个会话名」
  —— 不静默丢弃,也不无限堆积。
- **不同会话名之间**:真并行。实测 8 条 `sleep 1`(不同会话名)从 **8.246 s** 降到 **1.077 s**
  (2026-10-17,AMD Ryzen 9 8945HX,Go 直接起进程 + 独立 `--endpoint` 口径);
  测试里 3 条(`TestSessionsRunInParallel`)的硬上限是 1.8 s —— 串行会是约 3 s。
- **具名会话上限 16 个**(默认会话不占名额),超了 `exit 1` 明确拒绝,**不回收**最久未用的那条:
  回收等于静默销毁状态,而状态正是这个会话存在的理由(`TestSessionLimitIsRejected`)。
- 每个会话一个独立 `shell.Session`;② 层的 pwsh 引擎是**按需**启动的,所以只用 ①/③ 层的会话
  不额外占内存,也不付那 ~350 ms。

**需要「每条命令彼此独立」时用 `--fresh`**(别名 `--no-state`):这条命令跑在一个**一次性会话**里,
跑完就丢。隔离是**双向**的 —— 前面的变量 / `set -e` / `trap` / 函数影响不到它,它设的也不会留给后面:

```bash
winbash -c 'set -e; PERSIST=1'          # 默认:会话持久
winbash -c 'echo $PERSIST'              # → 1
winbash --fresh -c 'echo ${PERSIST:-无}' # → 无
winbash --fresh -c 'LEAK=1'
winbash -c 'echo ${LEAK:-无}'            # → 无
```

默认仍然是持久(不破坏既有行为),代价说清楚:`fresh` 会话用到 ② 层时,常驻 pwsh 引擎挂在
Session 上,所以要重新拉起一次(约 350 ms);只用 ①/③ 层没有额外开销。`--fresh` 与
`--serve`/`--stop`/`--status` 组合是用法错误(`exit 2`),**与 `--session` 同用也一样**:
一个要求「只跑这一条」,一个要求「落到长期具名会话」,语义正交到没法凑一起。都不会静默忽略。

**不持久的两样**,写出来免得拿它们当配置用(实测,两样都与真 `bash -c` 一致,**不是缺陷**):

- **`cd`** —— client 每次把自己的 cwd 随请求带上,daemon 先切过去,于是覆盖掉上一条留下的目录。
  一条命令的 `cd` 不会污染下一条,这正是 `bash -c` 的语义。副作用是**跨调用的 `cd` 不累积**:
  一次调用里 `cd D:\work` 后 `pwd` 是对的,但**下一次**调用的 `pwd` 回到调用方目录。
- **`alias`** —— 非交互式下 `expand_aliases` 本来就没开。真 bash 同样如此:
  `bash -c 'alias gg="echo hit"; gg'` → `gg: command not found`。所以它在这里当不了配置。

**当前限制**:

- **stdin 转发的形态与边界**:`echo x | winbash -c 'cat'` → `x` **已支持**,而且是**流式**的
  (2026-10-16 起:请求头之后跟裸字节流,边读边转、**不缓存到内存**)。因此
  `yes | winbash -c 'head -1'` 这类**上游永不给 EOF** 的管道也能跑,同样不受大小限制。
  剩两条边界:
  1. **交互终端下不读 stdin** —— 判据是它不是字符设备。否则 `winbash -c 'ls'` 这类
     **不读** stdin 的命令会永远挂住(无法从脚本文本判断它读不读)。代价:终端下
     `winbash -c 'cat'` 拿到空输入,而真 bash 会读 tty。
  2. **界靠时间而不靠字节**:流式不缓存,所以没有「stdin 太大」这回事;要防的是
     「一直流个不停」,用 `--timeout`(默认 300 s,同时约束执行与读 stdin)。
     命令**不读** stdin 而上游仍在灌数据时,daemon 在命令返回后立刻回收转发协程 ——
     这一条有回归钉着(见 `smoke-gitbash.sh` 的「流式 stdin」节)。
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

`go test ./...` 从 **winbash 会话里**跑也是绿的:`cmd/winbash` 与 `internal/daemon` 的
`TestMain` 会把 ③ 层派生的 `WINBASH_SESSION` 摘掉 —— 否则 client 会把每次调用都当成"嵌套",
`--status`/`--stop` 刻意拒绝,3 条 daemon 管理用例必然红(而且看着像产品 bug,不像环境问题)。

`smoke-gitbash.sh` 反过来**不宽容**:开头发现 `WINBASH_SESSION` 就直接以 `2` 退出并说明原因,
因为那 4 条 daemon 管理断言会齐刷刷假红 —— 从 winbash 会话里启动它的人看到的不是"入口错了",
而是"daemon 管理坏了"。另外它用**自己的临时端点**与带 PID 的临时文件
(`go test` 里的 `benchEndpoint` 是同一个理由),所以跑一次 smoke 不会停掉你正在用的 daemon
(会话里的变量、`set -e`、`trap` 都还在),两条 smoke 也能并行跑。

约束:**唯一直接依赖**是 `mvdan.cc/sh/v3`(实测带入 `golang.org/x/sys`、`golang.org/x/term` 两个间接依赖)。
依赖镜像要设 `GOPROXY=https://goproxy.cn,direct`(`proxy.golang.org` 在本机不通)。
设计文档与实现计划在 `docs/superpowers/` 下。
