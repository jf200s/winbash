# winbash 会话语义专项设计:EXIT trap / export PATH / 后台 job(2026-10-31)

**状态:待用户拍板,尚未实现。** 本文只描述设计与取舍,不改任何产品代码。

> 一句话:三个分叉同源于「**每次请求是一次 Run**」的会话模型 —— 会话是常驻的 `interp.Runner`,请求是喂给它的一段 `*syntax.File`,请求结束 ≠ 会话结束,于是「退出」「环境」「进程生命周期」这三件在真 bash 里绑在同一个进程上的事,在 winbash 里各自有了不同的终点。本文逐项给现状证据、选项表与推荐;结尾给「若全部按推荐落地」的分批实施顺序。

审计与用户已确认三项一起定夺。写作口径与既有 spec 一致(2026-10-13 设计、2026-10-17 并发设计):**实测先行,每条结论给 file:line 或命令输出**。

---

## 0. 共同根源:会话模型一小节

先摆模型,三个分叉都从这张图读出来:

```
client 请求 ──► daemon.runJob(internal/daemon/daemon.go:700)
                 │  建 per-request ctx(cancel 挂在 stopCtx 下,daemon.go:745-746)
                 ▼
             shell.Session.RunArgs(internal/shell/shell.go:1086)   ← 同一个 Session 跨请求复用
                 │  解析脚本为 *syntax.File
                 ▼
             runner.Run(ctx, prog)(shell.go:1129)                  ← mvdan interp
                 │  跑完整个 File 后:
                 │  api.go:1047-1049  → 触发 EXIT trap   ← 分叉①
                 ▼
             Run 返回 → defer cancel()(daemon.go:746)
                        → 后台 goroutine 连带被砍    ← 分叉③
```

- **分叉①**:mvdan 对 `*syntax.File` 的 Run 结束一律视为「shell 退出」,于是常驻会话里**每条请求收尾都触发 EXIT trap**;真交互 bash 只在进程真正退出时触发。
- **分叉②**:命令定位与子进程 PATH 用的是会话**构造期**固化的 `searchPath`(shell.go:129),请求里 `export PATH` 改的只是 interp 的变量,两者不接。
- **分叉③**:请求的 ctx 是后台 job 的 ctx 来源(mvdan runner.go:374-380),Run 一返回 cancel 就把它连同「会话还活着」的直觉一起砍掉。

三个选项表的推荐都必须过同一杆秤(AGENTS.md 项目底线):**不静默做错事、语义可控、文档与实现同步**;以及 AI 使用场景 —— 单请求脚本化调用是绝对主流,「会话跨命令常驻」是对外承诺(ipc.go:129-131)。

---

## 1. 分叉①:EXIT trap 每条请求收尾都触发

### 1.1 现状与证据

**机制。** `trap '动作' EXIT` 把动作存进 runner 的 `callbackExit`(mvdan `interp/builtin.go:1021-1022`),它挂在**常驻** runner 上,跨请求持久。mvdan 对每次 `Run` 的收尾逻辑(mvdan `interp/api.go:1047-1049`):

```go
// Running an entire file implies an exit; ...
if _, ok := node.(*syntax.File); ok || r.exit.exiting {
    r.trapCallback(ctx, r.callbackExit, "exit")
}
```

winbash 的每条请求都是 `*syntax.File`(shell.go:1129 传给 `runner.Run`),所以**每次请求收尾都触发一次**。这是 mvdan 的通用语义(它无法区分「脚本跑完了」和「常驻 shell 收到一段新输入」),winbash 直接继承了它。

**实测**(本机,2026-10-31,独立 `--endpoint` 探针,同一 daemon):

```
winbash -c 'trap "echo TRAPPED" EXIT; echo armed'   →  armed \n TRAPPED   (rc=0)
winbash -c 'echo Y'                                 →  Y \n TRAPPED       (rc=0)  ← 每条都触发
winbash -c 'echo Z'                                 →  Z \n TRAPPED       (rc=0)
winbash -c 'trap - EXIT; echo disarmed'             →  disarmed           (rc=0)
winbash -c 'echo W'                                 →  W                  (rc=0)  ← 解除后干净
```

