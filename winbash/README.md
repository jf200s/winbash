# winbash

bash 语法 + pwsh 能力的常驻 shell,给在 Windows 上干活的 AI 用。

> 解决的问题:AI 熟悉 bash 但 bash 管不了 Windows;pwsh 管得了但不熟、而且每次冷启约 480 ms。
> winbash 让 `ls | grep go` 这种命令照常跑,需要 Windows 能力时(服务、注册表、进程、系统信息)
> 不用换语法;上百条命令的总耗时从分钟级降到秒级。**连 `pwsh` 本身也被接管**,
> 所以 AI 顺手写的 `pwsh -Command '...'` 同样不再每次付 ~350 ms 的启动成本。

## 快速开始

**已加入用户 PATH**(本机 2026-10-17 配置:把 `winbash\` 目录追加进 `HKCU\Environment` 的 `Path`,
没有安装器、没有服务、没有注册表项),所以直接写命令名:

```powershell
winbash -c 'ls | grep go'
winbash -c 'svc list --running'
winbash --json -c 'sys mem'
```

**未配置的机器**(或想卸掉)用全路径 —— 路径含中文,记得引号;卸载就是从**用户 PATH** 里删掉 `D:\work\中文 目录\winbash` 这一条:

```powershell
& 'D:\work\中文 目录\winbash\winbash.exe' -c 'ls | grep go'
```

- 第一次调用会自动拉起常驻 daemon,之后每条命令:**冷启动 63 ms / 已热 15 ms**
  (2026-10-17 实测,见下一行的口径)。
  原先这里写的是「约 70 ms / 约 22 ms」并标注**早期观察、本机无法证实** ——
  用 PowerShell 的 `Stopwatch` **直调进程**复测后,数字与那个量级**对得上了**,
  所以去掉「无法证实」的措辞;详见「性能怎么量」里的口径说明。
- daemon 空闲 30 分钟后自行退出,不留僵尸进程;也可以显式管理:

```powershell
winbash.exe --status    # pid / cwd / 地址 / 引擎状态 + sessions=N running=M 与各具名会话的明细
winbash.exe --stop      # 停掉 daemon(不会为了停掉它而先把它拉起来)
```

## 命令行

| 参数 | 说明 |
|---|---|
| `-c` / `--script` | 要执行的脚本(也可以直接写位置参数:`winbash ls -l`) |
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

client 与 daemon 之间有协议版本(当前 **4**,见 `internal/ipc/ipc.go` 的 `ProtoVersion`)。
daemon 是**常驻**的(默认 30 分钟才退),且它由 client 用**同一个 exe 路径**拉起 —— 所以
「你替换了 `winbash.exe`、而旧 daemon 还活着」是**正常状态**,不是意外。

这种情况下新 client 连旧 daemon **会明确报错,不静默执行**:

```text
winbash: client: daemon 没有协议版本(它是旧版),与本客户端(4)不兼容
怎么办: 运行 `winbash --stop` 停掉旧 daemon,下次调用会自动用新 exe 重建
```

为什么宁可报错也不静默降级:协议一旦变化(例如 2026-10-16 把 stdin 从「一次性缓存」
改成「流式转发」),旧 daemon 会**忽略**新 client 发来的 stdin —— 脚本照样 `exit 0`,
而输入其实丢了。那正是本项目「不静默做错事」要消灭的失败形态,不能在新入口复现。
同一理由适用于 **3**:旧 daemon 不认识 `fresh` 字段,会当成没写 —— 于是 `--fresh`
静默退化成「复用会话」,而调用方以为已经隔离了。

同一理由适用于 **4**:旧 daemon 不认识 `session` 字段,会当成没写 —— 于是 `--session build`
静默退化成默认会话,并行的几条命令全挤在同一个队列里排队,而调用方以为已经分开了。

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

## 四层路由

每条命令由 `ExecHandler` 判层,顺序如下。

### ① 层:纯 Go 内建(bash 文本语义)

| 命令 | 参数 |
|---|---|
| `ls` | `[-l -a -h -1 -d] [路径...]`;默认按 POSIX **不列点文件**(要列用 `-a`)。`-d` 列**参数本身**而不展开目录:`ls -d */` 只给子目录名、`ls -ld 某目录` 只看那个目录的属性(与 bash 一致,也不打 `total` 行) |
| `cat` | `[-n] [文件...]`;无参数 = 读 stdin,`-` 也是 stdin |
| `grep` | `[-i -n -v -c -l -r -E -F -q] PATTERN [文件...]`;无文件 = 读 stdin |
| `which` | `[-a\|--all] 名字 [名字...]`;认识下面**四层**,①/② 层报名字,③ 层报绝对路径。`-a` 列出**所有**匹配位置(按 PATH 顺序);不认识的开关**响亮拒绝**(`exit 2`)而不是当名字搜 |

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

`grep` 的方言:**默认是 BRE**(真 grep 的方言,不是"整串字面")—— `.` `*` `[ ]` `^` `$` `\`
都是元字符,`\(...\)` 分组、`\{n,m\}` 区间重复,而裸的 `( ) { } + ? |` 是**字面**;
`-E` 走 POSIX ERE。

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
- **自带路径的写法**(`./x.sh`、`/c/x/y.sh`、`C:\x\y.exe`)按**会话 cwd** 解析,不是进程 cwd
  —— 否则 `cd /tmp && ./t.sh` 会去 daemon 启动时那个目录里找。判"能不能直接执行"看的是
  **文件头是不是 PE**,不是扩展名:`.sh`/`.py` 这类带 shebang 的脚本因此能像在真 bash 里
  一样跑(`usr\bin` 的 `env.exe` 读 shebang 找解释器);`.bat`/`.cmd` 既不是 PE 也没有 shebang,
  会显式报错并指路 `cmd /c`,而不是丢一句 `%1 is not a valid Win32 application`。
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
措辞在 `internal/builtins/oserr.go`,六类错误(`No such file or directory` / `Not a directory` /
`Permission denied` / `Is a directory` / 非法字符 / 未知 errno 的 `I/O error (Win32 error N)`)。

两处细节值得知道:
- **`Not a directory` 与 `No such file or directory` 在 Windows 上是同一个 errno(3)**,
  靠"路径里有没有把文件当目录用"的结构检查区分 —— 真 bash 分得很清,而它决定 AI 下一步是改结构还是找文件。
- **`cat 某目录`**:Windows 上 `os.Open` 一个目录会**成功**,失败要到第一次 `Read` 才发生(给
  `Incorrect function.`)。所以 `cat` 在 open 之后显式 stat 判目录,而不是靠错误码猜 —— 猜 `ERROR_INVALID_FUNCTION`
  是错的,那是通用错误码。

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

与真 bash 相比的差异。**这些差异的回归测试不在一个文件里** —— 原先这句话写的是「全部有测试钉住,见 `internal/shell/corpus_test.go`」,那是不准确的(那张表有 31 条,而 `corpus_test.go` 里只有一个 Test 函数)。实际分布是这样:

- **`corpus_test.go` / `pbb_corpus_test.go` 是差分测试**(拿真 bash 的实际输出当参照)。它们的 `known:` 语料是**豁免说明** —— 「这几条差异存在,比对时跳过」。所以一条差异被列进 `known:`,只说明它被**承认**,不说明它被**钉住**。这两件事别混。
- 真正把行为写死的是各层专项测试:`classify_test.go`(printf/trap 类缺口)、`commandv_test.go`、`grep_test.go` + `grep_binary_test.go` + `grep_classes_test.go` + `grep_class_scanner_test.go`、`ls_test.go`、`cat_test.go`、`msyspath_e2e_test.go`、`msysargv_test.go`、`trapguard_test.go`、`oilspec_gaps_test.go`、`internal/winmgmt/pwshcmd_test.go`、`internal/daemon/guard_test.go` + `stdin_cwd_test.go` + `state_test.go` + `fresh_test.go` + `boundary_test.go` + `serve_test.go`、`cmd/winbash/e2e_test.go`。
  (`daemon` 包的专项测试是分三轮补的,每轮都用覆盖率数据定位、用变异验证鉴别力:第一轮 `stdin_cwd_test.go` 补 **stdin 流式转发**与 **cwd 切换**(原为 0%);第二轮 `state_test.go` 补**会话池记账与关停超时**;第三轮 `fresh_test.go` + `boundary_test.go` + `serve_test.go` 补 **`--fresh` 的四个隔离维度**、**协议边界**(未知 `stdin_mode` / 坏帧 / 队列满 / 具名会话上限)与 **`Serve`/`watchdog` 的参数边界**。该包总覆盖 **71.6% → 94.5%**,`handleConn` / `watchdog` / `statusText` / `entryStatusLocked` / `waitWorker` / `closeSessions` / `submit` / `tokenOK` / `shellQuote` / `rejectJob` 均达 100%。)
- 还有三条**刻意不测**的(写了就是不稳定测试):`pickSession` 里「锁外建 Session 期间 daemon 开始关停」与「别人抢先建了同名」两处竞态窗口 —— 只在极窄调度窗口命中,靠 sleep 碰运气必然 flaky,而注入 hook 会让生产代码为测试让步;`newToken` 的 `rand.Read` 失败分支同理(需注入随机源)。这三处靠 code review 保证。
- 另外三条**变异验证证明抓不住**的,已写在各测试的注释里(不是缺口,是等价/不可观测):`pickSession` 的**第一处** `stopping` 检查(去掉后第二处仍会兜住,功能等价,只是白建一个 Session 再关掉 —— 第一处是性能优化);`TestBadFrameDoesNotWedgeDaemon` 只验证「坏帧不拖垮 daemon」,不验证「坏帧必须有错误响应」(实现选择是不回,因为对端根本没在讲这个协议);`Serve` 里 `net.Listen` 失败与 `writeResponse` 的 Encode 失败属于实际不可达路径。
- 有 5 条只是**弱覆盖**(测试覆盖了相邻行为,但没有逐字钉住这条差异):显式 `Git\bin\bash.exe` 的第二跳吃引号、`grep -E 'a*?'` 的匹配数差异、`ls` 默认不列点文件、`grep -r` 的 `./` 前缀、`pwsh -c "$env:X"` 的空展开。改这些地方时不能指望测试变红。**这份清单是照测试注释与断言盘的,不是机器判定** —— 某个用例后来被加强了,这里就会偏保守(写"弱"而其实够强),不会反过来。
  (原先这里还有一条 `ls -l` 的四列;那一列已经补成 7 列,不再算"弱覆盖",见表格里对应的行。)

**原先完全没有测试钉住的 2 条,本轮已补上**:`cat -n` 的号码格式(`%6d<TAB>`、跨文件累加,见 `cat_test.go` 的 `TestCatNumberedUsesSixWideTabFormat` / `TestCatNumberedKeepsCountingAcrossFiles`,以及未实现开关要响亮拒绝的 `TestCatUnimplementedFlagsAreRejected`)与 `printf '%(%Y)T'`(strftime 报 `invalid format char` 且 exit 1,见 `classify_test.go` 的 `TestPrintfStrftimeFormatIsRejectedLoudly`)。补之前这两条是「文档承认、行为也对、但改坏不会变红」的状态。

| 现象 | 说明 |
|---|---|
| `pwd` 给 `D:\...` | 刻意:这一层的返回值要能直接喂给 Windows 工具(bash 给 `/d/...`)。**例外**:`cd /` 后 `pwd` 给 Git 安装根(真 bash 的 `/` 就是它) |
| glob 给 `.\x` | 同理,Windows 原生形式;①/②/③ 层两种写法都吃 |
| `${v/#前缀/替换}`、`${v/%后缀/替换}` | **不支持,报错 `2`**。mvdan/sh 没实现,而且会静默原样输出——我们宁可报错 |
| `printf %q`、`printf -v` | 同上:不支持但显式报错,并给替代写法 |
| `command -v find` / `type find` | 这两个是 mvdan/sh 的内建,按进程 PATH 回答(给 System32 那个)。准确答案用 `which find`。**`command -v` 已被 guard 接管**:①/② 层内建与只在 Git Bash `usr\bin` 里的工具都答得对,**多名字也行**(`command -v ls cat`;`TestCommandVHandlesMultipleNames`)。一条残余缺口:**混合写法**会放行给 mvdan/sh —— 例如 `command -v ls git` 里 `ls` 仍被漏报(`git` 在进程 PATH 里、mvdan 自己答得对,而 `CallHandler` 只能返回一条命令,没法逐名字替换)。要逐个判存在就分开写:`command -v ls && command -v git`。`type -p` / `type -t` 与 `type find` 同待遇:**未接管**,按进程 PATH 回答 |
| 显式写 `Git\bin\bash.exe` 时参数里的双引号 | 那个 47 KB 的 exe **不是 MSYS 程序**,是 Git for Windows 的原生 launcher(导入表只有 `KERNEL32`/`msvcrt`/`SHELL32`,没有 `msys-2.0.dll`)。它把命令行**转发**给 `usr\bin\bash.exe`,这一跳用 MSVC 规则重拼,参数里的 `"` 到真 bash 手里已经变成 `\` —— **不是我们的转义问题**:对原生目标用标准 MSVC 规则是对的,坏在第二跳不受我们控制。按 PATH 找 `bash`(`which bash` 给的就是 `usr\bin\bash.exe`)完全正常,只有显式写全路径到 `bin\` 才会撞上 |
| `set -u` / `${v:?}` | 退出码 `1`(bash 的 `-c` 给 `127`),错误文本不带 `bash: line N:` 前缀——都非零、都点名变量 |
| `${#关联数组[@]}` | mvdan/sh 的缺口:恒返回 `1`(键列表 `${!m[@]}` 与取值都是对的) |
| `IFS` 给**多字符** | mvdan/sh 把整个串当分隔符,真 bash 只取**第一个字符**(`IFS=BC` 等价 `IFS=B`)。**静默**差异,光看输出看不出对错 —— 要按多字符分,请用 `IFS=,` 这类单字符,或先 `tr` 转换 |
| **管道段不是真子 shell** | **静默差异**(两边都 `exit 0`、都不报错,只有输出不同):`cmd \| <右侧>` 右侧对变量的赋值会**泄漏到外层**,而真 bash 里右侧是独立进程、赋值随进程消失。实测三种形态都泄漏:`printf "a\nb" \| while read -r _; do n=$((n+1)); done` 之后 `n=2`(真 bash 得 `n=0`);`echo x \| f` 里函数内的赋值;`echo x \| { export PX=1; }` 的 `export`。**根因是上游架构限制,不是本项目的 bug** —— mvdan/sh 的 README 明说 `Go does not support forking its own process, so subshells use a goroutine instead`,管道每段是一个 goroutine 而非进程,变量绑定因此共享作用域。**这一条的方向对 AI 干活有利**(`n=2` 才是直觉预期),要防的是它无声变掉;`oilspec_gaps_test.go` 的 `TestPipelineSegmentsAreNotRealSubshells` 同时钉住三件事:泄漏的三种形态、显式 `( )` / `$( )` / `{ }` 与 bash 一致的隔离(防止有人"修"泄漏时把对的也改坏)、以及推荐替代写法 `while ... done < file`(`TestRedirectWhileDoesNotLeak`) |
| `printf '%.0s'` 这类宽度为 0 的格式 | mvdan/sh 的内建 printf 不认(打 `invalid format char`),而且**退出码仍是 0** —— 唯一"有 stderr 但退出码不非零"的形态,靠退出码判断成败的调用方要当心 |
| `grep` 遇到含 NUL 的文件 | **输出策略与真 grep 不同**:真 grep 一旦看到 NUL 就切二进制模式,只打一行 `Binary file X matches`;我们**保持逐行输出**(这样 `-c` 的计数与行号都可复现 —— 真 grep 在二进制模式下 `-c` 会给出另一个数),但匹配行里含 NUL 时**往 stderr 说一句**,不静默。`-l` / `-q` 不吐内容,不提示 |
| `grep -E '\d'`、`(?i)` | 真 bash **静默**按字面处理(`\d` 退化成 `d`);我们**明确拒收**并给 POSIX 替代 |
| `grep '\d'`(无 `-E`) | 默认方言是 BRE,元字符与真 grep 一致;但 `\` 后跟**普通字符**时我们按**字面**处理(`grep '\d'` 只匹配 `\d` 这两个字符),而 GNU grep 会忽略反斜杠、连 `d` 一起匹配。刻意如此:不让"没匹配"被误读成 Perl 简写生效了 |
| `grep -E 'a*?'` | **接受**,按 ERE 语义(`*` 后跟字面 `?`)。它是**已知差异**:真 bash 也接受,但两边对同一输入的匹配数不同(winbash 2 / 真 bash 0)—— Perl 的"懒惰量词"在这里没意义。早先 README 把它写成"拒收",与实际实现不符,已改正 |
| `grep -E '[\d]'` | **支持**,按 ERE 原意匹配「反斜杠或字母 d」。Go 的 `regexp.CompilePOSIX` 不认字符类里的 `\`,内部归一化成 `\\` 再编译,而不是退化成 Perl 语义。两个边界同样按 ERE 处理:**已转义**的 `\\` 不会再转一次(`[\\d]`、`[\\]` 仍合法),`[` / `[^` 后**紧跟**的 `]` 是字面量(`[\]]` 不被截断)。这 11 个形状有 golden 表钉住,期望值由真 Git Bash 生成(`go test ./internal/builtins/ -update-grep-golden`),不手写 |
| `more` | Git Bash 里没有,我们用了 Windows 的——多一个工具,不是缺陷 |
| `mktemp` 的默认目录 | **不是 msyspath 的映射错误,是 MSYS 自己解析 `/tmp` 的结果**:winbash 的 `/tmp` 恒等于 `%TEMP%`;而 `mktemp.exe` 启动时若把 `/tmp` 解析成 `%TEMP%` 的**父目录**,它就会把 `TMPDIR` 投影成 `/tmp/<TEMP 的 basename>/tmp.XXX` —— 那个路径在两侧不是一个目录,`f=$(mktemp); cat "$f"` 于是读不回来(真 Git Bash 在同一个进程里也读不回来)。实测:直接派生 `mktemp.exe` 时稳定复现,经 `bash -c` 或 winbash ③ 层派生时约 1% 触发,**且触发一次后成片触发**(直到另一次干净的启动)。所以要跨工具链传临时路径,就在同一侧创建并消费;`internal/shell/msyspath_e2e_test.go` 对这种形态按「TEMP 的 basename 在路径里连续出现两次」跳过,而不是红 —— 判据精确到不会掩盖真的映射故障 |
| **MSYS 挂载点** | `/`、`/tmp`、`/usr/bin`、`/bin`、`/c`…**支持**,与真 Git Bash 的挂载表对齐(`/` = Git 安装根,`/tmp` = 用户 `%TEMP%`)。所以 `mktemp`、`echo x > /tmp/f`、`cd /c/Windows`、`[ -d /tmp ]` 都能用。**`cd` 的三种写法都规范化了**:`cd /x`、`command cd /x`、`builtin cd /x`(`TestCdThroughCommandAndBuiltinTakesEffect`)。**一条限制**:`[ -e /etc ]` 这类"根挂载点下的子路径"走 `test`/`[` 时**不支持** —— mvdan/sh 会先把路径拼成 `<cwd>\etc`,而相对路径 `etc` 拼出**同一个字符串**,无法区分,所以不猜(猜错会把大量相对路径误判成"存在")。同样的判断用 `ls /etc` 就行(那条能拿到原始路径) |
| `ls` 默认不列点文件 | **与 bash 一致**(POSIX 语义)。要列用 `-a`;`-a` 时 `.` / `..` **参与同一个排序**(与 bash 一致) |
| `ls` 的**开关支持面** | 支持 `-l -a -h -1 -d`。其余(如 `-R`、`-t`、`-p`、`--color`)一律**响亮拒绝**:`exit 2` + 列出支持的参数,不静默忽略。`-R` 另给指路(递归用 `find`)。**`ls -d` 的已知边界**:参数里含**反斜杠**时输出也带反斜杠 —— 因为 winbash 的 glob 用反斜杠分隔(`echo */` 给 `dirA\`),`-d` 又按"参数原文输出"实现(与 bash 行为一致),所以 `ls -d */` 给 `dirA\` 而真 bash 给 `dirA/`。两者都不是错的,只是各自的 glob 命名法;要干净的裸名字用 `for d in */; do basename "$d"; done`。**注意别用 `ls -a \| grep /`** —— `ls` 的名字不带斜杠,那条**没有任何输出**(实测 rc=1) |
| `ls -l` 与真 bash 一样是 7 列 | 权限 链接数 属主 属组 大小 时间 名字,单空格分隔,连同 `total` 行、数值列右对齐、时间两形态,都与真 Git Bash 对齐(`TestLsLongFormatHasSevenColumns`)。**属主/属组是进程级常量**:MSYS 不读文件的属主 SID,一律显示当前登录用户 —— 实测 `C:\Windows\notepad.exe`(属主是 TrustedInstaller)真 Git Bash 也显示 `Test User`,我们照此实现(`ownerName()` / `ownerIDs()`,`sync.Once` 缓存)。**属主名可能含空格**(Windows 账户名如 `Test User`),所以**不要**用 `awk '{print $3}'` 按位置取字段,那会**静默**错位(输出照样像对的);拿名字用 `awk '{print $NF}'`,要大小用 `awk '{for(i=5;i<NF;i++)if($i ~ /^[0-9]+$/){print $i; break}}'` 这类稳健写法,或直接用 `stat`。**`total` 行是近似值**:真 bash 的 total 是文件系统块分配数(实测 1B→1、1000B→4、5000B→8、200000B→196),依赖 NTFS 簇大小与 MFT 小文件驻留,不可跨盘复现;我们按 1024 字节块求和 —— **行在、位置对**(`tail -n +2` 能正确跳过),但数值可能与真 bash 不同。**一处已知偏差(2026-10-18 查明)**:展开目录时条目走 `DirEntry.Info()`,而 Windows 上它对**子目录**给的 size 是 0(`GetFileAttributesEx` 才给 4096),所以含子目录的 `ls -l` 的 total 会偏小 —— 实测含 40 个文件的子目录:真 bash `total 5`、winbash `total 1`。`-a` 的 `.` / `..` 走 `os.Stat` 能拿到真实 size,那条路径又反过来比真 bash 偏大(真 bash 让它们贡献 0 块)。**脚本不该依赖 total 的数值**,只该依赖"这行存在"。**权限位已对齐(2026-10-18)** 复刻了 MSYS 的算法(`lsmode.go`),19 个代表性条目与真 Git Bash 逐条一致 —— 目录 `drwxr-xr-x`、普通文件 `-rw-r--r--`、只读文件 `-r--r--r--`、可执行 `-rwxr-xr-x`、只读可执行 `-r-xr-xr-x`。**「可执行」的判据是 MSYS 那套**:`.exe` 扩展名(大小写不敏感)或文件头 `MZ` 或文件头 `#!`,三者之一;`.bat`/`.cmd`/`.ps1` **不可执行**,`.sh`/`.py` 光有扩展名也不可执行(要有 `#!`),ELF/PNG/ZIP 头都不可执行。所以这一列现在**可以**用来判可执行。**两条残留限制**:① winbash **没有 `umask` 内建**,权限按 Git Bash 的默认 `022` 固定算,`umask 077` 不会生效;② 符号链接显示目标类型(实测真 Git Bash 在这台机器上也一样,不给 `l` 前缀)。**`-h` 时大小列的文本形态也不同**(只有带 `-h` 才不同,默认的纯字节数完全对齐):GNU 在小于 1K 时打 `5` 而**不带单位**,我们的 `FormatBytes` 打 `5B`;GNU 在数值 ≥10 时取整(`196K`),我们恒保留一位小数(`195.3K`)。同一条规则也影响 `-lh` 的 `total` 行。读大小的脚本本来就不该用 `-h`,所以这条的优先级低,但它确实是差异 |
| `grep -r` 的输出路径 | 基于**你写的那个路径**算(`grep -r b .` → `./d.txt:b`),与 bash 一致 |
| `cat -n` 的行号 | `%6d<TAB>`,与 bash 相同;**多文件时跨文件累加**(`cat -n f1 f2` 里 f2 接 f1 的编号,与 GNU cat 一致);`-A`/`-b`/`-e` 等控制字符可视化开关**报错"未实现"**而不是装作支持 |
| `pwsh` 只有三种形式被接管 | `-c` / `-Command` / 脚本文件;其余形式报错并指路到原生 pwsh(不静默回落) |
| `read -d <分隔符>` | **支持**(2026-10-17)。这是**我们自己给 mvdan/sh 打的补丁**,不是上游功能 —— 见 `third_party/mvdan-sh/WINBASH-PATCH.md` 与 `go.mod` 的 `replace`。上游 v3.14.1 缺它,而缺的行为是**报 `invalid option` 但外层退出码 0、变量留空**,属于静默错值。现在 `IFS=$'\n' read -d "" -ra fd < f`、`while IFS= read -r -d '' x`(配 `find -print0`)都与真 bash 一致 |
| `mapfile -n <行数>` | **支持**(同上,本地补丁)。`0` = 不限(与 bash 一致)。这让 pure-bash-bible 的 `head`/`tail`/`lines` 三个函数真正可用。补丁前它们是「空数组 + exit 0」,而且**光看输出分不清"文件真的空"还是"选项没被支持"** |
| `trap <信号>` 只支持 `ERR` / `EXIT` | mvdan/sh 只实现这两个;其余信号(INT / TERM / USR1 / HUP / DEBUG / RETURN …)的上游行为是**stderr 报 `invalid signal specification`、退出码却是 0、hook 一个字节都没生效**。2026-10-17 起 `callGuard` 提前拦成响亮失败:`exit 1` + 三行式(点名信号 + 给替代写法)。`trap '' INT`(想忽略 Ctrl+C)这类"以为信号被屏蔽了"的写法受害最深,现在会明确报错。**边界精确**:只拦出现在**信号位置**的名字,`trap 'echo DEBUG' EXIT` 里作为 callback 文本的不误拦。`trap` 无参数(列出)不拦 —— 它只是少个查询功能,不产生错值。回归:`internal/shell/trapguard_test.go`(21 条子用例,含变异验证) |
| `printf '%(%Y)T'`(strftime) | **不支持**:报 `invalid format char: (`,**退出码 1** —— 是响亮的,不是静默。要格式化时间请用 `date`(③ 层,如 `date -u -d @0 '+%Y-%m-%d'`) |
| `pwsh -c "$env:X"` | 双引号里的 `$env` 会被 bash 先展开成空 —— **与真 bash 一致**,要写单引号 |
| `pwsh` 的列名按字母序 | 真实 PowerShell 会按对象属性定义顺序排版;我们排序是为了让两次运行的列序一致 |
| 超时**不会**重放脚本 | 客户端超时后直接报错。早先它会重拉 daemon 把同一个请求原样重跑,于是 `echo RUN >> f; sleep 6` 配 `--timeout 2s` 会让文件出现**两行** —— 对写文件类脚本是数据损坏 |
| 一条坏连接最多宽限 5 秒 | daemon 读完请求头就撤读预算,所以「只连、不说话」的连接会在 5 秒(`DefaultReadTimeout`)后被单独丢掉 —— 并发化之后它只占自己那支 goroutine 与一个 fd,不再让别人排队。修之前既没有这个上限(能一直挡到客户端自己超时),又因为是串行处理而拦住**所有**人 |
| 并发**首次**启动只起一个 daemon | **已修(2026-10-17)**。原先 `ensureDaemon` 只看端口文件能不能读出来,不验证对端存活,也没有跨进程启动锁 —— 多个 client **同时**发现"没有 daemon"时各 spawn 一个。而 daemon 用 `127.0.0.1:0` 取随机端口,并发起的每一个**都能绑定成功**,不存在"后来者失败退出"这种天然去重,于是后写端口文件者赢,先起的全成孤儿(`--status` 看不到、`--stop` 停不掉,各自还抱着一个 Session,只能等 30 分钟空闲自己退)。实测 8 个 client 同时首次调用:**修前 8 个 `--serve` 进程、`--stop` 只停掉 1 个;修后恒为 1 个**。做法是 spawn 之前抢一把跨进程锁(`<endpoint>.lock`,`O_CREATE\|O_EXCL` 原子创建),持锁只覆盖"拉起 daemon + 等就绪"、**不覆盖命令执行**(否则并发就没了);抢不到的一方改为轮询端口文件,赢家一起来就直接用。陈旧锁(持锁者被强杀)按 mtime 超过 30s 接管 —— 否则一次强杀会让该端点永久起不来,而表现只是"daemon 在 20s 内没有起来",极难归因。回归:`TestConcurrentFirstStartSpawnsOneDaemon` / `TestStaleStartLockIsTakenOver` |
| client 的 cwd 切换失败时,**错误可见但退出码不反映** | daemon 收到 `cwd` 后先 `cd` 过去,**失败不致命**:记一条错误行(`client: 无法切换到 <路径>,已在原目录 <实际目录> 下执行`)、脚本继续在原目录跑、退出码仍取**脚本**的退出码。所以 `cd` 失败时你可能看到 `exit 0` —— 但 stderr 里有那两行。这是有意的取舍(cd 失败不该掩盖脚本本身的结果),`TestCwdFailureIsReportedNotSilent` 钉住"错误必须可见且点出路径与原目录"。**若你要靠退出码判断"到底在哪个目录执行的",请显式写 `cd <dir> && <命令>`** —— 脚本里的 `cd` 失败会正常让整条命令非零 |

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
