# winbash 源码级优势与劣势分析

审计对象：`D:/work/中文 目录/winbash`（Go 1.26.4，唯一直接依赖 `mvdan.cc/sh/v3 v3.14.1`）
审计方式：4 个并行子代理按子系统拆解（ipc/daemon/client、builtins、winmgmt+pwsh、测试体系），
并对高风险结论逐一做**一手运行时探针复核**。所有标注「实测」的结论均在本机真实执行验证过。

规模：生产代码 4107 行（23 文件），测试代码 3785 行（26 文件），测试/生产 ≈ 0.92。

---

## 一、优势

### A1 架构定位准确，四层路由的判定逻辑干净且自洽

`internal/shell/classify.go:20-30` 是整个设计的核心，只有 11 行：

```go
func classify(cmd string, searchPath string) layer {
	if _, ok := builtins.Registry[cmd]; ok { return layerBuiltin }
	if _, ok := winmgmt.Registry[cmd]; ok  { return layerWinmgmt }
	if _, ok := builtins.Which(cmd, searchPath); ok { return layerPath }
	return layerMissing
}
```

关键决策写在了注释里且执行到位：①/② 命中后**不再看进程 PATH**——`ls` 永远是我们的 ls，
从根上消掉了 Windows 上 `find`/`sort`/`where` 重名冲突这个真正的病根。
`searchPath` 由调用方传入而非读进程环境，全仓库无一处 `os.Chdir`，进程环境全程不动。

### A2 常驻 daemon 的性能收益是真实的（实测）

本机环境固有进程创建开销 ≈ **706 ms**（`cmd.exe` 空转基线）。在此基线上：

| | 平均耗时 | 扣除基线后的净开销 |
|---|---|---|
| winbash 热路径（20 次） | 708 ms | **≈ 2 ms** |
| 真 Git Bash（20 次） | 887 ms | ≈ 181 ms |

结论：daemon 摊销**确实有效**，每条命令相对 Git Bash 省约 180 ms，上百条命令即分钟级→秒级。
⚠️ 但 README:20 声称的绝对值「首次 70 ms / 之后 22 ms」**在本机无法证实也无法证伪**——
进程创建开销（706 ms）完全淹没了信号。建议改用进程内 benchmark 度量，别用端到端计时。

### A3 「不静默做错事」是本项目最硬的真功夫，且每一条都有实测出处

这是全项目最有价值的部分——针对的都是 mvdan/sh **不报错但给错结果**的缺口：

| 防护 | 位置 | 挡住的静默错误 |
|---|---|---|
| `rejectProcessSubstitution` | shell.go（解析后、运行前走 AST） | Windows 上未实现，会把 `TODO: support process substitution` **打到 stdout** |
| `rejectUnsupportedParamExpansion` | shell.go | `${v/#pre/x}` 未实现且**原样输出原值、退出码 0** |
| `printfGuard` | shell.go | `printf -v` 把 `-v` 当普通参数输出，看着"成功"了 |
| `callGuard` 改写 `command -v` → `which` | shell.go | mvdan/sh 内建按**进程 PATH** 回答，对我们自己的 `ls` 和 Git Bash 的 `sed` 会说"没有" |
| `splitEchoFlags` | shell.go | `echo -en` 被当文本打印（真 bash 给两行） |
| `normalizeCdPath` | shell.go | `cd /` **静默什么都不做**；`cd /c/Users` 报错 |

每一条的注释里都写了实测的错误输出，来源可追（部分标注 `oils spec`）。这不是文档修辞，是真踩过。

### A4 MSYS2 与 Windows 交互的两个深坑，解法有实测依据

- **`msysWithNoGlob`（shell.go:565）**：Cygwin runtime 在「调用方不是 Cygwin 程序」时会自己再展开一遍
  通配符，导致 `find . -name '*.go'` 报 `possible unquoted pattern`。加 `noglob` 把展开权收回真正的 shell。
- **无扩展名脚本走 `env.exe`（runExternal）**：Git Bash `usr\bin` 里有 44 个靠 shebang 跑的脚本，
  Windows 的 CreateProcess 认不了。注释里记了两条**试过但不行的路**（`bash <script>` 会让 Perl 脚本
  `my: command not found`；`bash -c 'exec "$0" "$@"'` 引号在 Windows 转义下被搞坏）——这是有价值的负向知识。

