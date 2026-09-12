# 69 — GitHub Source Repository & Supervisor Control Plane

## 1. 目标

POST 的产品开发使用一个 canonical GitHub repository：`lichman0405/post`。本文件定义源码 GitHub control plane，防止它与产品运行时 Gitea infrastructure 混淆。

## 2. Canonical repository

```text
owner/name: lichman0405/post
default branch: main
https remote: https://github.com/lichman0405/post.git
ssh remote: git@github.com:lichman0405/post.git
```

`rddev doctor` 最终必须能够校验：

- 当前目录位于 Git repository；
- `origin` 的 normalized owner/name 为 `lichman0405/post`；
- default integration branch 为 `main`；
- Supervisor environment 可访问 GitHub；
- Worker environment 没有 GitHub write credential；
- Supervisor 与 Worker credential boundary 可通过自动测试证明。

## 3. Supervisor workflow

建议生命周期：

```text
Task ready
  ↓
Supervisor optionally creates GitHub Issue
  ↓
Supervisor creates task branch + worktree
  ↓
rddev starts independent Claude Code Worker
  ↓
Worker modifies files + RESULT.json, no commit
  ↓
G1 Worker Local Gate
  ↓
Supervisor G2 acceptance
  ↓
Supervisor commit + push
  ↓
PR + G3/G4
  ↓
Supervisor merge
  ↓
Task status -> merged
```

任务 ID 必须进入 branch/commit/PR traceability。推荐：

```text
branch: task/T0042-short-slug
commit: T0042: <imperative summary>
PR: [T0042] <task title>
```

如果一个 GitHub Issue 对应多个 machine tasks，则 PR body 中列出全部 Task IDs；不得强行一任务一 Issue。

## 4. Worker security boundary

Worker 启动时：

- unset `GH_TOKEN`, `GITHUB_TOKEN` 及同类变量；
- 不挂载 Supervisor SSH private keys；
- `gh auth status` 不得显示 write-capable authenticated account；
- Claude permissions deny Git control commands；
- `rddev collect` 校验 HEAD 未移动、refs 未改变；
- Worker 仅提交文件系统 diff 和符合 schema 的 `RESULT.json`。

仅靠提示词声明“不要 commit”不构成权限隔离。

## 5. GitHub repository visibility

Visibility 属于 repository governance，不是产品 runtime visibility。首次 push 前 Supervisor 必须重新查询远端状态。

若 repository 为 Public，则提交到 GitHub 的所有规格、源代码、ADR、测试 fixture 都应视为公开信息。Secret、真实客户数据、内部 credentials、私有数据集绝不能进入 repository。

如果 remote visibility 与项目 owner 的预期不一致，属于 L3 governance 阻塞：停止首次 push 并请求人工确认，不得自行改变 repository visibility。

## 6. Branch policy target

P0 CI 就绪后，Supervisor 应配置/提示人工配置以下目标规则（GitHub plan/API 权限受限时不得伪造成功）：

- `main` 不作为 Worker 直接修改目标；
- merge 前 required CI Gate 全绿；
- 禁止 force push 到 `main`；
- 删除已 merge task branch 可自动化；
- merge strategy 优先 squash 或 merge commit，项目只选一种 canonical 策略并写 ADR/L1 decision；
- release tag 只能由 Supervisor release workflow 创建。

## 7. GitHub 与 Gitea mapping 禁止

禁止直接把源码 GitHub 的：User / Issue / PR / Release 模型复用为 POST runtime 的 Project / Research PR / Release 模型。

源码 GitHub 是 POST 的工程协作工具；GiteaProvider 是 POST 产品实现的一部分。二者的身份域、权限域、生命周期均独立。
