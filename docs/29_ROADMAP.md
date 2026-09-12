# 29 — 完整 V1 路线图

具体 Task 的要求、依赖、验收见 `tasks/ROADMAP_TASKS.md` 与 `tasks/tasks.json`。

## P0 — Canonical Dev Environment / Orchestrator / Bootstrap

Ubuntu preflight、Go+Next+Python Monorepo、Docker infra、DB、CI、`rddev`、独立 Worker、权限隔离、四层 Gate。Gate：Supervisor 能在不手工管理进程/Git worktree 的情况下调度至少两个并行 Worker，并拒绝 Worker commit/越界 diff。

## P1 — Identity / Organization / Project shell
用户、Profile、Organization、Project Public/Private、导航、权限框架。

## P2 — RSG Core
Scientific Object/version/relation/state transition/branch/current projections/schema validation。

## P3 — Git compatibility & Files
Gitea adapter、repo mapping、protected main、push ingestion、只读 Files。

## P4 — PR / Review / Semantic Merge
Research State Diff、review、conflict、merge。

## P5 — Evidence / Knowledge
Research Question、Hypothesis、Claim、Finding、Evidence/Provenance。

## P6 — Release / Abort / Policies
Frozen main、Release、Abort/Reopen、policy、audit。

## P7 — Asset Hub / Publishing / Rights
四类 Research Asset、publish gate、PID/version、lineage、rights。

## P8 — Open Network / Profiles / Contribution
Explore、Fork/Contribute、Contribution Ledger/Profile。

## P9 — Search & Research Answer
Network-only evidence-backed search、Start Research Project。

## P10 — Events / Subscriptions / Impact
Domain events/outbox、Inbox/RSS/webhook、dependency impact。

## P11 — UI / Security / Performance hardening
Primer consistency、a11y、rate limit、observability、performance。

## P12 — Canonical E2E / Deployment
MOF 完整开放研发闭环、backup/restore、staging/prod runbook、Master Acceptance。
