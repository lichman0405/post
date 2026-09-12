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
