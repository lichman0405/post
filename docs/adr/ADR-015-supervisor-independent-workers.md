# ADR-015：Supervisor + Independent Claude Code Workers

**Status:** Accepted

一个长期 Supervisor Claude Code 负责全局规划、调度、验收和 Git control plane。每个任务由独立完整 Claude Code Worker 在独立 worktree 内实现；不是 subagent。Worker 无 commit/PR/merge 权限。确定性编排由 Go CLI `rddev` 执行。