### A5 「定位用一条 PATH、执行用另一条」的分裂被正确堵上

`envList`（shell.go）把构造出的 `searchPath` 注入**派生子进程**的 `cmd.Env`。注释记录了实测后果：
`xargs -n1 echo` → `echo: No such file or directory`；`ls | xargs -n1 echo | wc -l` → **静默返回 0**。
这类"静默少给结果"的 bug 最难查，这里堵住了。

### A6 ②层命令注入防护到位（实测）

统一走 `psQuote`（`winmgmt/common.go:186`）做单引号转义，svc/reg/pwshcmd 全部过这一道。
实测 `svc stop "foo; Remove-Item C:\ -Recurse"` → 退出码 1，服务名被当作**字面量**。
`ps.go:60` 的 `Get-Process -Id` 未走 psQuote，但靠 `isAllDigits` 校验兜住了（安全，只是属于"靠校验而非转义"）。

### A7 二进制安全有设计自觉

`ipc.go:29-34` 的 `Response.Out` 刻意用 `[]byte` 而非 `string`：

> json.Marshal 会把 string 里的非法 UTF-8 字节静默替换成 U+FFFD，而 []byte 走 base64 可以原样往返。

`ipc_test.go` 有对应回归保护。`cat` 实测 5 字节二进制（`0x00 0xff 0xfe 0x01 0x80`）原样通过。

### A8 嵌套调用死锁的预防考虑周全

`EnvNested` 机制解决「daemon 串行 + 内层连同一 daemon = 互相等到 300 s 超时」。更细的是
`NewSession`（shell.go:104-114）**过滤继承来的嵌套标记**——否则标记会经 daemon 漏进之后每条命令，
让「嵌套」变成常态，白白丢掉常驻速度。这个二阶效应想到了，很难得。

### A9 工程质量基本面扎实

- `go test -race ./...` **全绿，无 data race**（实测 3m16s）
- 错误风格统一：`errs.Errorf` 三行式（命令 / 问题 / 怎么办），②层引擎不可用时直接指路排查命令
- 依赖极简，仅 1 个直接依赖
- `grep` 用 `bufio.Reader.ReadBytes('\n')` 而非 `bufio.Scanner`，**主动避开 64KB 行长陷阱**，
  注释明写；`grep_test.go:204` 有 200KB 超长行回归
- registry 设计好（`registry.go:20`），加一个内建命令只改 1 处 map + 新建文件；`init` 赋值而非 map
  字面量以避开初始化循环
- 语料测试**真 fork Git Bash**（`corpus_test.go:318` `exec.Command(bash,"-c",script)`），
  不是硬编码预期值——README 这条声称属实

---

## 二、劣势（按严重度排序）

### P0-1 🔴 daemon 零鉴权，任意本机进程可任意代码执行（实测）

`ipc.go:17-22` 的 `Request` 只有 `{Op, Script, Cwd, JSON}`，**无 token、无版本号、无握手、无校验**。
端口文件（`%TEMP%\winbash-<user>.port`）内容是**明文地址**：

```
内容: [127.0.0.1:55826]
```

**我用一个完全无关的 Python 裸 socket 直接发请求**：

```
daemon 响应: ok=True exit=0
stdout: UNAUTH-EXEC-SUCCEEDED | Test User
```

`whoami` 返回的是 **daemon 属主身份** ⇒ 同机任何能读该文件的进程都能以 daemon 属主权限执行任意命令，
也能发 `{"op":"stop"}` 把别人 daemon 打停。这是本机提权/横向通道。

加剧因素：
- `daemon.go:31` `os.WriteFile(endpoint, ..., 0o600)`——`0o600` 在 Windows 上**无效**，
  实测文件是 `-rw-r--r--`，可被改写指向攻击者的监听器；
- client 完全不校验对面是不是 winbash daemon（`client.go:210-220` 读完地址就 dial）；
- `daemon.go:96-103` 的 switch **无 default**，`op` 不认识一律降级为执行脚本。

> 修复方向：端口文件写入随机 token + 每帧 HMAC；或改用 NamedPipe（项目已引入 `golang.org/x/sys`，
> 不必新增依赖）；`0o600` 换成显式 ACL。

### P0-2 🔴 一个"连上但不发数据"的连接可永久挂死 daemon

