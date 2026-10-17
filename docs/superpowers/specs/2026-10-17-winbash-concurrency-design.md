# winbash 并发设计(2026-10-17)

**状态：待 review，尚未实现。** 本文只描述设计与取舍，不改代码。

> 一句话：默认行为一字不变（会话持久、请求串行）；新增**显式会话名**，不同会话名之间才真正并行。
> 这同时修掉「并发首次启动留下一堆孤儿 daemon」——那个是纯 bug，与并发模型无关。

---

## 1. 要解决的两件事

### 1.1 孤儿 daemon（bug）

`client.ensureDaemon`（`internal/client/client.go:363`）是「读端口文件 → 读不到就 spawn → 轮询等端口文件出现」。第 364 行读失败到第 367 行 `spawnDaemon` 之间**没有任何互斥**，而 `spawnDaemon` 只 `cmd.Start()` 不等待（`:401`）。

多个 client 同时首次调用时，每个都读到「没有 daemon」，于是各 spawn 一个。因为 daemon 用 `net.Listen("tcp","127.0.0.1:0")` 取随机端口，**它们全都绑定成功**，不存在「后来者失败退出」。最后写端口文件的那个成为赢家，其余全部变成孤儿。

实测（8 个 client 同时首次调用，端点隔离）：

```
并发 8 个首次启动: 非零退出=0
  +0s/+1s/+3s/+10s   存活 --serve 进程 = 8 个(全都活着)
端口文件只指向其中 1 个
--stop 之后         仍存活 = 7 个   ← 停不掉,只能等 30 分钟空闲超时
```

危害不只是"多几个进程"：**赢家之外的 7 个各自持有一个 `Session`**（其中用到 ② 层的还会各持一个常驻 pwsh），白占内存；而 `--status` 看不到它们、`--stop` 停不掉它们。

### 1.2 并发请求完全串行（设计欠账）

`daemon.Serve` 是 `Accept → handle` **同步**调用（`internal/daemon/daemon.go:82-90`），`Session` 全生命周期只建一次（`:72`），而 `shell.Session` 的注释写着「不保证并发安全(interp.Runner 本身也不安全)」（`internal/shell/shell.go:46`）。

于是并发请求严格排队。实测（同时发 N 条 `sleep 1`）：

```
并发 2 条 → 总墙钟 2.074s      单条区间 [1.053s .. 2.074s]
并发 4 条 → 总墙钟 4.155s      单条区间 [1.066s .. 4.155s]
并发 8 条 → 总墙钟 8.246s      单条区间 [1.055s .. 8.246s]
对照:串行 4 条 → 4.169s
并发 8 条 echo ok → 29ms       ← 轻命令看不出排队
```

注意最后一行：**client 进程级并发已经具备**（8 个进程启动与 IPC 往返是重叠的），缺的只是 daemon 侧的执行并发。所以轻命令感觉不到问题，而 `go test`、构建、网络请求这类会互相等。

设计文档与实施计划（`docs/superpowers/specs/2026-10-13-winbash-design.md`、`plans/2026-10-13-winbash.md`）**全文没有讨论过**并发、多客户端、`Session` 并发安全；协议里也没有会话标识。当初选持久化的理由只有性能（15ms vs 351ms）。也就是说：锁死并发是当时**没算进去的账**，不是有意的取舍。

---

## 2. 主流怎么做（为什么选「会话内串行 + 跨会话并行」）

三个互相独立的生态给出同一个答案。

| 系统 | 单会话 | 并发怎么做 | 原话 |
|---|---|---|---|
| Jupyter（JEP 91，官方标准提案） | 严格串行，后续请求排队 | 新增 subshell，跨 subshell 并行 | "Within an individual subshell, requests are processed sequentially"；"concurrency is *across* contexts, not *within* a single context" |
| PowerShell（MS-PSRP 协议规范） | 一个 runspace 一次只能跑一条管道 | `RunspacePool`（有界池，超额排队） | "Only one pipeline can be executed in a runspace at a time"；"Runspaces lack the necessary synchronization to support concurrency" |
| Claude Code（Bash 工具） | 不适用（每次独立进程） | harness 判定只读命令才并发 | 并行调用共享 cwd 导致兄弟调用失败（issue #97902） |

