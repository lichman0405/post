# 67 — 四层测试 Gate

## G1 Worker Local

任务定义必须列 required tests。Worker 运行后在 RESULT 提交 command/exit/evidence。Go 至少考虑 `go test`, `go vet`, staticcheck；Web 至少 typecheck/lint/unit；adapter pytest；UI 关键路径 Playwright。

## G2 Supervisor Acceptance

Supervisor 重跑任务测试并检查：

- diff 是否只在 allowed scope；
- acceptance criteria 是否逐条有证据；
- HEAD 是否仍等于 baseline；
- 是否改变 schema/API/ADR 而未声明；
- 是否存在测试弱化；
- error/security/permission/visibility negative path。

## G3 Integration / E2E

跨系统链路使用真实容器服务。特别是：PostgreSQL transaction、Gitea branch protection/webhook、S3/MinIO hash、Redis retry、browser navigation、auth/visibility。

## G4 Merge

PR merge 前按 change impact 运行完整受影响集；核心/安全/数据库/发布类改动执行 full gate。

## 额外规则

- flake 不是“rerun until green”；先定位再修。
- migration 必须 fresh install + upgrade path。
- browser E2E 验证 console/network errors、keyboard、loading/empty/error state。
- canonical MOF workflow 每个 phase milestone 至少跑一次，P12 必须完整跑通。