`daemon.go:87` `ipc.ReadMessage(conn)` **未设 `SetReadDeadline`**，而 `handle` 是同步内联调用
（daemon.go:55，串行处理）。实测：连上后一个字节不发 → 后续所有命令永久阻塞。
更糟的是空闲计时器用 `tcpLn.SetDeadline` 且只在 accept 前重置，handle 阻塞期间**永不触发**，
daemon 既不能服务也不能自愈，只能 `taskkill`。

### P1-1 🟠 ②层 pwsh 引擎失效后永不重建，一次失误整层瘫痪

`pwsh.go` 的 `dead` 原子布尔在 174/191/196/212/224 行**只有 `Store(true)`，全仓无 `Store(false)`**；
而 `shell.go:583` `psEngine()` 是 `if s.engine != nil { return s.engine, nil }`——**永久缓存**。
实测：脚本里一句 `Write-Host` 就会污染协议帧（长度前缀被写坏），报
`帧长度不合理(1280066888 字节)`，此后该 daemon 的所有 ② 层命令（`svc`/`reg`/`ps`/`sys`/`pwsh`）
**永久失效**，只能重启 daemon。

### P1-2 🟠 无取消机制 + 超时重试会让副作用脚本被执行两遍

- `daemon.go:105` `ctx := context.Background()`——daemon 侧**无超时、无取消**，
  client 断开也不会传播到解释器。
- `client.go:98-107` 注释自己承认「重试可能让脚本执行两遍」，但代码照做
  （`forgetStaleEndpoint` → `ensureDaemon` → 重跑同一 req）。
- 实测：`winbash --timeout 2s -c 'echo RUN >> f; sleep 6'` → `wc -l f` = **2**。

对 `mkdir`/`git commit`/写文件类脚本，这是**数据损坏级别**的问题。

### P1-3 🟠 串行处理下长脚本饿死所有后续命令

实测：`while :; do :; done` 配 `--timeout 3s`，client 放弃后 **daemon 仍在跑**，
后续命令永久排队；后台 `sleep 25` 期间第二条命令拿到 `exit=124`。

### P2-1 🟡 `ls` 不带 `-a` 会列出点文件——与 bash 直接冲突，且 README 未记录

`ls.go:93` 的遍历循环**没有任何点文件过滤**，注释反而写「Windows 上 os.ReadDir 本来就返回隐藏文件，
所以这里唯一需要补的就是 `.` 和 `..`」——作者把它当成了无需处理的特性。

实测对照（同一目录）：

```
-- winbash --        -- 真 Git Bash --
.hidden              d.txt
d.txt                normal.txt
normal.txt
```

这是 POSIX 语义（点文件默认隐藏），与 Windows 隐藏属性无关。README:121-134 的「已知差异」表**未收录此条**。

### P2-2 🟡 `grep -E` 的 Perl 简写检测有误伤，拒收合法 POSIX 表达式

`grep.go` 的 `firstPerlClass` 只做「成对跳过转义」，**不豁免括号表达式**。实测：

```
winbash:  grep -E '[\d]' d.txt   → exit=2，报「\d 属于 Perl 简写，在这里没有定义」
真 bash:  grep -E '[\d]' d.txt   → exit=1（合法：匹配反斜杠或字母 d）
```

POSIX ERE 里 `[\d]` 是合法的字符类。README:54 声称「明确拒收并给替代写法」是**有意**的，
但这条误伤会让合法脚本凭空失败，属于过度拦截。

### P2-3 🟡 内建命令功能面偏窄，几处语义偏差

- `cat` **完全没有 flag 解析**（`cat.go:29` 起把每个 argv 当文件名），实测 `cat -n f` → 输出内容但 `exit 1`。
- `ls -l` 只有 4 列（`权限 大小 时间 名`，`ls.go:115`），**无属主/组/链接数、无列对齐**，
  时间格式是 `2006-01-02 15:04:05` 而非 bash 的 `Oct 14 17:26`。`-R -t -r -1 --color` 全部 exit 2。
- `grep -r` 输出**绝对路径**（`C:/.../sub/f.txt:hit`），真 bash 给 `./sub/f.txt:hit`；且**不跳过 `.git`**。
  现有测试用 `strings.Contains`（`grep_test.go:178`）正好漏掉这个——**测试盲区**。
- `grep` 的 `opts.fixed` 字段在 `grep.go:171,173` 赋值但**全包无一处读取**，
  `-F` 是"恰好正确"而非"正确实现"。