PSRP 规范里还有一句与我们直接相关：

> "the PSRP provides no mechanism for specifying which runspace in a pool is to be used… if pipelines depend on state built up by previous pipelines in the same runspace, you must use a **RunspacePool of size 1** — which serializes execution."

即微软只能二选一：要状态连续性就整体串行，要并发就放弃状态连续性。**我们可以做得更细**：用显式会话名路由到具体会话，同时保住「同一会话内状态连续」与「跨会话并行」。代价是会话名必须显式传（每次调用都是新进程，没有稳定身份可用来自动识别）。

第三行那个 bug 是我们的反面教材：它之所以会串味，是因为选了「无状态 + 共享 cwd」。我们每个会话各有独立 `interp.Runner`（含独立 cwd），天然不存在这个问题。

---

## 3. 目标 / 非目标

**目标**

1. 并发首次启动只留 **1 个** daemon；`--stop` 能停掉它。
2. 显式会话名之间**真正并行**（8 条 `sleep 1` 的墙钟从 8.2s 降到 ≈1s）。
3. **默认行为零变化**：不写会话名时，仍是同一个会话、仍串行、状态仍跨命令持久。
4. 顺带收益：`--status` / `--stop` 不再被长命令饿死（今天就靠 5s 上限放弃）。

**非目标**

- 不做同一会话内的并行（`interp.Runner` 不支持，也不该假装支持）。
- 不改成「每条命令一个独立会话」（那等于退回 `bash -c`，本项目就没有存在理由了）。
- 不引入多 daemon 进程（N 份 pwsh、N 份端口文件、把孤儿问题放大 N 倍）。
- 不改持久化语义本身。

---

## 4. 设计 A：并发首次启动的跨进程锁

### 4.1 机制

锁文件路径 = `<endpoint>.lock`（与端口文件同目录，命名由端点派生）。

```
ensureDaemon:
  1. 读端口文件 → 成功则直接用（不变）
  2. 抢锁: os.OpenFile("<endpoint>.lock", O_CREATE|O_EXCL|O_WRONLY, 0o600)
     抢到 → 写 "pid\nunixnano\n" →
        double-check 再读一次端口文件(别人可能刚写好) → 有就释放锁返回
        → 否则 spawnDaemon → 轮询等端口文件(startupBudget=20s) → 释放锁 → 返回
     没抢到 → 轮询 20ms:
        端口文件出现 → 直接用(赢家已经起来了,不必自己 spawn)
        锁文件消失   → 回到第 1 步重试
        超过 startupBudget → 报错(沿用现有错误文案)
```

要点：**持锁时间只覆盖 spawn 与等就绪**（毫秒到 3 秒级），不覆盖命令执行——否则并发就没了。

### 4.2 陈旧锁

持锁者可能被强杀（`taskkill`、宿主回收），锁文件会留下。判定陈旧：锁文件 `mtime` 超过 `staleLockBudget`（建议 30s，远大于 `startupBudget` 的 20s）即视为陈旧，删掉并重新抢。

不靠「读 pid 再判断进程存活」：Windows 上 `os.FindProcess` 恒成功，真正的存活探测需要 `syscall`/`golang.org/x/sys` 的平台代码。

### 4.3 为什么不用别的方案

- **命名互斥量（`CreateMutexW`）**：最稳，但要直接引 `golang.org/x/sys/windows`。目前 `x/sys` 只是 `mvdan/sh` 带进来的**间接**依赖，而项目的硬约束是「唯一直接依赖 `mvdan.cc/sh/v3`」。为一把锁破这条不划算。
- **端口文件独占创建**：端口文件由 daemon 写（地址 + token 必须由 daemon 自己生成），client 无法原子预占；若 client 先占位再删，daemon 写入会与删除竞争，比锁文件更容易出错。
- **`flock` 风格的文件锁**：Windows 上要 `LockFileEx`，同样落到 `x/sys`。

---

## 5. 设计 B：显式会话与 daemon 并发模型

### 5.1 协议

