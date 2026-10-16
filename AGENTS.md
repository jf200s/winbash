# 工作规范：子代理并行 + 分支/worktree 隔离

**判据一条**：≥2 个**无共享状态**的任务，就不要在一条线上串着做 —— 派子代理、开 worktree、各提各的分支。主代理只负责**拆、派、验收、合并**，不亲自通读大文件、不亲自跑长链路。

## 1. 什么时候必须派子代理

- **只读类**（摸结构、查历史、审计、复审、比对文档与实现、跑测试给结论）→ **一律**走子代理（`explore` / `research` / `review` / `security-review`）。主代理只看结论。
- **≥2 个独立子任务** → **同一响应里并发派发**（多条派发在一条消息里 = 并行；一条一条发 = 串行，那是要避免的形态）。
- **60 秒以上、且不需要立刻阻塞**的子任务 → `run_in_background` 当异步件，回头用 `job_output` 收。
- **不要派**：失败之间有耦合（修一个可能顺带修掉另一个）、还看不出坏在哪（探索性排障）、子代理之间会踩同一份状态（那就先隔离，见 §2）。

## 2. 隔离铁律：写代码的子代理必须有自己的 worktree

```powershell
# 位置固定在项目内（.gitignore 已覆盖 .worktrees/ 与 .workbuddy/）
git worktree add .worktrees/<slug> -b <类型>/<slug>
```

- **分支命名**：`feat/<slug>` · `fix/<slug>` · `docs/<slug>` · `test/<slug>`，slug 用英文短横线（如 `fix-stdin-silent`）。
- **一个子代理 = 一个 worktree + 一个分支**，它只许动 `git worktree add` 时那条分支。
- **只读**子代理**免隔离** —— 不开 worktree、不建分支、不许写。
- worktree 里会各自生成一份 `.reasonix/`（本地运行状态，已忽略）—— 正常，别提交。
- **开 worktree 前 `git status` 必须干净**，基线不干净时开出来的 worktree 会把别人的半成品一起带走。
- 收尾 `git worktree remove .worktrees/<slug>` + `git branch -d <分支>`，不留残骸。

## 3. 子代理的提交与汇报契约（原子、不越权）

子代理**完成即提交**，message 沿用本仓库风格（中文、单主题、可解释动机）：`fix(①/ls): …`、`docs: …`、`test(语料): …`。

子代理**不许**：`merge`、`push`、删别人的分支/ worktree、改 `main`。集成权只在主代理。

子代理返回给主代理的**固定四件**，缺一件就算没做完：

1. 分支名 + `commit` hash
2. 跑过的验证命令与结果（含退出码）
3. 改动了哪些文件、**以及没改但同属该任务的地方**（漏改清单）
4. 未做 / 存疑 / 需要复核的点（没有也要写"无"）

## 4. 主代理的集成流程

1. **review**：逐个 `git diff main..<分支>`，对着上面第 3、4 条核。
2. **全量门禁**：主工作树跑 `go test ./... -count=1`（带 `GOPROXY=https://goproxy.cn,direct`）**和** `smoke-gitbash.sh`（真 Git Bash），都绿才合。
3. **动了跨进程 / 协议 / 状态生命周期的，端到端必须亲手跑** —— 这次有硬反例：stdin 那条线 `go test ./...` **11 包全绿**，端到端第 4 条却炸出 `read |0: file already closed`。原因是前 3 条命令带 stdin、第 4 条不带，而单元测试里没有那个调用序列。**单元测试只证明"被测的函数对"，证不到"两次调用之间 Session 是干净的"。** 端到端要专门构造「同一 daemon 上先 A 后 B」的序列，不是跑通一条就完。
4. **合并**：`git merge --no-ff <分支>`，一提交 = 一任务。冲突**退回子代理在它的 worktree 里重解**，不在主代理手里硬解。
5. 删 worktree 与分支（§2 收尾命令），确认 `git status` 空、`git worktree list` 只剩主工作树。
6. 有失败 → 按 `systematic-debugging` 先复现、再修，别猜。

## 5. 并发的物理约束（本项目的硬边界，违反会产假失败）

