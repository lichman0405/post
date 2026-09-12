# ADR-005：V1 使用 PostgreSQL + pgvector，不引入专用 Graph DB/OpenSearch
Status: Accepted

## Decision
结构化、FTS、vector、relation traversal 都先在 Postgres 实现；达到明确规模/性能阈值后再增加 projection engine。

## Reason
降低 V1 运维面和一致性复杂度。
