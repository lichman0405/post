# 53 — Database Standard

PostgreSQL 是 semantic canonical store。

- Core identity/version/relation/state transition 使用关系表。
- Domain metadata 使用 versioned JSONB + JSON Schema validation；高频字段投影成列/index。
- Current state 是 materialized pointer/projection，历史版本 append-only。
- `UPDATE` 不能覆写历史 scientific content；创建新 version 后移动 current pointer。
- Domain event + outbox 与状态变更同一 transaction。
- 使用 FK/unique/check constraint 尽可能编码不可变量。
- migration forward-only；已经发布 migration 不修改。
- RSG traversal 先 recursive CTE；Graph DB 未来仅 projection。

## Schema canonical 与 snapshot（Phase Boundary checkpoint，2026-09-13）

- **canonical schema history = `infra/migrations/`**。迁移是开发期 schema evolution 的唯一真相源。
- **`specs/database/postgres.sql` 是生成物**：由 `scripts/gen_schema_snapshot.py` 从迁移的
  Up 段按序生成；`make check-schema-snapshot` 在两者不一致时失败，CI 的 spec-validation 也跑它。
  **不手工编辑**。
- 方向为什么反转：原先该文件被声明为"source of truth"、迁移是它的"faithful decomposition"，
  结果是**文件冻结、迁移前进**——P1 结束时它已落后五个迁移，而所有人都还在信任它。
  **会静默滞后的 canonical artifact 比没有更糟，因为它被相信。**
- **迁移编号由 Supervisor 在 dispatch 时分配**（`rddev` 的编号台账，随任务包下发，rework/respawn 不变）。
  **Worker 不得自行选号**，也不得从索引推断——两个并行 Worker 曾各自创建 `00017_*.sql`。
- **Worker 对 `specs/` 的唯一写入口就是这个快照**：它在
  `specs/orchestrator/derived-artifacts.json` 中声明为 `infra/migrations/**` 的 derived artifact，
  因此覆盖迁移目录的任务**必须**同时覆盖它（spawn 前的 scope 校验会强制），
  并且只能通过重新生成来写。产品规格（其余 `specs/**`、`docs/**`）仍然 Supervisor-only。
