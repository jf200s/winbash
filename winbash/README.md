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

## 升级与协议版本

client 与 daemon 之间有协议版本(当前 **2**,见 `internal/ipc/ipc.go` 的 `ProtoVersion`)。
daemon 是**常驻**的(默认 30 分钟才退),且它由 client 用**同一个 exe 路径**拉起 —— 所以
「你替换了 `winbash.exe`、而旧 daemon 还活着」是**正常状态**,不是意外。

这种情况下新 client 连旧 daemon **会明确报错,不静默执行**:

```text
winbash: client: daemon 没有协议版本(它是旧版),与本客户端(2)不兼容
怎么办: 运行 `winbash --stop` 停掉旧 daemon,下次调用会自动用新 exe 重建
```

为什么宁可报错也不静默降级:协议一旦变化(例如 2026-10-16 把 stdin 从「一次性缓存」
改成「流式转发」),旧 daemon 会**忽略**新 client 发来的 stdin —— 脚本照样 `exit 0`,
而输入其实丢了。那正是本项目「不静默做错事」要消灭的失败形态,不能在新入口复现。

处置只有一步:**`winbash --stop`**。`--stop` 与 `--status` **刻意不参与版本检查** ——
否则「版本不匹配时先 `--stop`」这个推荐处置会被版本检查自己挡掉,陷入「要停它得先连上它」的死锁。

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

**性能怎么量**:**先说口径 —— 口径决定能不能测出信号**。两种,别混:

| 口径 | 结果 | 什么时候用 |
|---|---|---|
| **PowerShell 直调进程**(`& $exe -c '…'` + `Stopwatch`) | 信号干净。2026-10-17 实测:冷启动(拉起 daemon + 执行)**63 ms**、已热**15 ms**、批量 12 条 **595 ms**(50 ms/条) | 报绝对值用这个 |
| **端到端**(Git Bash 里手测) | 本机**测不出信号** —— 启动一个进程约 700 ms(`cmd.exe` 空转基线),把 winbash 的开销整个淹掉 | 只做相对比较,别报绝对值 |

正因为在 PowerShell 直调口径下测出了信号,开头那对数字(冷 **63 ms** / 热 **15 ms**)已
不再标「早期观察」;**Git Bash 口径仍测不出**,所以别在那个口径下引用绝对值。

能复现到微秒的是进程内 benchmark:

```powershell
go test ./internal/shell/ -run '^$' -bench BenchmarkSessionRun -benchtime=200x
```

它量的是 interp 的一次 `Run`(本机:纯 `echo` 约 **6.4 µs/op**,`echo hello | grep hell` 约 **119 µs/op**)。
端到端那 20 ms 量级里,绝大部分是**进程创建与 IPC 往返**,不是 winbash 本身。

**① 层不比真 bash 快**(实测 20 次 `echo x`:winbash 0.783s vs 真 `bash -c` 0.689s,差异在噪声量级内)。
它的价值是**语义可控** —— `ls` 永远是自己的 `ls`,不被 PATH 上重名的 Windows exe 抢走 —— **不是快**。
省时间的地方在 **② 层**(吃掉 pwsh 的 ~350 ms 冷启动):**3.7 倍**是评估报告的口径
(daemon 冷、逐次摊),而 2026-10-17 在「引擎已热、连跑 5 次」下量到 **20.6×**
(2038 ms → 99 ms)。两个都真,口径不同 —— **别混着引用**,对外说法用「重复调用 ② 层比每次
冷启 pwsh 快一个数量级」。再加上「不用换语法」,别指望纯 bash 那层提速。

## 已知差异与限制

与真 bash 相比的差异(全部有测试钉住,见 `internal/shell/corpus_test.go`):

