# 应用版本发布 Runbook

此处指软件应用发布，不是科研 Project Release。

- 所有 blocking CI 绿。
- Changelog、migration、feature flags、security scan 完成。
- staging canonical E2E。
- 生成 immutable image digest。
- 生产部署后 smoke。
- 观察 error/queue/outbox/git drift 30min 的指标窗口（自动/人工可按环境）。
- 失败按 deployment runbook rollback/forward fix。
