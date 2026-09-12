# AGENTS.md

本仓库使用 Supervisor + Independent Claude Code Workers 的研发模型。

- 长期 Supervisor 读取根目录 `CLAUDE.md`。
- Worker 不应自行读取完整 `CLAUDE.md` 作为任务上下文；其规则由 `rddev` 从 `docs/63_WORKER_CONTRACT.md` 与 task definition 生成。
- 任何 Agent 先确认自己的角色：Supervisor / Worker / Review Worker。
- Worker 没有 Git control-plane 权限，只能修改自己的 worktree 并提交 `RESULT.json`。
- 产品与科学语义以 `docs/`、`specs/`、`tasks/tasks.json`、ADR 为准。