| 现象 | 说明 |
|---|---|
| `pwd` 给 `D:\...` | 刻意:这一层的返回值要能直接喂给 Windows 工具(bash 给 `/d/...`)。**例外**:`cd /` 后 `pwd` 给 Git 安装根(真 bash 的 `/` 就是它) |
| glob 给 `.\x` | 同理,Windows 原生形式;①/②/③ 层两种写法都吃 |
| `${v/#前缀/替换}`、`${v/%后缀/替换}` | **不支持,报错 `2`**。mvdan/sh 没实现,而且会静默原样输出——我们宁可报错 |
| `printf %q`、`printf -v` | 同上:不支持但显式报错,并给替代写法 |
| `command -v find` / `type find` | 这两个是 mvdan/sh 的内建,按进程 PATH 回答(给 System32 那个)。准确答案用 `which find` |
| `set -u` / `${v:?}` | 退出码 `1`(bash 的 `-c` 给 `127`),错误文本不带 `bash: line N:` 前缀——都非零、都点名变量 |
| `${#关联数组[@]}` | mvdan/sh 的缺口:恒返回 `1`(键列表 `${!m[@]}` 与取值都是对的) |
| `IFS` 给**多字符** | mvdan/sh 把整个串当分隔符,真 bash 只取**第一个字符**(`IFS=BC` 等价 `IFS=B`)。**静默**差异,光看输出看不出对错 —— 要按多字符分,请用 `IFS=,` 这类单字符,或先 `tr` 转换 |
| `printf '%.0s'` 这类宽度为 0 的格式 | mvdan/sh 的内建 printf 不认(打 `invalid format char`),而且**退出码仍是 0** —— 唯一"有 stderr 但退出码不非零"的形态,靠退出码判断成败的调用方要当心 |
| `grep` 遇到含 NUL 的文件 | **输出策略与真 grep 不同**:真 grep 一旦看到 NUL 就切二进制模式,只打一行 `Binary file X matches`;我们**保持逐行输出**(这样 `-c` 的计数与行号都可复现 —— 真 grep 在二进制模式下 `-c` 会给出另一个数),但匹配行里含 NUL 时**往 stderr 说一句**,不静默。`-l` / `-q` 不吐内容,不提示 |
| `grep -E '\d'`、`(?i)` | 真 bash **静默**按字面处理(`\d` 退化成 `d`);我们**明确拒收**并给 POSIX 替代 |
| `grep -E 'a*?'` | **接受**,按 ERE 语义(`*` 后跟字面 `?`)。它是**已知差异**:真 bash 也接受,但两边对同一输入的匹配数不同(winbash 2 / 真 bash 0)—— Perl 的"懒惰量词"在这里没意义。早先 README 把它写成"拒收",与实际实现不符,已改正 |
| `grep -E '[\d]'` | **支持**,按 ERE 原意匹配「反斜杠或字母 d」。Go 的 `regexp.CompilePOSIX` 不认字符类里的 `\`,内部归一化成 `\\` 再编译,而不是退化成 Perl 语义。两个边界同样按 ERE 处理:**已转义**的 `\\` 不会再转一次(`[\\d]`、`[\\]` 仍合法),`[` / `[^` 后**紧跟**的 `]` 是字面量(`[\]]` 不被截断)。这 11 个形状有 golden 表钉住,期望值由真 Git Bash 生成(`go test ./internal/builtins/ -update-grep-golden`),不手写 |
| `more` | Git Bash 里没有,我们用了 Windows 的——多一个工具,不是缺陷 |
| **MSYS 挂载点** | `/`、`/tmp`、`/usr/bin`、`/bin`、`/c`…**支持**,与真 Git Bash 的挂载表对齐(`/` = Git 安装根,`/tmp` = 用户 `%TEMP%`)。所以 `mktemp`、`echo x > /tmp/f`、`cd /c/Windows`、`[ -d /tmp ]` 都能用。**一条限制**:`[ -e /etc ]` 这类"根挂载点下的子路径"走 `test`/`[` 时**不支持** —— mvdan/sh 会先把路径拼成 `<cwd>\etc`,而相对路径 `etc` 拼出**同一个字符串**,无法区分,所以不猜(猜错会把大量相对路径误判成"存在")。同样的判断用 `ls /etc` 就行(那条能拿到原始路径) |
| `ls` 默认不列点文件 | **与 bash 一致**(POSIX 语义)。要列用 `-a`;`-a` 时 `.` / `..` **参与同一个排序**(与 bash 一致),不是恒在最前 |
| `ls -l` 只有四列 | 权限 大小 修改时间 名字;没有属主/组/链接数(Windows 上没有 POSIX 属主),时间格式也不同 |
| `grep -r` 的输出路径 | 基于**你写的那个路径**算(`grep -r b .` → `./d.txt:b`),与 bash 一致 |
| `cat -n` 的行号 | `%6d<TAB>`,与 bash 相同;**多文件时跨文件累加**(`cat -n f1 f2` 里 f2 接 f1 的编号,与 GNU cat 一致);`-A`/`-b`/`-e` 等控制字符可视化开关**报错"未实现"**而不是装作支持 |
| `pwsh` 只有三种形式被接管 | `-c` / `-Command` / 脚本文件;其余形式报错并指路到原生 pwsh(不静默回落) |
| `pwsh -c "$env:X"` | 双引号里的 `$env` 会被 bash 先展开成空 —— **与真 bash 一致**,要写单引号 |
| `pwsh` 的列名按字母序 | 真实 PowerShell 会按对象属性定义顺序排版;我们排序是为了让两次运行的列序一致 |
| 超时**不会**重放脚本 | 客户端超时后直接报错。早先它会重拉 daemon 把同一个请求原样重跑,于是 `echo RUN >> f; sleep 6` 配 `--timeout 2s` 会让文件出现**两行** —— 对写文件类脚本是数据损坏 |
| 坏连接最多挡住 5 秒 | daemon 串行处理连接,一个只连不说话的连接会让后续命令排队;它会在 5 秒后丢掉这条连接。修之前没有这个上限,能一直挡到客户端自己超时 |

**会话级状态**(常驻会话的必然结果,不是 bug):`set -e`、`trap`、**变量**、**函数**都**跨命令持久**。
`bash -c` 每条都是新进程,所以这一点与它不同——一条命令里的 `set -e` 会影响**后续所有命令**。

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

约束:**唯一直接依赖**是 `mvdan.cc/sh/v3`(实测带入 `golang.org/x/sys`、`golang.org/x/term` 两个间接依赖)。
依赖镜像要设 `GOPROXY=https://goproxy.cn,direct`(`proxy.golang.org` 在本机不通)。
设计文档与实现计划在 `docs/superpowers/` 下。