**测试钉子。** `internal/daemon/fresh_test.go:114-137` `TestFreshDoesNotInheritTrap`:对照组断言「长驻会话的 EXIT trap 在后续命令结束时依然触发」(注释原话:「trap 跨命令持久(既有设计)」),关键断言是 `--fresh` 不继承、不触发它。也就是说:**触发时机本身是既有设计且有测试钉住;差异表里只有「`trap <信号>` 只支持 ERR/EXIT」一行(已知差异与限制.md:60),触发时机没有专行** —— 见 §1.4 的补账项。

### 1.2 与交互 bash 的分叉到底伤在哪

| 用法 | 真 bash(交互) | 真 bash(`bash -c` / 脚本文件) | winbash 现状 |
|---|---|---|---|
| 单请求内 `trap '清理' EXIT; 工作…` | 不适用 | 请求结束时触发一次 | **相同**(对) |
| 上一条请求装的 trap,后续每条请求 | 不触发(等真退出) | 不适用 | **每条都触发**(分叉) |
| 会话结束时(对应「进程退出」) | 触发一次 | — | 不触发(分叉) |

危害分两档:

1. **噪音档**(常见):装的 trap 是 `echo`/`log` 类,后续每条请求多几行输出。AI 逐条读输出,可见、可理解,烦人但不致错。
2. **副作用档**(低频但真):装的 trap 是清理类(如 `trap 'rm -rf "$TMP"' EXIT` 跨请求的工作流)—— 后续**每条请求收尾都重跑一次清理**。§1.1 探针里 trap 持久到 `echo W` 之后仍在触发;如果第 2 条请求在 `$TMP` 里写了东西,它会在第 2 条收尾被第 1 条请求装的 trap 删掉。这与「会话跨命令持久」的承诺直接冲突:**持久的是变量,被清理的却是变量指向的文件**。

### 1.3 选项表

| 选项 | 语义 | 实现成本 | 风险 |
|---|---|---|---|
| **A. 维持现状 + 补文档** | 模型自洽:「每个请求是一段脚本,脚本结束即退出」——EXIT trap 跟着脚本走。单请求清理类用法(AI 主流写法)语义正确 | 零代码;差异表 + README 各补一段(见 §1.4) | 保留 §1.2 的副作用档 footgun;跨请求工作流里用户拿到的会话状态(变量)与文件状态不一致 |
| **B. 向交互语义看齐:收尾临时解除 EXIT trap、Run 后恢复,另在「会话真正退出」时触发** | trap 只在「装它的那次请求结束」或「会话销毁」时触发 | **中高**:① mvdan 补丁(winbash 拥有 third_party 树,先例:fd 表、`read -d`、`mapfile -n`,见 third_party/mvdan-sh/WINBASH-PATCH.md):Run 开头记 `callbackExit` 是否**本次新装**,收尾只在新装时触发;暴露一个手动触发口子。② 「会话真正退出」在 winbash 里对应 **`Session.Close()`** 的三个调用点:`--stop`/空闲退出的收尾(closeSessions,daemon.go:360-372,`e.sess.Close()` 在 :368)、fresh 会话的每请求 defer(daemon.go:763-766)、daemon 进程兜底退出(那一路**刻意不 Close**,daemon.go:355-359 的注释:宁可不关也不在关停路径上冒 panic/竞争风险 —— 这一路 trap 就不会触发,需要在文档里如实说)。③ Close 时触发的输出**没有去处**(没有请求通道,只能进 daemon stderr —— 使用者看不见)。④ fresh 会话要防双触发(请求收尾已触发过一次,Close 不能再来一次,需要「已触发」标记) | Close 时触发不可观察(结果静默);`--stop` 延迟被 trap 脚本拉长(受宽限预算约束,runShutdown daemon.go:288-296 到点强杀);mvdan 补丁升级时要跟着维护;fresh 双触发边界 |
| **C. 响亮拒绝/警告持久 trap** | 装 trap 时若会话是长驻的,回一条 note「EXIT trap 每条请求收尾都会触发」 | 低(callHandler 拦 `trap`,类似 trapGuard 的形状,shell.go:498) | 又一处特判;note 会进每次响应的 errors,对熟悉现状的人是噪音 |

