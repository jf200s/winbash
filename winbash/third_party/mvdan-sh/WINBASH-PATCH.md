# winbash 对 mvdan/sh 的本地补丁

**来源**：`mvdan.cc/sh/v3 v3.14.1`（未经修改的上游源码，复制自 Go module cache）
**为何进库**：`go.mod` 里有一条 `replace mvdan.cc/sh/v3 => ./third_party/mvdan-sh`。
**补丁范围**：三个补丁 —— 两个内建选项（`interp/builtin.go`，外加 `interp/runner.go` 一处调用点）、一处错误分类修正（`interp/runner.go`，见 §3）。共约 45 行。

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

## 验证

18 条用例与真 Git Bash 逐条对照，**全部一致**（含反向回归：未带 `-d`/`-n` 的 `read`/`mapfile`、以及原有的 `mapfile -d ""` 都不受影响）。语料回归在
`internal/shell/pbb_corpus_test.go` 的 `read -d` / `mapfile -n` 小节，期望值由
`go test ./internal/shell/ -update-pbb-corpus` 从**真 Git Bash** 生成，不手写。

## 升级流程

上游出新版本时：

1. 把新版本源码覆盖进本目录（保留本文件）：
   `go mod download mvdan.cc/sh/v3@vX.Y.Z`，然后从 module cache 复制；
2. 重新打这几处补丁 —— 上游很可能已经支持，那就**先检查**：
   `grep -n 'case "-d"' third_party/mvdan-sh/interp/builtin.go`（`read` 段）、
   `grep -n '"-n"' third_party/mvdan-sh/interp/builtin.go`（`mapfile` 段）、
   `grep -n 'errors.As(err, &pathErr)' third_party/mvdan-sh/interp/runner.go`（§3；
   若新版已经不再用 `switch err.(type)` 而改用 `errors.As`，这一处就不用打了）；
3. 如果上游已实现且行为一致，**删掉本补丁与 `replace` 行**，回到正常依赖；
4. 跑 `go test ./... -count=1` 与 `smoke-gitbash.sh`，两者都必须绿。

## 为什么不选别的做法

- **`go mod vendor` + 补丁**：任何人跑一次 `go mod vendor` 都会把补丁**静默冲掉**，而这种事故极难发现。
- **fork 到独立仓库**：库更干净，但要多维护一个仓库、需要凭据，离线环境拉不到。
- **用 AST 改写模拟**：`read -d` 无法用 mvdan/sh 原生语法等价表达；用 `mapfile` 替代会改变「流式读取」语义（一次性读完），在管道场景会吃掉后续数据 —— 那是用一个静默错值换另一个。
- **只报错不实现**：能消灭静默，但 pure-bash-bible 的 head/tail/lines 依然不可用。

## 建议：向上游提 PR

上游对这类改动的接受模式是逐项增量（`read -s` 是 #1063，`mapfile` 初始实现是 #863），这两个选项正属于同一类。补丁合并后即可删掉本目录与 `replace` 行。
