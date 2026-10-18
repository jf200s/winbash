# winbash 对 mvdan/sh 的本地补丁

**来源**：`mvdan.cc/sh/v3 v3.14.1`（未经修改的上游源码，复制自 Go module cache）
**为何进库**：`go.mod` 里有一条 `replace mvdan.cc/sh/v3 => ./third_party/mvdan-sh`。
**补丁范围**：四个补丁 —— 两个内建选项（`interp/builtin.go`，外加 `interp/runner.go` 一处调用点）、一处错误分类修正（`interp/runner.go`，见 §3）、一处措辞对齐（`interp/builtin.go` + `interp/runner.go`，见 §4）。共约 85 行。

---

## 为什么必须打补丁（而不是在 winbash 里拦一层）

上游 v3.14.1 缺两个 bash 选项：

| 选项 | 上游行为 | 危害 |
|---|---|---|
| `read -d <delim>` | 报 `read: invalid option "-d"` | **stderr 有话说，但外层退出码是 0、变量留空** |
| `mapfile -n <count>` | 报 `mapfile: invalid option "-n"` | 同上：数组留空、`${#arr[@]}` 是 0 |

那个组合是最坏的失败形态 —— 调用方看到「成功」，拿到空数据。项目底线是「不静默做错事」，所以必须消灭。

**在 winbash 侧拦不住**，这一点实测确认过（不是推测）：

- `interp.CallHandler` 只能读 stdin、改 argv，**不能写 shell 变量**；
- `interp.HandlerCtx(ctx).Env` 看起来能写，但它每次调用都新建一个
  `&overlayEnviron{parent: r.writeEnv}`（`interp/runner.go`），写进去的值 handler 一返回就丢；
- 真正能持久写变量的 `(*Runner).setVar` 是**私有方法**，`Runner` 没有任何公开的写变量 API。

所以只能动上游源码。

## 补了什么

### 1. `read -d <delim>`

`interp/builtin.go` 的 `read` 分支：

- 新增 `-d` 选项解析。bash 只取**首字符**；`-d ""` 表示 NUL（与上游 `mapfile -d` 的处理一致）。
- `readLine` 增加 `delim byte` 参数，终止判断从写死的 `b == '\n'` 改成 `b == delim`。
- `interp/runner.go` 里 `select` 内建的第二处调用补 `'\n'`（它就是「读一行」，语义不变）。

配套语义（与 bash 一致，已验证）：

```bash
IFS=$'\n' read -d "" -ra fd < f.txt     # 读到 EOF，按换行切分成数组，返回 1
read -rd "" x < f.txt                   # 组合开关 -rd 也支持
while IFS= read -r -d '' n; do …; done  # 配 find -print0 遍历 NUL 分隔
```

### 2. `mapfile -n <count>`

`interp/builtin.go` 的 `readarray`/`mapfile` 分支：

- 新增 `-n` 选项解析（非负整数）。
- 读取循环改为 `for maxLines == 0 || len(vr.List) < maxLines`，`0` 表示不限（与 bash 一致）。

这让 pure-bash-bible 的 `head` / `tail` / `lines` 三个函数真正可用：

```bash
head2() { mapfile -tn "$1" line < "$2"; printf '%s\n' "${line[@]}"; }
tail2() { mapfile -tn 0 line < "$2"; printf '%s\n' "${line[@]: -$1}"; }
lines() { mapfile -tn 0 l < "$1"; printf '%s\n' "${#l[@]}"; }
```

### 3. `open` 错误按 `errors.As` 分类（而非类型断言）

`interp/runner.go` 的 `(*Runner).open`。上游原文：

```go
// TODO: support wrapped PathError returned from openHandler.
switch err.(type) {
case nil:           return f, nil
case *os.PathError: if print { r.errf("%v\n", err) }   // 非 fatal
default:            r.exit.fatal(err)                   // fatal:终止整个脚本
}
```

**为什么这是个问题**：类型断言只认**裸的** `*os.PathError`。handler 想自定义错误文案，
就得返回自己的类型 —— 那就掉进 `default`，被当成"handler 的 fatal 错误"，**整个脚本
被终止**。于是只有两条路：要么让 Win32 细节漏给使用者，要么改变控制流语义。