**B 的关键难点不是解除/恢复本身,而是「会话真正退出」的三个调用点语义不齐**:正常关停会 Close(能触发)、进程兜底退出不 Close(触发不了 —— closeSessions 上方注释写明「等不到就不关」的取舍,daemon.go:356-359)、fresh 每请求都 Close(必须防重)。三个点行为不一致的话,「向交互看齐」就只是换了一个更复杂的分叉。

### 1.4 推荐:**A(维持现状)+ 补账**,B 记为「被咬到再立项」

理由:

1. **AI 主流用法恰好得到正确语义。** `trap '清理' EXIT; 工作…` 写在一条请求里,请求结束触发一次 —— 这正是脚本语义,AI 要的就是它。改成 B 反而砍掉这个保证(清理要等到会话销毁,而长驻会话可能几小时不销毁,清理形同虚设)。
2. **B 的收益(消灭副作用档 footgun)配不上它的成本与新的静默面。** Close 时触发的结果使用者看不见 —— 用一个可见的噪音换一个不可见的触发路径,与「不静默做错事」的取舍方向相反。
3. **补账是必须的**(文档与实现同步的欠账,不是可选项):
   - `已知差异与限制.md` 新增一行:「EXIT trap 的触发时机 = 每条请求收尾(长驻会话里 trap 跨命令持久,每条命令结束都会重触发一次;真交互 bash 只在进程退出时触发)。清理类 trap 的后果:**后续每条请求收尾都会重跑清理**(含误删后续请求写入的文件);要只清理一次,把 trap 和工作写在同一条请求里,或用完 `trap - EXIT` 解除。回归:`TestFreshDoesNotInheritTrap` 对照组」。
   - README「会话级状态与当前限制」段补一句,同上。
4. **顺手加一条显式钉子**(可选,半小时):daemon 包补 `TestExitTrapFiresOnEveryCommand`,把 §1.1 探针的四行(装→触发→解除→干净)钉成回归 —— 现有测试只钉了「跨命令持久」(fresh 对照组),没把「每条都触发」单独钉住;将来真改 B 时,先反转这条。

---

> **决议(2026-10-31):维持现状(A),本节其余方案记为「被咬到再立项」。** 用户拍板:EXIT trap 每条命令收尾触发不是缺陷,是 `bash -c` 的语义,而且是唯一自洽的那种 —— winbash 的每次请求本来就是一次 Run/一个脚本生命周期,「退出」对应的就是这条命令的收尾。差异表对应行转为决议记录;钉现状的 `TestExitTrapFiresOnEveryPersistentCommand` 判据继续有效。

## 2. 分叉②:会话内 `export PATH` 对定位与子进程静默无效

### 2.1 现状与证据

**机制。** 两条线各固化一次:

1. **定位线**:Session 构造期 `searchPath: BuildSearchPath(os.Getenv("PATH"))`(shell.go:129;构造逻辑在 pathsearch.go:33-60:Git Bash `usr\bin` 前置 + 去 + 真实 PATH)。此后所有 ③ 层定位都走它:`classify(args[0], s.searchPath)`(shell.go:230)、`runExternal` 里的 `builtins.Which`(shell.go:672)、① 层 `which` 内建(shell.go:394)。请求里 `export PATH` 改的是 interp 的 PATH **变量**,这条线完全不看。
2. **子进程环境线**:`envList`(shell.go:825)把 interp 环境展开成 `cmd.Env`,但对 PATH **无条件替换**成 searchPath(shell.go:834-836;没导出 PATH 时也补一条,shell.go:848-850)。这是**刻意取舍**,注释(shell.go:809-824)给出实测论证:`xargs -n1 echo` → `usr\bin/xargs: echo: No such file or directory`、`ls | xargs -n1 echo | wc -l` → **0(静默)**,因为子进程若拿进程 PATH,定位用一条路径、执行用另一条,Git Bash 的工具链在子进程里断链。② 层的环境同步复用同一个 `envList`(shell.go:250),所以 `$env:PATH` 同样拿不到会话的 export。

