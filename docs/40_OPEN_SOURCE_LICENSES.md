# 40 — Open Source / License Inventory

重点预期：Go toolchain/chi/pgx/sqlc、Next.js/React、Primer/Octicons、Gitea、PostgreSQL/pgvector、Redis、MinIO、Python scientific packages、Playwright 等。实际版本与许可证在 bootstrap/CI 中生成 SBOM 并核对。

原则：
- Gitea 作为独立内部基础设施服务，不 fork、不复制其 UI 到产品代码。
- 任何新增依赖必须记录 license、版本、用途、替代方案；高 copyleft/商业限制依赖需显式 ADR/法务确认。
- Worker 不得擅自引入新的基础框架或高风险 license。