`ipc.Request` 增加一个字段：

```go
// Session 是要落到的会话名。空串 = 默认会话(与今天完全等价)。
// 同名会话内严格串行且状态持久;不同名会话并发执行。
Session string `json:"session,omitempty"`
```

`ProtoVersion` **3 → 4**。理由：旧 daemon 不认识 `session` 字段，会把它当没写——于是 `--session build` 静默退化成默认会话，调用方以为已经隔离了。这正是本项目明令禁止的失败形态（与 `fresh` 当初升版本同一条理由）。

升级代价必须说清：**换新 exe 后旧的常驻 daemon 会报版本不匹配，用户需要跑一次 `winbash --stop`。** 错误文案与处置已存在（README「升级与协议版本」节），`--stop` / `--status` 刻意不参与版本检查，所以不会死锁。

### 5.2 客户端

- `--session <名>`：显式会话名。
- `WINBASH_SESSION_NAME` 环境变量：优先级低于 flag。目的是让**并行子代理**天然各用各的会话（外层注入一次，该子代理的所有 `winbash` 调用自动归入同一会话），不必每条命令都写 flag。日常单线程不设它 → 默认会话 → 行为不变。
- `--session` 与 `--serve` / `--stop` / `--status` / `--fresh` 组合的处理见 §6.5，原则同 `--fresh`：**不认识的组合报 `2`，不静默忽略**。

### 5.3 daemon 侧结构

```
Serve:
  listener 不变;Accept 循环改为每条连接 go handleConn(...)
  会话表: map[sessionName]*sessionEntry   (加一把表锁,只保护 map 本身)
  sessionEntry {
      reqs     chan *job      // 每会话一个队列:保证同会话 FIFO
      sess     *shell.Session // 独立 interp.Runner(含独立 cwd、变量、函数)
      engine   (由 sess 自己懒启动,见 shell.go:58)
      lastUsed time.Time
  }
  每个会话一个 worker goroutine:从 reqs 取 job → 执行 → 回写连接
```

为什么用「每会话一个队列 + 单 worker」而不是「每会话一把 mutex」：**mutex 不保证 FIFO**，同一会话的两条请求可能乱序执行。而乱序会直接毁掉状态语义——`X=1` 与 `echo $X` 颠倒就得到空值。队列 + 单 worker 让同会话严格 FIFO，且天然串行（一条连接一个 job，worker 一次只执行一个）。

队列容量建议 64；满了就在投递处阻塞（背压），不静默丢弃。

### 5.4 会话池上限

`MaxSessions` 默认 **16**。超过时**不新建**，返回明确错误（`exit 1`），提示当前上限与 `--stop`。

为什么不是「回收最久未用的会话」：回收会**静默销毁状态**，而状态正是这条会话存在的理由。宁可让使用者看到拒绝，也不要让他在另一条命令里发现变量没了。

### 5.5 默认语义零变化的论证

不写会话名时，`req.Session == ""` → 落到唯一的默认会话 → 单 worker 串行 → 与今天 `Accept → handle` 同步完全等价（连 FIFO 顺序都一样，因为队列是按连接被 Accept 的顺序投递的）。

验证方式：现有全部测试（`TestDaemonIsReusedAcrossClientProcesses`、`TestFreshSessionIsIsolated`、`TestManageCommandsGiveUpWhileDaemonBusy`、corpus / smoke）必须原样全绿，不许改断言。

---

## 6. 生命周期

### 6.1 `--stop`

并发之后 `--stop` **不再排在长命令后面**（今天的 5s 上限就是为此存在的）。语义定义：

1. 立即回 `OK`（客户端拿到确定答复）；
2. 给正在跑的请求 `stopGrace`（建议 5s）宽限；
3. 到点仍在跑的，取消其 ctx（解释器会停下）；
4. 关闭 listener、关闭所有会话（各自 `Close()` 收掉 pwsh）、删端口文件、退出。

### 6.2 空闲超时

今天的机制是 `Accept` 上设 deadline，超时即退出（`daemon.go:78-88`）。并发后 `Accept` 不再能代表「空闲」，改成看门狗 ticker：