- `which` 默认 PATHEXT 是 `.COM;.EXE;.BAT;.CMD`（`which.go:16`），缺少 `.ps1` 等；
  ④ 层在 which 里完全没有体现。

### P2-4 🟡 README 的「值保证」与实现存在落差：三个函数是死代码

全仓 grep 确认：`output.go` 的 `FormatDWORD`、`FormatServiceState`、`StripANSI`
**仅被它们自己的 `output_test.go` 引用**，生产链路（`winmgmt`/`pwsh`）从不调用。

- 「`0xa` → `10`」「`STATE: 1` → `Stopped`」实际靠 PowerShell 侧 `.ToString()` 实现
  （`svc.go:145`、`reg.go:108`），Go 侧那两个函数是空转的；
- 「任何输出无 ANSI 色码」**链路上没有 strip 兜底**，只靠 `pwsh.go:109-112` 的
  `CREATE_NO_WINDOW` 让 pwsh 检测不到终端。一旦接入 ConPTY，色码会直接泄漏且无处拦截。

### P2-5 🟡 测试覆盖率结构性失衡：最关键的进程生命周期几乎没测

实测 `go test ./... -coverprofile`（total **68.3%**）：

| 包 | 覆盖率 | 说明 |
|---|---|---|
| `cmd/winbash` | **0.0%** | main/run/defaultEndpoint/sanitize 全 0 |
| `internal/client` | **28.0%** | **9 个函数 0%**：`spawnDaemon`/`dial`/`exchange`/`readEndpoint`/`ensureDaemon`/`forgetStaleEndpoint`/`dialAddr`/`oneShot`/`Status` |
| `internal/ipc` | 42.5% | `DecodeRequest`/`EncodeResponse`/`DecodeResponse` 全 0 |
| `builtins/registry.go` | 33.3% | `catBuiltin`/`grepBuiltin`/`egrep`/`fgrep` 全 0 |
| `internal/daemon` | 63.0% | `shellQuote` 0% |
| `internal/shell` | 81.1% | `Dir` 0%，`msysWithNoGlob` 42.9% |

**daemon 的拉起、拨号、端口文件残留处理、协议编解码——也就是 P0/P1 那些 bug 所在的区域——全部未测。**
这解释了为什么零鉴权、挂死、重复执行这些问题能一路活到今天。

### P2-6 🟡 语料测试的「钉住」有水分，且全量测试当前是 FAIL

- 131 条语料中 **13 条 `known` 走 `corpus_test.go:45` 直接 `return`，不做任何断言**
  （只在"两边恰好一致"时才 Skip）。README:119「差异全部有测试钉住」实际是**钉了个空壳**，
  真比对只有 118 条。
- 本机全量 `go test ./...` **FAIL 3 条**（`internal/shell`），均由含 `rm` 的语料触发。
  根因经核实是**主机环境注入**：`BASH_ENV` 指向 WorkBuddy 的 `safe-delete-bash-env.sh`，
  它把 `rm` 包成 bash **函数**（真 bash `type rm` → `rm is a function`），而 winbash 不读 `BASH_ENV`。
  ⇒ 这 3 条**不是 winbash 的 bug**，但暴露了两件事：
  1. 真差异：winbash 不遵循 `BASH_ENV`，继承的 shell 函数垫片会分叉；
  2. **测试设计缺陷**：语料把文件写进**包源码目录**（`corpus_test.go:178` `echo hi > wb-corpus-1.txt`）
     而非 `t.TempDir()`，且 `cmd.Output()` 丢弃 stderr，诊断信息完全不可见。

### P2-7 🟡 测试基础设施与可移植性

- **无 CI 配置**（`.github/` 不存在），无 benchmark（`grep "func Benchmark"` 为空），无 fuzz。
  唯一性能测 `TestEndToEndLatencyReport` 默认 Skip。
- 换一台**没有 Git Bash** 的机器，`corpus` 整组 + 约 11 个顶层测试会**静默 Skip**。
- 硬编码本机路径：`smoke-gitbash.sh:102-104` 写死 `D:/work/中文 目录`、`C:/Users/Test User`；
  `ipc_test.go:34` 同样。换机必挂。
- 弱断言较多：59 处 `strings.Contains`；`msys_script_test.go:38-42` 有 `_ = out` 丢弃输出后只查 exit。
- 全仓 **0 处 `t.Parallel()`**，无并发测试——而 daemon 恰是串行/并发问题的重灾区。
- 安全测试仅 1 条（`svc_test.go:194` 服务名单引号注入）。