而 Win32 细节确实不能漏。修之前 `echo x > /bad/y` 给的是：

```
open C:\Program Files\Git\bad\y: The system cannot find the path specified.
```

三处都不对：泄漏 Win32 API 名 `open`、把用户写的 `/bad/y` 换成绝对路径、Win32 长句而
不是 POSIX 措辞（真 bash 给 `line 1: /bad/y: No such file or directory`）。
重定向（`>` / `>>` / `<` / `2>`）与 `source` 都走这条，全都一样漏。

**改法**（就是上游那个 TODO）：

```go
var pathErr *os.PathError
switch {
case err == nil:               return f, nil
case errors.As(err, &pathErr): if print { r.errf("%v\n", err) }
default:                       r.exit.fatal(err)
}
```

这样 handler 可以返回一个 `Error()` 自定义、`Unwrap()` 回到 `*os.PathError` 的包装类型：
文案完全可控，同时**保住"非 fatal"的原语义**。winbash 侧的实现在
`internal/builtins/oserr.go` 的 `ShellPathError`（`shellPathError` 类型），
`OpenHandler` 在 `internal/shell/shell.go`。

**关键**:`Unwrap` 不能省。它同时让 `errors.Is(err, fs.ErrNotExist)` 这类判定沿链成立 ——
否则只有文案看得过去、错误类别判定全失效。

**回归**:`internal/shell/openerr_test.go`。其中
`TestRedirectFailureDoesNotAbortScript` 专门盯**控制流**（不是字符串）——把补丁退回
类型断言，这条会红并指出根因。

### 4. `cd` 与 `>&` / `<&` 的措辞对齐真 bash

**位置**：`interp/builtin.go` 的 `(*Runner).changeDir`（`cd` / `pushd` / `popd` 共用）
与 `case "cd"` 的参数检查；`interp/runner.go` 的 `(*Runner).redir` 里 `case syntax.DplOut`
与 `case syntax.DplIn` 的 `default`。

**为什么在 winbash 侧拦不住**：这三处都在 mvdan/sh 的**内建**里，走不到 winbash 的
`builtins` 措辞层（那一层只处理 ①/② 层命令）。`cd` 更特殊 —— 它直接改 runner 的当前目录，
连 OpenHandler 都不过。

**修前 → 修后**（全部对着真 Git Bash 实测，2026-10-18）：

| 写法 | 修前 | 修后 = 真 bash |
|---|---|---|
| `cd <不存在>` | `cd: no such file or directory: "<路径>"` | `cd: <路径>: No such file or directory` |
| `cd <一个文件>` | `cd: no such file or directory: "<路径>"` | `cd: <路径>: Not a directory` |
| `cd ""` | `cd: empty directory path` | `cd: null directory` |
| `cd a b` | `usage: cd [dir]` | `cd: too many arguments` |
| `unset OLDPWD; cd -` | `cd: null directory` ← 把「没 cd 过」说成「空目录」 | `cd: OLDPWD not set` |
| `cd <无权限>` | `cd: permission denied: "<路径>"` | `cd: <路径>: Permission denied` |
| `echo x >& 99` | `unhandled >& arg: "99"` | `99: Bad file descriptor` |
| `echo x <& 99` | `unhandled <& arg: "99"` | `99: Bad file descriptor` |

三处共性缺陷：**路径被 `%q` 多包了一对引号**、**消息首字母小写**（POSIX 的 `strerror`
是大写）、**错误原因被丢掉**（`cd` 把「stat 失败」与「不是目录」合并成一个分支，
连 `err` 都不看）。

**`err != nil` 与 `!info.IsDir()` 必须拆成两个分支**。上游是
`if err != nil || !info.IsDir()` 一个分支报同一个措辞，于是 `cd <一个文件>` 说
"不存在"——它其实存在，只是不是目录。拆开之后 `err == nil` 时**必须**先判 `IsDir()`，
再往下走 `PWD` / `OLDPWD` 的更新（那两步在函数末尾，前置判定写错会静默漏掉）。

