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
**已交付**：V1 完成声明于 2026-09-24 成立（`tasks/decisions.md` ㊼ §7 的四条判定，证书 `tests/acceptance/v1-final-report.md`）。

## P13 — V1 之后的加固：流水线自己的可信度
V1 交付之后，仓库里剩下的是**欠账**而不是功能：流水线为了自己的诚实度必须先可信——挡着每笔任务必过作业
的偶发红（`#243` 的临时目录清理竞态、`#207`/`#133` 的退出状态窗口、`#240` 的 Gitea bootstrap 竞态），
然后是安全与数据完整性缺陷。这一阶段**不改产品语义**，也不在 V1 证书的判定范围内
（证书钉在它自己那棵树上，见 `tests/acceptance/v1-final-report.md:56-62`）。