**实测**(本机,2026-10-31,独立 `--endpoint` 探针,同一 daemon):

```
winbash -c 'export PATH=/nonexistent-aabbcc:$PATH; echo $PATH'
    → IN-SHELL-PATH=/nonexistent-aabbcc:...        ← 变量本身更新了
winbash -c 'export PATH=/nonexistent-aabbcc:$PATH; ls …'   → 仍能跑(rc=0)   ← 定位不理它
winbash -c 'export PATH=/nonexistent-aabbcc:$PATH; env | grep ^PATH'
    → PATH=/usr/bin:/c/Users/…                     ← 子进程拿到 searchPath(MSYS 形式显示),不是 /nonexistent
winbash -c 'export PATH=/nonexistent-aabbcc:$PATH; pwsh -NoProfile -c '"'"'$env:PATH'"'"''
    → C:\Program Files\Git\usr\bin;C:\Users\…      ← ② 层同样是 searchPath
winbash -c 'PATH=/marker-zzz:$PATH sh -c "echo $PATH"'     ← **逐命令前缀也一样无效**
    → /usr/bin:/c/Users/…                          ← 赋值前缀同样被 envList 丢弃
winbash -c 'env PATH=/nonexistent-aabbcc no-such-xyz'      ← 唯一漏网:env.exe 在子进程**内部**改 PATH
    → /usr/bin/env: 'no-such-xyz': No such file or directory (rc=127)  ← 这条是真的生效了
```

**127 的报错还误导**(shell.go:673-684,hint 文案在 :676):`确认命令名有没有写错;③ 层会搜索 Git Bash 的 usr\bin 与真实 PATH,可用 winbash -c 'which …' 自查` —— 用户刚 export 过 PATH,「写错」恰恰是最不可能的原因,真话(「定位与子进程都不吃 export PATH」)一句没说。

**测试覆盖。** `internal/shell/corpus_test.go:189-190` 钉住 `echo a b | xargs -n1 echo` 与 `ls | xargs -n1 echo | wc -l` —— 那是 envList 取舍**受益面**的钉子;「export PATH 无效」这个**代价面**没有专行测试,差异表里也**没有行**(全库 docs + README + 差异表 grep「export PATH」零命中)。README:243-261 只描述了 searchPath 的构成与「子进程拿到同一条 PATH」,没说 export 不生效。

### 2.2 选项表(逐项对着 xargs 取舍谈)