**上游测试同步改了 13 处期望值**（`interp/interp_test.go`）：包括 `cd ''`、`noexist`、
`>a && cd a`、`>&5`、5 处 `permission denied`、3 处 `pushd`/`popd`（共用 `changeDir`），
以及 `ABS_PATH_A` 宏的定义（`%q` → 裸字符串，因为措辞不再带引号）。
其中 `>a && cd a` 那条**变准确了**：`a` 是刚创建的文件，所以现在是 `Not a directory`。

**已知遗留**（不算回归，都是另一件事）：

- `cd <路径>` 报错里的路径是 **Windows 绝对形式**（如 `C:\Program Files\Git\no-such-dir`），
  不是用户写的 `/no-such-dir`。根因在 winbash 的 `normalizeCdPath`：它在参数进 interp
  **之前**就把 POSIX 路径改写成了 Windows 形式，而 interp 没有 API 能把原串带回来。
  这与 `pwd` 的既定设计一致（`pwd` 刻意给 `D:\...`，以便直接喂给 Windows 工具），
  所以本轮只对齐措辞、不动路径。
- `exec 3>&1` 这类 fd 3+ 的功能缺失**已修**（2026-10，见 §5 fd 表），不是措辞问题。

**回归**:`internal/shell/cdwording_test.go`（7 条 / 18 子用例）。7 处变异全部抓到。

### 5. fd 表：fd 3+ 在进程内可用

**位置**：新增 `interp/fdtable.go`；`interp/runner.go` 的 `(*Runner).redir`、
`stmtSync`、管道与子 shell 收尾、`(*Runner).exec`；`interp/api.go` 的
`Runner.fdTable` / `StdIO` / `Reset` / `subshell`；`internal/shell/shell.go` 的
`Session.Close`。

**为什么**：上游只支持 fd 0/1/2，其余一律 `unsupported redirect fd: N` ——
**功能缺失**，不是措辞问题。真 bash 里 `exec 3>&1; echo x >&3` 正常输出 x。
连带还有几个**静默做错事**的既存 bug：`cmd 3<<EOF` / `cmd 3<<<x` / `cmd 3<file`
把内容静默接到 stdin、`cmd 3&>file` 连 stdout+stderr 一起改 —— 都是「重定向的
fd 编号被无视」。一并按目标 fd 修掉。

**修前 → 修后**（全部对着真 Git Bash 实测）：

| 写法 | 修前 | 修后 = 真 bash |
|---|---|---|
| `exec 3>&1; echo x >&3` | `unsupported redirect fd: 3`、rc=1 | 输出 x、rc=0 |
| `echo x 3>&1; echo y >&3` | 同上 | y 不输出，`3: Bad file descriptor`、rc=1（fd 3 不泄漏到后续命令） |
| `exec 4>f; echo a >&4; echo b >&4` | 同上 | f 内容 `a\nb\n`（同一句柄，偏移累积） |
| `(exec 3>f; echo x >&3); echo y >&3` | 同上 | 后者 `3: Bad file descriptor`（子 shell 不穿透） |
| `echo foo 5>a` | `unsupported redirect fd: 5`、rc=1 | 输出 foo、静默创建 a、rc=0 |
| `cmd 3<<EOF` / `cmd 3<<<x` / `cmd 3<file` | **静默**接到 stdin（静默做错事） | 接到 fd 3 |
| `cmd 3&>file`（带 N 的 `&>`） | **静默**把 stdout+stderr 一起改 | 只装 fd 3 |

**设计三原则**（详见 `interp/fdtable.go` 文件头注释）：

1. **fd 0/1/2 不进表**，它们就是 `Runner.stdin/stdout/stderr` 三个槽 ——
   winbash 的 StdIO 重设机制（`closeStdin` 每次 Run 换 stdin）直接换这三个槽，
   复制一份进表立刻与它脱同步。
2. **引用计数**的底层资源：被 dup、被子 shell clone、被命令级快照各计一份
   引用，归零才 Close。否则 `exec 4>f; exec 5>&4; exec 4>&-` 会把 fd 5 脚下
   的文件提前关掉（use-after-close，比泄漏更糟）。