```
空闲 = 没有任何在跑请求 且 所有会话的 lastUsed 都超过 idleTimeout
```

满足即退出（同样收回所有会话）。

### 6.3 `--status`

至少给出会话总数与在跑数，例如：

```
pid=…  cwd=…  addr=…
sessions=2 running=1
  default  pwsh=pid=37024 calls=2  idle=12s
  build    pwsh=not-started        idle=3m
```

默认会话的输出格式保持现状一行（`pwsh=…`），避免破坏解析；会话明细部分另起。

### 6.4 stdin 流式转发

`startStdinStream` 现在绑在连接与 `Session` 上（`daemon.go:281`）。并发后必须绑到**该 job 所属会话**，且明确：请求在队列里排队时**不读**连接上的 stdin（上游会被反压，这是可接受的，也是「同会话内长命令挡住后续请求」的自然含义）。

风险点：一条「永不给 EOF 的上游 + 不读 stdin 的命令」排在同会话队列里时，daemon 必须在命令返回后回收转发协程——这条现有回归（`smoke-gitbash.sh` 的流式 stdin 节）必须在并发模型下重跑。

### 6.5 `--fresh` 与 `--session`

`--fresh` 现在表示「一次性会话」（`client.go` 的 `Fresh` 选项）。它与会话名是两个正交机制，组合语义容易被误解（`--fresh --session build` 到底是「build 的一次性副本」还是「忽略 build」？）。

建议：**组合即用法错误（`exit 2`）**，与现有 `--fresh` + `--serve/--stop/--status` 的处理一致。理由同项目一贯原则：不静默忽略。

### 6.6 嵌套

内层带 `WINBASH_SESSION` 时改走 `runLocal` 本地一次性会话（不连 daemon），与会话池无关，无需改动。`WINBASH_SESSION_NAME` 是另一个变量，两者不冲突（嵌套下 `SESSION_NAME` 无意义，可忽略）。

---

## 7. 语义细则

| 场景 | 行为 |
|---|---|
| 同一会话，两条请求同时到 | **排队**，严格 FIFO，不报错 |
| 不同会话，同时到 | **并行** |
| 会话 A 设 `X=1`，会话 B `echo $X` | 空（隔离，这是并行的代价） |
| 会话 A 设 `X=1`，会话 A 再 `echo $X` | `1`（持久化保住） |
| `cd` | 本来就**不**跨调用累积（client 每次带自己的 cwd），并发不新增问题 |
| 会话数超过 `MaxSessions` | `exit 1` + 明确错误，不静默淘汰 |
| 旧 daemon + 新 client | 报协议不匹配，提示 `winbash --stop` |

---

## 8. 测试计划

**A. 孤儿修复（`cmd/winbash/e2e_test.go`，端点必须隔离）**

1. `TestConcurrentFirstStartSpawnsOneDaemon`：8 个 client 同时首次调用 → 全部 `exit 0`；统计 `--serve` 进程数为 1（用 `tasklist`/WMI 按 endpoint 过滤）；`--stop` 之后为 0。
2. `TestStaleLockIsTakenOver`：手工造一个 mtime 很老的锁文件 → 调用仍能成功（锁被接管）。
3. `TestConcurrentFirstStartAllClientsSucceed`：8 个并发里**没有**任何一条报「daemon 在 20s 内没有起来」。

**B. 会话语义**

4. `TestSameSessionKeepsState`：`--session s1 -c 'X=1'` → `--session s1 -c 'echo $X'` → `1`。
5. `TestCrossSessionIsIsolated`：`--session s1 -c 'X=1'` → `--session s2 -c 'echo ${X:-none}'` → `none`。
6. `TestSameSessionIsFIFO`：在同一会话里并发投递 `X=n; sleep 0.2` 与 `echo $X`，断言顺序不颠倒。
7. `TestSessionsRunInParallel`：两个会话各跑 `sleep 1`，总墙钟 < 1.6s（这是本设计的存在理由，必须有一条硬的）。
8. `TestMaxSessionsRejected`：把上限调成 2（测试注入），第 3 个会话拿到 `exit 1` 且错误点明上限。