| 选项 | 语义 | 与 xargs 取舍的冲突 | 实现成本 | 风险 |
|---|---|---|---|---|
| **A. 维持 + 补账**(现状) | 「PATH 是系统级事实,会话不改它」;要改用 `env PATH=… cmd`(实测可用)或改 daemon 启动时的 PATH | 无 —— 正是这个取舍的受益面被 corpus 钉着 | 差异表补一行 + README ③ 层段补一句(见 §2.4) | 保留 127 误导文案(可单独小修,见 D) |
| **B. envList 透传会话 PATH** | 子进程拿到 interp 变量里的 PATH | **正面撞车**:从 cmd/PowerShell 启动 daemon 时(非 Git Bash 会话),进程 PATH 没有 `usr\bin`,透传后 `xargs -n1 echo` 的静默 0 立刻回归(corpus_test.go:190 红)。若透传时又硬塞 `usr\bin`,则 `export PATH=/ restrictive:$PATH` 的**收紧意图**被静默放宽 —— 换一个方向静默做错事 | 低 | 两个方向都是静默语义破坏 |
| **C. classify/locate 追加 hc.Env 的 PATH 目录(保持 usr\bin 前置)** | 「能加不能抢」:会话 PATH 里新增的目录追加进搜索,usr\bin 恒在最前 | 部分成立:`export PATH=/x:$PATH` 后 /x 可被定位、xargs 也不坏(usr\bin 仍在)。但 ⓵ 收紧意图(`export PATH=/x` 不带 `$PATH`)在定位侧仍被无视(照搜全表);⓶ 用户**前置**意图失效(/x 追加在尾部,抢不过 usr\bin 与真实 PATH 里的同名工具);⓶ 定位吃了 /x 而子进程 PATH 若不同步(B 不做的话)又造出「定位一条路、执行另一条路」的新分裂 —— 正是 envList 注释反对的形状(shell.go:815-817) | 中:每请求从 hc.Env 算「新增目录」并合并,与 feat/locate-cache 的 locate 单入口对齐(见 D2);合并顺序、去重、相对路径都要定规则 | 半吊子语义需要长文档解释;负缓存键仍兼容(键=完整 searchPath 串,内容变了键就变,无陈旧命中) |
| **D. 维持语义,修「误导面」**(A 的加强版,拆成两件小事) | D1:④ 层报错时若会话 PATH 变量 ≠ searchPath 的源(hc.Env 里的 PATH 与构造期不同),加一句真话:「会话里改过 PATH:命令定位与子进程环境不受 export/前缀影响(设计见差异表),可用 `env PATH=… <命令>` 临时改」。D2:差异表行里点名 feat/locate-cache 的负缓存键设计(键 = 完整 searchPath 串,locate.go 的 locateMisses 注释)——**键天然按内容隔离,未来若真做 C 的动态 PATH,缓存不用改** | 无 | D1 低(④ 层报错处一次字符串比较 + 一行文案 + 单测);D2 纯文档 | D1 只是文案,不改语义;误报面 = 真的拼错命令的人多看一句,可接受 |

### 2.3 推荐:**D(= A + 修误导面)**

理由:

1. **xargs 取舍是承重墙。** envList 的 PATH 替换撑着「Git Bash 工具链在子进程里可用」这个核心承诺(README:261),corpus 两行钉着它。任何让 export 生效的方案要么拆墙(B),要么引入新的半吊子规则与定位/执行分裂(C)。AI 场景里「会话中途切工具链」的真实需求,现有的两扇门都能走:`env PATH=… cmd`(实测可用)或换 `--session`。
2. **当前最伤的不是「不生效」,是「报错误导」。** 127 的 remedy 把人引向「查拼写」,而真话一个字没有 —— 这与本项目修过的每一类「静默缺口」同型(不静默做错事,以及不静默地不解释)。D1 用最小代价把这句话补上。
3. **C 留档不立项。** 若未来真实需求出现(用户想在会话里临时挂一个工具目录),正确的下一步是先做 D2 说清楚的 locate 单入口合并,并把「收紧意图被放宽」这一条作为已知差异写死 —— 那时再说,现在不做。

### 2.4 落地清单(若采纳 D)

1. `已知差异与限制.md` 新增一行:「会话里 `export PATH` / 逐命令 `PATH=…` 前缀:命令定位(③ 层)与子进程/② 层拿到的 PATH 都不受影响 —— 定位与子进程环境用的是会话构造期固化的 searchPath(usr\bin 前置 + 真实 PATH,shell.go:129/envList),这是 xargs 等『子进程再 exec』能工作的前提(envList 注释的实测)。临时改单条命令的环境用 `env PATH=… <命令>`;要整套换环境,启动 daemon 前改 PATH 或换 `--session`。回归:probe 断言(见下)」。
2. README ③ 层段(243-261 一带)补一句同义说明 + `env` 前缀的逃生口。
3. shell.go ④ 层报错(shell.go:674-679)加 D1 文案;`internal/shell` 补单测:`export PATH=…` 后 (a) `ls` 仍命中、(b) `env` 子进程 PATH 首项仍是 usr\bin、(c) ④ 层报错含新文案;(d) 对照:未改 PATH 时报错不含新文案。**无需 e2e**(行为全在单请求内)。