3. **clone 是拷贝**（map 与条目都拷、底层资源共享）：与 fork 语义一致（共享
   文件偏移），而且管道左右两段并发跑，共享 map 就是 data race。

**save/restore 与 keepRedirs**：fd 表与 stdio 槽进**同一个**快照-恢复对
（`stmtSync`），否则命令级 `cmd 3>&1` 的 fd 3 会泄漏到后续命令。`exec` 的
`keepRedirs` 分支同时豁免 fd 表的恢复与关闭（`exec 3>&1` / `exec 4>f` 跨命令
持久），并改还快照份额。

**生命周期**：子 shell / 后台命令 / 管道段 / 命令替换退出即关各自的 fd 表
（bash 语义：`(exec 3>f)` 不穿透）；`Runner.Reset` 与 `Session.Close` 全清 ——
`*r = Runner{}` 整体重建只会把 map 静默丢掉而不 Close，常驻 daemon 的
Session 每 Reset 一次就漏一个 `*os.File`。

**stdin use-after-close 防护**：`StdIO` 换槽时把 fd 表里引用旧槽值的条目失效
（winbash 的 `closeStdin` 在每次 Run 收尾把 stdin 管道换成 /dev/null）。
`exec 3<&0` 之后跨 Run 的 `cat <&3` 现在报 `3: Bad file descriptor`，而不是
拿着已 Close 的 `*os.File` 读出 `file already closed`。槽值没换的不动：
winbash 的 `switchWriter` 每次 Run 换 buffer，`exec 3>&1` 的 fd 3 写入
**跟随当次 Run 的 stdout** —— 那正是「fd 是 stdout 的 dup」的正确行为。

**不做（负面清单，与真 bash 的已知边界）**：

- **传 fd 给外部命令**（`cmd 3>log`、`bash -c 'echo x >&3'`）：Windows 的
  `cmd.ExtraFiles` 不支持，子进程看不到 fd 3+。其中「命令自身带 fd 3+
  重定向」是唯一会**无声数据丢失**的形态（重定向被丢掉、命令照跑），现在
  **响亮报错**：`cmd 3>log` → `cmd: fd 3 cannot be passed to external
  commands`、rc=1。`exec 4>f; cmd`（命令自身不带 fd 3+ 重定向）不拦 ——
  cmd 不用 fd 4 就没有损失，拦了反而误伤；cmd 真要用 fd 4 时仍会有损，
  这是残留缺口。
- `test -t 3`（`interp/test.go` 只查 0/1/2）、`read -u 3`、`{var}>` 动态分配、
  `/dev/fd/3` 路径。
- 跨命令 fd 偏移/seek 语义（io.Reader 包装下做不真）。
- 标准流**反向重开**（`read 1</dev/null`、`exec 1<file`）按近似处理：fd 1/2
  变成只读（写它报 `Bad file descriptor`，与真 bash 同），但**读** fd 1/2 做不到
  （我们的 stdio 槽只有写侧），输入内容不挂载；输出重定向落 fd 0（`0>file`）
  报 `unsupported redirect fd: 0`。
- dup 是**单向**的：`4>&3` 只复制写端、`4<&3` 只复制读端（真 bash 的 fd 双向）。
- `>&bad`（非数字）仍报 `bad: Bad file descriptor`（真 bash 静默走 `&>bad`
  语义），同 §4 的取舍：宁可响亮。

**回归**：`internal/shell/fdtable_test.go`（8 个 Test / 约 30 子用例），含变异
探针 `TestFdTableSubshellIsolation`（把 `subshell` 的 fd 表 clone 换成共享
map，它必须变红）。上游测试同步改 **1 处**期望值：`interp/interp_test.go` 的
`echo foo 5>a`，从 `unsupported redirect fd: 5` 改成真 bash 行为 `foo\n`
（fd 5 命令内创建、命令结束关闭，echo 不碰它）—— 实现 fd 表后那条是
**预期内的红**，不是补丁打错了。

## 验证

18 条用例与真 Git Bash 逐条对照，**全部一致**（含反向回归：未带 `-d`/`-n` 的 `read`/`mapfile`、以及原有的 `mapfile -d ""` 都不受影响）。语料回归在
`internal/shell/pbb_corpus_test.go` 的 `read -d` / `mapfile -n` 小节，期望值由
`go test ./internal/shell/ -update-pbb-corpus` 从**真 Git Bash** 生成，不手写。

