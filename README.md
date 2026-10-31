# winbash

[![CI](https://github.com/jf200s/winbash/actions/workflows/ci.yml/badge.svg)](https://github.com/jf200s/winbash/actions/workflows/ci.yml)

bash 语法 + pwsh 能力的常驻 shell，给在 Windows 上干活的 AI 用。

AI 熟悉 bash，但 bash 管不了 Windows；pwsh 管得了，却不熟、而且每次冷启约 480 ms。
winbash 让 `ls | grep go` 照常跑，需要 Windows 能力时（服务、注册表、进程、系统信息）
不用换语法。**连 `pwsh` 本身也被接管**，所以顺手写的 `pwsh -Command '...'` 同样不再每次
付 ~350 ms 的启动成本。

```bash
winbash -c 'ls | grep go'          # bash 语义，但 ls/grep 是我们自己的实现
winbash -c 'svc list --running'    # Windows 服务，不用切到 pwsh 语法
winbash --json -c 'sys mem'        # 类型化 JSON 输出
```

---

## 这条仓库怎么读

**上手手册是 [`winbash/README.md`](winbash/README.md)**（525 行）—— 命令行参数、四层路由、
安全边界、会话级状态、性能口径。与真 bash 的差异另立档案
**[`winbash/已知差异与限制.md`](winbash/已知差异与限制.md)**（74 行，每条差异都注明回归测试在哪）。
**先读手册，查差异去档案。**

| 路径 | 是什么 |
|---|---|
| [`winbash/`](winbash/) | 全部源码（Go，11 个包） |
| [`winbash/README.md`](winbash/README.md) | **主文档（上手手册）**：怎么调、四层怎么分 |
| [`winbash/已知差异与限制.md`](winbash/已知差异与限制.md) | **差异档案**：和真 bash 差在哪、每条差异的回归测试在哪 |
| [`docs/superpowers/`](docs/superpowers/) | 设计文档与实施计划 |
| [`docs/winbash-*.md`](docs/) | 三轮独立复审（各含我的逐条回应）+ 一份实测评估 + 一份源码审计 |
| [`AGENTS.md`](AGENTS.md) | 本仓库的协作规范：子代理并行 + worktree 隔离 + 验收纪律 |

## 快速开始

```bash
cd winbash
export GOPROXY=https://goproxy.cn,direct          # 必须:proxy.golang.org 在开发机上不通
go build -o winbash.exe ./cmd/winbash

./winbash.exe -c 'ls | grep go'      # 第一次调用会自动拉起常驻 daemon
./winbash.exe --status               # pid / cwd / 地址 / 引擎状态 / 会话池明细
./winbash.exe --stop                 # 停掉 daemon（不会为了停它而先把它拉起来）
```

想把 `winbash` 当命令名用，把 `winbash\` 目录加进用户 `PATH` 即可 —— 没有安装器、
没有服务、没有注册表项；卸载就是从 `PATH` 里删掉那一条。

## 它凭什么值得存在

**性能不是重点，语义可控才是。** ① 层在引擎已热的口径下比真 bash 快（2026-10-31 实测
20 次 spawn→exit：winbash **16.6 ms/次** vs 真 `bash -c` **37.4 ms/次**，约 2.2×；
单次冷调要拉起 daemon，72 ms，比 bash 一次 spawn 慢 —— 批量场景才占优）。省时间的地方在 ② 层 ——
吃掉 pwsh 的冷启动：引擎已热时连跑 5 次 **20.6×**（2038 ms → 99 ms），
逐次摊的评估口径是 3.7×。对外说法用「重复调用 ② 层比每次冷启 pwsh 快一个数量级」。

> 以上数字连同**测量口径**记在主文档的「性能怎么量」一节 —— 那个口径很要紧：
> 用 PowerShell 直调进程才测得出信号，在 Git Bash 里手测会被进程启动开销整个淹掉。
> 引用前先看口径，别混着比。

**真正的价值**是 `ls` 永远是自己的 `ls`，不会被 `C:\Windows\System32` 里同名的
Windows 程序抢走；以及每条命令省掉一次语法切换。

**三条底线**（贯穿整个实现）：

1. **不静默做错事** —— 不认识的 flag / 参数 / 子命令一律非零退出。上游在
   `grep --include`、`ls -R`、进程替换 `<(...)`、`trap INT`、`printf %q` 这些地方
   会**静默给出错值**，winbash 一律响亮拒绝并指路。
2. **值保证** —— `0xa` → `10`、`STATE: 1` → `Stopped`、裸字节大小、输出无 ANSI 色码。
   退出码 `0` 成功 / `1` 一般错误 / `2` 用法错误 / `126` 不可执行 / `127` 命令未找到。
3. **文档与实现不许对不上** —— 改了行为就改「已知差异与限制」档案
   （[`winbash/已知差异与限制.md`](winbash/已知差异与限制.md)）。那份档案是这份
   代码库的主要资产之一。

## 四层路由（一句话版）

每条命令判层，顺序固定：

| 层 | 内容 |
|---|---|
| ① 纯 Go 内建 | `ls` / `cat` / `grep` / `which`，bash 文本语义（`grep` 默认是 **BRE**，不是字面匹配） |
| ② Go 内建 + 常驻 pwsh | `svc` / `reg` / `ps` / `sys` / `pwsh` |
| ③ 真实工具 | Git Bash 的 `usr\bin` **排在 PATH 最前**，所以 `sed`/`awk`/`find` 拿到的是 Unix 语义 |
| ④ 兜底 | 三行式错误（问题 + 怎么办）+ `127`，措辞按命令对齐真 GNU |

细节（尤其是 ② 层怎么把 `cwd` 与环境**走协议字段**而不是拼脚本文本前导，
以及 MSYS 程序的参数为什么要用混合引号传）见主文档。

## 状态

| 项 | 值 |
|---|---|
| 生产代码 / 测试代码 | 8,734 行 / 13,797 行（测试比生产多，比例 1.58） |
| 包 | 11 个 |
| 唯一直接依赖 | `mvdan.cc/sh/v3 v3.14.1` —— **本地打过补丁**，见 [`winbash/third_party/mvdan-sh/WINBASH-PATCH.md`](winbash/third_party/mvdan-sh/WINBASH-PATCH.md) |
| 门禁 | `go test ./... -count=1` 全绿 + `smoke-gitbash.sh`（必须从真 Git Bash 跑） |

升级 `mvdan/sh` 时不能只改 `go.mod`：把新版本源码覆盖进 `third_party/mvdan-sh`，
再按 `WINBASH-PATCH.md` 重打补丁 —— 那几处补的都是上游**静默错值**（`read -d`
报 invalid option 却退出 0、`mapfile -n` 同理），不是可选优化。

## 已知不做

不做交互式 REPL，不追求 100% bash 兼容，不做 pwsh 对象管道。
管道每段是 goroutine 而非真子 shell（上游架构限制，后果与应对见
[已知差异与限制.md](winbash/已知差异与限制.md)）。