**C. 生命周期**

9. `TestStopInterruptsRunningRequest`：一条长命令在跑时 `--stop` → 长命令以非零退出，daemon 消失。
10. `TestIdleTimeoutWithMultipleSessions`：两个会话都空闲到 `--idle` → daemon 退出、端口文件消失。
11. `TestProtoMismatchRejectsSession`：构造旧协议请求（proto=3）→ 必须报错，不许静默按默认会话执行。

**D. 回归（不许改断言）**

12. 全量 `go test ./... -count=1` 连跑 ≥2 次 + `smoke-gitbash.sh`（真 Git Bash）。
13. `-race` 单跑会话池相关包（串行跑，避免抢 CPU 干扰计时）。

---

## 9. 风险

| 风险 | 说明与缓解 |
|---|---|
| 协议升版强迫现网 `--stop` | 一次性代价；错误文案与处置已存在。**发布说明必须写。** |
| 同会话 FIFO 靠队列实现，比 mutex 复杂 | 但没有它，状态语义会静默崩坏；用测试 6 钉住。 |
| 每会话一个 pwsh（② 层） | 引擎是懒启动的（`shell.go:58`），只用 ①/③ 层的会话不付成本；用到 ② 层的会话各约一个 pwsh。 |
| `--stop` 取消在跑请求会造成「命令半途被杀」 | 与今天 `--timeout` 撞超时的后果一致（本项目已明确「超时不重放」）；宽限 5s 后仍强杀，属显式操作，可接受。 |
| 并发把 CPU 打满（多个 `go test` 同时跑） | 这是使用者自己的选择，不是本设计的缺陷；文档提示即可。 |
| 会话名混用导致「我明明 export 过」 | 硬规则：一个工具链/一个子代理的会话名**全程一致**；推荐用 `WINBASH_SESSION_NAME` 由外层注入一次。 |

---

## 10. 决策（2026-10-17 review 通过）

| # | 问题 | 结论 |
|---|---|---|
| Q1 | 会话数超上限时怎么办 | **拒绝 `exit 1`**，不回收最久未用——回收等于静默销毁状态 |
| Q2 | `--stop` 时正在跑的请求 | **立即回 OK → 宽限 5s → 取消 ctx**。不选「无限等它跑完」（那会把 `--stop` 变回挂死） |
| Q3 | `--fresh` + `--session` 组合 | **用法错误 `exit 2`**，与现有 `--fresh` + `--serve/--stop/--status` 一致，不静默选一个 |
| Q4 | `MaxSessions` 默认值 | **16**（够并行子代理用；引擎懒启动，不真用 ② 层就不额外起 pwsh） |
| Q5 | `--status` 是否加会话明细 | **加**；默认会话那行格式保持不变，明细另起，避免破坏现有解析 |

已确认不做的事（review 时明确排除）：多 daemon 路由、单会话内并行、默认并发（放弃持久化）、命名互斥量（会引入 `x/sys` 直接依赖）。

---

## 11. 实施顺序

两步分开提交、分开验证（每步都要能独立回滚）：

1. **P0 孤儿修复**（只动 `internal/client`，加一条锁辅助 + 3 条端到端测试）：不涉语义，可立即合。
   **已完成（2026-10-17）**：`acquireStartLock` + `staleLockBudget`，回归
   `TestConcurrentFirstStartSpawnsOneDaemon`（并发 8 个 → 恒为 1 个 daemon）
   与 `TestStaleStartLockIsTakenOver`（陈旧锁接管）。变异验证过：旁路锁后该测试立刻
   报「存活 daemon = 8」并失败，与实测一致。
2. **P1 会话与并发**（`internal/ipc` 升版本、`internal/daemon` 会话池、`internal/client` 传参、README/已知差异表、测试 A/B/C）：改动大，合入前必须做「同一 daemon 上先 A 后 B」的手工端到端序列（含并发首次启动、跨会话并行、`--stop` 打断、空闲退出）。

文档与实现必须同步：改了行为就同步 README 的「已知差异与限制」表（`--session`、`MaxSessions`、`--stop` 宽限、协议 4）。