> **决议(2026-10-31):重派生 searchPath 方案落地,本节其余方案作废。** 用户拍板不走 D(补账 + 改文案),直接修:每条外部命令进 execHandler 时对一次 interp 变量里的 PATH(实时源是 `hc.Env`,`runner.Vars` 在 Run 中途是旧快照),变了就重走 `BuildSearchPath` —— usr\bin 恒前置,xargs 取舍不破。上文 §2.4 按 D 写的落地清单未执行。实现:`internal/shell/shell.go` 的 `syncSearchPath`(原子快照,管道两段/后台命令并发安全);回归:`internal/shell/sessionpath_test.go` + `cmd/winbash/e2e_test.go` 的 `TestSessionPATHExportTakesEffectAcrossProcesses`;已知差异表对应行已改为「已修」。

---

## 3. 分叉③:后台 job(`cmd &`)随请求结束被取消

### 3.1 现状与证据

**测试钉子。** `cmd/winbash/e2e_test.go:1063-1087` `TestBackgroundJobDiesWithTheRequest`(本次实跑:PASS,7.4s,`go test ./cmd/winbash -run TestBackgroundJobDiesWithTheRequest -count=1`):
- a. `( sleep 2; echo BG > marker ) & echo FIRST-DONE` 立即返回 rc=0;
- b. 给足 2 倍余量后 marker 不存在 —— 副作用连同输出**静默消失**;
- c. 对照组 `( sleep 0.3; echo LATE ) & echo NOW; sleep 2` 正常 —— 取消发生在「请求结束」,不是后台 job 在 winbash 里不工作。

**差异表已记账**:已知差异与限制.md:77(`cmd & 后台 job 随请求结束被取消`,2026-10-31,含根因与「改语义先反转 a/b、c 保留」的预案)。

**根因与机制。** `runJob` 每请求建 `ctx, cancel := context.WithCancel(d.stopCtx)` + `defer cancel()`(daemon.go:745-746);mvdan 的后台分支(runner.go:364-380)把后台 subshell 交给 `r2.Run(ctx, &st2)` —— **用的就是这个请求 ctx**。Run 一返回 cancel 触发,`r.stop(ctx)`(runner.go:344-354)让后台 goroutine 下一拍 fatal 退出;外部子进程由 `exec.CommandContext`(shell.go:760)连带杀掉。e2e 注释里那句判断成立:与 `bash -c` 的孤儿进程语义相反,而 winbash 承诺会话跨调用常驻,两者放一起就是静默数据丢失 —— AI 的 `nohup … &` 写法必然踩中,且 rc=0、无任何提示。

**「放活」要过的三道坎(都已在代码里核实):**

1. **输出去向**:后台 subshell 继承 `r.stdout`(api.go:1108-1109,subshell 拷贝)—— 那是 winbash 的 `switchWriter`,Run 收尾 `defer` 把它 `Set(nil)`(shell.go:1098-1103),之后 `switchWriter.Write` 对 nil **静默吞掉**(shell.go:97-104);若 job 活到下一条请求,`outBuf.Reset()` + `Set(&outBuf)`(shell.go:1094-1097)会把它的输出**混进下一条请求的 stdout**(归因错乱)。所以放活后「未重定向的后台输出」必须显式定义:丢(响亮告知)或逐 job 落文件,二选一,不能维持继承。
2. **生命周期绑谁**:绑**会话**(session 才是「shell 进程」的对应物;绑 daemon 会把不同会话的 job 混在一起,`--session` 并行化之后更说不清)。落点:`Session` 挂一个会话级 job ctx(父是 stopCtx,不是请求 ctx)+ job 表;`Close()` 与 `--stop` 的 closeSessions(daemon.go:360-372)收割 —— 收割顺序在关停宽限预算内做,超了按现有口径「随进程回收 + stderr 一句」(daemon.go:370-372)。mvdan 侧需要一个补丁:后台分支不用请求 ctx 而用 `bgCtx`(Runner 加字段或 interp 选项,~10 行;winbash 拥有补丁树,有完整先例与升级流程 WINBASH-PATCH.md)。
3. **跨请求的 job 语义**:mvdan 的 `bgProcs` 挂在常驻 runner 上,只在 `Reset()` 时清空(api.go:941-943),长驻会话里**跨请求累积**;`wait`/`$!`(builtin.go:375-400、vars.go:171)按 `len(bgProcs)` 索引 —— 放活后跨请求 `wait 1` 会索引到陈旧条目,job 表与 PID 编号要重新定义(至少:每请求重排,或跨请求编号 + `jobs`/`wait` 的语义文档化)。