§4 的措辞补丁另有 **13 条** cd / 重定向用例与真 Git Bash 逐条对照（`cmd/winbash/e2e` 之外
的直接实测，脚本见本轮工作记录），修前 6 类不一致、修后措辞全部一致。剩下 1 类差异是
**已记录的遗留**（Windows 绝对路径形式），见 §4 末；原先的「不支持 fd 3」已由 §5 的
fd 表修掉。§5 的 fd 表行为在 `internal/shell/fdtable_test.go` 里与真 bash 基准表
逐条对照（含子 shell 不穿透、fd 不泄漏、偏移累积、use-after-close 防护）。

## 升级流程

上游出新版本时：

1. 把新版本源码覆盖进本目录（保留本文件）：
   `go mod download mvdan.cc/sh/v3@vX.Y.Z`，然后从 module cache 复制；
2. 重新打这几处补丁 —— 上游很可能已经支持，那就**先检查**：
   `grep -n 'case "-d"' third_party/mvdan-sh/interp/builtin.go`（`read` 段）、
   `grep -n '"-n"' third_party/mvdan-sh/interp/builtin.go`（`mapfile` 段）、
   `grep -n 'errors.As(err, &pathErr)' third_party/mvdan-sh/interp/runner.go`（§3；
   若新版已经不再用 `switch err.(type)` 而改用 `errors.As`，这一处就不用打了）、
   `grep -n 'null directory' third_party/mvdan-sh/interp/builtin.go`（§4 的 `cd`；
   若新版自己就分开了 `err != nil` 与 `!info.IsDir()`、且措辞与真 bash 一致，
   这一处就不用打了）、
   `grep -n 'Bad file descriptor' third_party/mvdan-sh/interp/runner.go`（§4 的 `>&`/`<&`）、
   `ls third_party/mvdan-sh/interp/fdtable.go`（§5 的 fd 表，整文件都是本补丁，
   新版若自带 fd 3+ 支持就把文件与 `Runner.fdTable` 相关字段一起删）；
3. 如果上游已实现且行为一致，**删掉本补丁与 `replace` 行**，回到正常依赖；
4. 跑 `go test ./... -count=1` 与 `smoke-gitbash.sh`，两者都必须绿。

**注意 §4/§5 会打破上游自己的测试**：`interp/interp_test.go` 里 13 处期望值断言的正是被改掉的
旧措辞（§4），另有 1 处 `echo foo 5>a` 断言 `unsupported redirect fd: 5`（§5，已改成真 bash
行为 `foo\n`）。重打补丁时要一并改回（清单见 §4/§5），否则 `go test ./interp/` 会红一片 ——
那是**预期内的红**，不是补丁打错了。截至 v3.14.1，`go test ./interp/` 还有 **1 条既存失败**
（`TestRunnerRun/#377`，`foo=~; test -d $foo` 在 Windows 上被含空格的路径拆词）、
`go test ./syntax/` 有 1 条既存失败（`TestPrintMultiline`）—— 两者在**未打补丁的上游源码上
同样失败**，已用 `git stash` 对照确认。

## 为什么不选别的做法

- **`go mod vendor` + 补丁**：任何人跑一次 `go mod vendor` 都会把补丁**静默冲掉**，而这种事故极难发现。
- **fork 到独立仓库**：库更干净，但要多维护一个仓库、需要凭据，离线环境拉不到。
- **用 AST 改写模拟**：`read -d` 无法用 mvdan/sh 原生语法等价表达；用 `mapfile` 替代会改变「流式读取」语义（一次性读完），在管道场景会吃掉后续数据 —— 那是用一个静默错值换另一个。
- **只报错不实现**：能消灭静默，但 pure-bash-bible 的 head/tail/lines 依然不可用。

## 建议：向上游提 PR

上游对这类改动的接受模式是逐项增量（`read -s` 是 #1063，`mapfile` 初始实现是 #863），这两个选项正属于同一类。补丁合并后即可删掉本目录与 `replace` 行。