- **daemon 端点必须隔离**：默认端口文件按用户名共享（`%TEMP%\winbash-<用户名>.port`）。任何**并发**跑的测试、冒烟、手工实测，都要显式带独立 `--endpoint <临时文件>` —— 否则并行线会互停、互抢对方的 daemon。仓库自己的做法见 `winbash/cmd/winbash/e2e_test.go` 的 `benchEndpoint`。
- **`-race` 与 benchmark 串行跑**：抢 CPU 会让计时数字失真（README 里"22 ms / 70 ms"已因此不可信，别再造一批不可信的数）。
- **`smoke-gitbash.sh` 必须从真 Git Bash 跑**（它拿 Git Bash 当对照）：`& 'C:\Program Files\Git\bin\bash.exe' ./smoke-gitbash.sh`。
- **写子代理并发上限 2–3**（同一仓库、同一套测试二进制，CPU 会顶满）；**只读子代理 4–6** 无压力。
- 本机无 `make`、无 `rg`；Go 项目用 `go test` / `go build`；**唯一直接依赖** `mvdan.cc/sh/v3`，`GOPROXY` 必须是 `https://goproxy.cn,direct`。

## 6. 派子代理的环境约束（本机踩实的 8 条，绕法都验证过）

子代理链路与 shell 链路是两套东西，这 8 条每条都当场卡过一次，**别再试第二遍**：

| # | 约束 | 绕法（已验证） |
|---|---|---|
| 1 | `task`/`fleet` 的**默认**子代理模型走 Anthropic 协议会 400（`当前模型不支持该协议`） | 显式传 `model: tokenrhythm/deepseek-flash`（父模型，已知可用）。**用户已把 `subagent_model` 改到「基元」provider**，改后不传 model 也能起 |
| 2 | **带 `profile` 的**子代理（如 `explore`）会绑到另一个坏模型 → 400 | 不传 `profile`；只读改用 `fleet` 的 `read_only: true` |
| 3 | `tool_profile` 是**非法字段**（schema 里没有）→ 400 | 别编字段。合法字段只有 `profile` / `model` / `effort` / `tools` / `write_paths` / `read_only` / `max_steps` |
| 4 | `effort` 只接受 `[disabled low high max]`，`medium` → 400 | 用 `high` 或 `low` |
| 5 | `neohorse-1-9b` **未声明 reasoning levels** → 传任何 `effort` 都 400 | **省略 `effort`**（这是现在 `subagent_model` 指向它时的必填姿势） |
| 6 | **同一时刻只有一个 background `task`**：不带 `write_paths` 就认领整个 workspace，`tools` 只读白名单**不能**解除这个写槽 | 要么显式传 `write_paths` 抢这个槽，要么用 `fleet`（≥2 任务）走 `read_only` |
| 7 | 写锁是**会话级**的，且会匹配**命令文本里出现的路径**（`git -C .worktrees/x …` 被挡）；`job_output` / `read_subagent_result` / 连 `Start-Sleep` 同样被挡 | **没有自解手段**，只能等子代理自己跑完；新轮次通常会重新评估锁 |
| 8 | `fleet` 要求 `tasks` **≥2** | 拆两个只读任务，或改用 `task` |

结论：**派发前先想清楚三件事** —— 传不传 `profile`、传不传 `effort`、写任务有没有 `write_paths`。这三件凑齐，400 与 blocked 基本都能避开。

## 7. 沿用不改的项目底线（新任务同样受约束）

- **不静默做错事**：不认识的 flag/参数/子命令一律非零退出，不得静默忽略；mvdan/sh 那类"看起来成功"的静默缺口必须在 ④ 层拦住。
- **值保证**：`0xa`→`10`、`STATE: 1`→`Running`、内存给裸字节、输出无 ANSI 色码。
- **退出码**：`0` 成功 · `1` 一般错误 · `2` 用法错误 · `126` 不可执行 · `127` 命令未找到。
- **改了行为就要改 README 的「已知差异」表**，文档与实现不许对不上（这条已有先例欠账）。
- **探针/临时文件不进库**：跑完 `git status` 干净，残留物一律清掉。