### 3.2 选项表

| 选项 | 语义 | 实现成本 | 风险 |
|---|---|---|---|
| **A. 维持 + 文档**(现状) | 「请求结束 = 这段脚本的进程组被杀」,与 `bash -c` 的差异写死在差异表;跨请求后台请调用方自己解决(独立进程/独立 endpoint) | 0(已记账,已钉) | 静默数据丢失留在原地;AI(最高频用户)的 `nohup … &` 写法必然踩 |
| **B. 放活(孤儿存活)** | 后台 job 活过请求:绑会话、未重定向输出丢弃并在**本次响应 errors 里响亮 note**、`--stop`/`Close` 收割、跨请求 `wait`/`$!` 重新定义 | mvdan 补丁(bgCtx,~10 行)+ Session job 表与会话 ctx(~100-150 行)+ runJob/Close/收割接线(~50 行)+ 响应 note + e2e 反转 a/b 与新增用例 + 文档。**合计 2–3 个工作日当量**(含测试与文档;纯编码 1 天,余量给生命周期边界) | runaway job 占资源直到会话结束(请求超时不再保护它);16 个长驻会话各挂 job 的累积;与 fresh 语义的边界(fresh 会话 = 一次性,job 仍随请求死,保持不变);每次修 mvdan 升级要跟着维护 |
| **C. 响亮拒绝 `&`** | 遇 `&` exit 2 + 指路 | 低 | **破坏单请求内合法用法** —— 对照组 c(`( sleep 0.3; … ) & echo NOW; sleep 2`)已证可用且是真实模式;拒绝它等于把「一条请求内并行」也禁了 |

### 3.3 推荐:**目标 B(放活),分批实施;在 B 落地前维持 A 的账**

理由:

1. **三个分叉里只有 ③ 是真静默数据丢失。** ① 是可见噪音 + 可文档化的 footgun,② 是误导文案;③ 是 rc=0、输出无、副作用无、后续请求也干净 —— 项目历史上所有补丁(mvdan 的 `read -d`/`mapfile -n`、fd 表)修的都是这一档。按项目自己的价值排序,B 是三个里唯一「必须修」的。
2. **但 B 是协议级改动,不该与 ①② 的文档批混在一起。** 它动会话生命周期、`--stop` 收割与跨请求 job 编号,需要自己的 e2e 序列(「同一 daemon 先 A 后 B」的形态 —— 正是 AGENTS.md §4.3 教训的那类)。
3. **A→B 的账已经立好**(差异表:77 写明了反转预案),B 的目标语义如 §3.2 所列三道坎的答案:绑会话、输出响亮丢弃、Close/`--stop` 收割。
4. **C 否决**:对照用法真实存在,拒绝它得不偿失。

**决议(2026-10-31):--detach + & 警告,本节其余方案作废。** 用户拍板不走 B(放活),改走双轨:`winbash --detach -c '…'` 新增显式脱离入口(拉脱离子进程 + 日志文件 + 回 pid,跨请求后台任务的正式答案);`cmd &` 行为维持现状(本节 a/b/c 的钉子原样有效),但脚本含后台任务时 shell 层追加一行指路 `--detach` 的警告(检测并进 `rejectUnsupportedSyntax` 的单遍 Walk,`cmd &` 是 `*syntax.Stmt` 的 `Background` 字段 —— 探针实测,不是想象中的 `BinaryCmd`;整个脚本至多一行、不改退出码)。§3.2 的 B 选项(放活)与 §3.3 的推荐随之作废;a/b 若哪天要反转,判据不变 —— 先改差异表再改测试。落地回归:`internal/shell/backgroundwarn_test.go`、e2e `TestBackgroundJobDiesWithTheRequest`(警告恰一次)与 `TestDetachSpawnsIndependentProcess`。

