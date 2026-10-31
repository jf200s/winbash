# delegation-discipline

一个 Reasonix **plugin package**，把“拆任务、派子代理、worktree 隔离、证据化验收”做成可复用规范。

它只描述**怎么工作**，不描述某个项目**做什么**；项目特有的端口、构建命令、退出码等约束应留在该项目自己的 `AGENTS.md`。

## 内容

- `reasonix-plugin.json`：Manifest v2，仅贡献 skills。
- `skills/delegated-delivery/SKILL.md`：完整流程（派发判据、切片、worktree、汇报契约、验收、并发隔离、底线）。
- `context/startup.md`：一页常驻摘要。
- `hooks/session_start.py`：把摘要打印到 stdout，供 `SessionStart` hook 使用。
- `AGENTS.snippet.md`：不想用 hook 时，手动并入项目/全局 `AGENTS.md` 的片段。

## 生效方式（重要）

**skill 是按需加载的**：安装后可通过 `/delegation-discipline:delegated-delivery` 调用，或由模型按 description 自动选择；不保证每轮进入上下文。

**常驻摘要按项目生效**：Reasonix 的 plugin manifest 里的 hooks 是包级、全局生效的；要只在本项目注入，应把 hook 注册在项目自己的 `.reasonix/settings.json`。

命令要用**绝对路径**：相对命令会被诊断解析成 `<workspace>/python` 这类错误路径。

```json
{
  "hooks": {
    "SessionStart": [
      {
        "command": "C:\\Windows\\System32\\cmd.exe /c type \"D:\\your\\repo\\plugin\\context\\startup.md\"",
        "timeout": 5000
      }
    ]
  }
}
```

Linux/macOS 用 `cat /path/to/plugin/context/startup.md`；也可以改用 `python /path/to/plugin/hooks/session_start.py`（同样要求绝对路径）。

项目级 hooks 需要先信任项目才会加载：

```powershell
reasonix trust
```

本项目已写入 `.reasonix/settings.json`。该目录通常被 `.gitignore` 忽略（属于本地运行状态），换机器或重新克隆后需要重建。

若项目设置不可用（例如 `.reasonix/` 被忽略且不随仓库分发），改用下面任一方式：

1. 把 `context/startup.md` 的内容并入项目 `AGENTS.md`；
2. 把 `AGENTS.snippet.md` 并入用户级 `AGENTS.md`（所有项目生效）。

## 安装

先在 dry-run 检查：

```powershell
reasonix plugin install "D:\work\中文 目录\plugin" --dry-run
```

确认输出包含 1 个 skill 后，以 link 方式装到全局：

```powershell
reasonix plugin install "D:\work\中文 目录\plugin" --link --yes
```

也可以让 agent 用 `install_source`：先 `apply=false` 看计划，再用同一 `planId` 执行 `apply=true`，`scope="global"`、`mode="link"`。

`--link` 让仓库里的插件文件改动即时生效，不需要重装。

## 验证

```powershell
reasonix plugin doctor delegation-discipline
python plugin/hooks/session_start.py
```

第二条命令应原样输出 `context/startup.md`。安装后还可在 `/skills` 或 `reasonix doctor capabilities --json` 的 Skills 段确认 `delegated-delivery` 已被索引。

## 边界

- 不修改系统提示、`AGENTS.md` 或 `REASONIX.md`。
- 不是强制机制：skill 条件加载，hook 只注入上下文，不能强制模型措辞。
- 不含项目专属约束；那些留在项目自己的指令文件里。
- 项目级 hook 配置属于本地运行状态，换机器或重新克隆后需要重建。