### P2-8 🟡 其他一致性问题

- `daemon.go:108` `sess.Run(ctx, "cd "+shellQuote(req.Cwd), false)` **丢弃了 cd 的返回值**
  → cd 失败就静默在旧目录执行，违反本项目自己最强调的「不静默做错事」原则。
- `errs.ExitNoExec(126)` / `ExitNotFound(127)` 在 `builtins` 包内**零引用**，README 的退出码承诺在该层没有落点。
- 不转发 stdin（`echo x | winbash -c 'cat'` 拿空输入），README 已声明但仍是硬限制。
- `pwshcmd.go:287` 表头侧未走 `sanitizeCell`（值侧走了 `common.go:173-179`），属性名含制表符会列错位。

---

## 三、综合评估与适用场景

### 总体判断

**这是一个设计品味明显高于工程完成度的项目。** 作者对「静默错误」有近乎偏执的敏感，
并且把每一个踩过的坑都写成了可追溯的注释和防护——A3/A4/A5 那批防护是真金白银的经验，
四层路由（A1）也是对症下药的干净设计。这些都经得起源码检验，README 在这部分的声称基本属实。

但工程完成度拖了后腿，且**薄弱区恰好集中在最危险的地方**：
`internal/client` 28% 覆盖、9 个函数 0%，而零鉴权 RCE、连接挂死、超时重复执行三个高危问题
全部出在这个未测区域。**测试投入的方向与风险分布错位**——①层内建测得细（80%+），
进程生命周期和协议层几乎裸奔。

另一个系统性问题是**文档与实现的落差**：死代码撑着「值保证」的承诺、
13 条语料不做断言撑着「全部有测试钉住」、`ls` 点文件差异未进差异表、性能数字无法复现。
这些不是恶意夸大，但会让人对 README 的整体可信度打折。

### 适用场景

**适合：**
- 单机、单用户、可信环境下的 **AI Agent 命令执行层**——这正是立项目标，而它在这个场景下
  确实解决了真问题（bash 语法 + Windows 能力 + 常驻省掉冷启动，实测每条省 ~180 ms）。
- 需要**批量短命令**的自动化：daemon 摊销的收益随命令条数线性放大。
- 文本管道类工作流（`ls | grep | ...`），①层语义稳定。
- 作为**研究 Windows 上 shell 兼容性**的样本：注释里对 MSYS2/Cygwin/mvdan-sh 缺口的实测记录很有参考价值。

**不适合：**
- **多用户机器 / 任何有非可信本地进程的场景**——在 P0-1 修掉之前，
  daemon 等于一个无鉴权的本地 RCE 端点，且权限是 daemon 属主的。
- **生产环境 / 长时间无人值守运行**——P0-2（一次挂死需人工 taskkill）、
  P1-1（引擎失效需重启 daemon）、P1-3（饿死）三个可用性缺陷叠加，自愈能力不足。
- **有副作用的脚本 + 慢命令**——P1-2 的超时重试会导致脚本被执行两遍。
- **CI 或跨机器分发**——无 CI、硬编码本机路径、无 Git Bash 时大批测试静默消失。
- 追求 bash 高保真的迁移场景——点文件、`ls -l` 列、grep 路径形式等多处偏差。

### 建议的修复顺序

1. **鉴权**（P0-1）：端口文件写随机 token + 每帧校验；或换 NamedPipe。这是上线前的硬门槛。
2. **每连接 `SetReadDeadline`**（P0-2）+ **每请求 `context.WithTimeout`**（P1-3）：几行代码消除挂死与饿死。
3. **超时路径不再重试**，或加 request-id 去重（P1-2）：避免副作用脚本双跑。
4. **引擎失效自动重建**（P1-1）：`psEngine` 检测 `dead` 后重启，`s.engine` 置 nil。
5. **补 `internal/client` + `ipc` 的测试**（P2-5）：把 9 个 0% 函数覆盖起来，前 4 项修复才有回归网。
6. **修 `ls` 点文件过滤**（P2-1）与 **`firstPerlClass` 豁免括号表达式**（P2-2），并同步更新 README 差异表。
7. **`known` 语料改为断言「差异等于 known 描述」**（P2-6），别直接 return。

---

*审计过程中创建的临时探针文件已全部清理，工作树 `git status` 为空。*