---

## 4. 若按推荐落地:分批实施顺序

前置说明:批 1、2 是**零行为变化**的文档/文案批,不需要 e2e 钉子;批 3 是唯一的语义改动批。每批一提交 = 一任务,合并前主代理跑全量门禁(AGENTS.md §4,本文档不越权代跑)。

**批 1(半天,纯文档):①② 补账**
1. ① 差异表新增「EXIT trap 触发时机」行(§1.4 文案),README 会话段补一句。
2. ② 差异表新增「export PATH 无效」行(§2.4 文案),README ③ 层段补说明 + `env PATH=…` 逃生口。
3. 可选钉子(顺手做):`TestExitTrapFiresOnEveryCommand`(daemon 包,钉 §1.1 四行序列);② 的「仍命中/子进程 PATH/④ 文案」单测(shell 包)。
4. **不需要 e2e 钉子**:两批都是把现状写下来,现有钉子(fresh_test 对照组、corpus xargs 两行)已覆盖行为面。
5. 验收:git status 只有 md 与测试文件;shell/daemon 包单测绿;全量门禁主代理跑。

**批 2(半天,小改):② 的 D1 误导面修复**(可与批 1 合并,也可单独回滚)
1. shell.go ④ 层报错块(shell.go:673-684)加「会话里改过 PATH」的真话句;附单测(§2.4 第 3 条)。
2. 行为变化仅限报错文案,差异表行(批 1 已写)不用改;**无需 e2e**。

**批 3(2–3 个工作日,语义改动):③ 放活** —— 最后做,因为它最大且独立
1. **先补 e2e 钉子序列**:按差异表:77 与 e2e 注释的预案,新增「放活后」的断言用例(a/b 反转、c 保留),加新场景:跨请求输出重定向到文件的 job 存活、未重定向时响应含 note、`--stop` 收割、fresh 会话 job 仍随请求死。**实施前这些用例红是预期** —— 先立后修,防实现漂移。
2. mvdan 补丁:bgCtx(后台分支改 ctx 来源;WINBASH-PATCH.md 增补一节)。
3. Session 侧:会话 job ctx + job 表 + Close/`--stop` 收割 + runJob 响应 note。
4. 跨请求 `wait`/`$!` 语义定义(每请求重排或跨请求编号,写入差异表行)。
5. 反转 `TestBackgroundJobDiesWithTheRequest` 的 a/b 判据(c 保留),差异表:77 行改为「已支持」并补新边界。
6. 端到端必须**主代理亲手**跑「同一 daemon 先 A 后 B」序列 + 全量门禁 ≥2 次(AGENTS.md §4.2/4.3)。

**顺序为什么这样排**:批 1 把两份缺失的账补上,用户与 AI 从此可预判 ①②;批 2 消掉 ② 唯一的实际伤害(误导);批 3 独立立项,失败可整体回滚而不拖累前两批。①② 的「维持」与 ③ 的「放活」互不依赖 —— 若用户否决 ③,批 1/2 照常成立。

---

## 附:本文实测记录

- 探针二进制:worktree 内 `go build -o winbash.exe ./cmd/winbash`(2026-10-31)。
- 所有探针用独立 `--endpoint "$TEMP/wb-semprobe-$$.port"`,用毕 `--stop` 并删端口文件;未触碰默认端点。
- 探针输出原文见 §1.1、§2.1 各代码块。
- ③ 未重复手测(按 e2e 钉子为准):`go test ./cmd/winbash -run TestBackgroundJobDiesWithTheRequest -count=1` → PASS(7.4s)。
- feat/locate-cache(84d75d8)截至本文**未合并** main(`git log main..feat/locate-cache` 仅此一笔);其负缓存键设计引用自该提交的 `winbash/internal/shell/locate.go`。
