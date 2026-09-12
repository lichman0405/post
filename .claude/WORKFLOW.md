# Claude Code Workflow

本项目不采用“一个 Claude Code 自己顺序写完所有业务功能”的工作方式。

Supervisor：读取根 CLAUDE.md，保持长期全局上下文，通过 `rddev` 选择并调度任务。
Worker：独立 Claude Code 进程，只读取 task package + relevant specs；产生未提交 diff + RESULT.json。
Review Worker：独立审查 diff，不具备 Git control plane。

若当前 Claude Code 不是 Supervisor，必须以 `docs/63_WORKER_CONTRACT.md` 和注入 Task Package 为最高项目工作边界，不得自行管理 PR/branch/commit。
