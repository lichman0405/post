# 52 — Go Backend Standard

## 层次

`transport/http -> application -> domain -> ports -> infrastructure adapters`。

Domain 不依赖 chi/Gitea/Redis/S3/Postgres concrete package。Application service 明确 transaction/authz/state transition/outbox。

## API

OpenAPI-first。Command endpoint 用动词表达不可逆/受控状态变化，如 `:abort`, `:merge`, `:publish`, `:reopen`。统一 request-id 与 error envelope。

## DB

pgx + sqlc。复杂 graph traversal 使用显式 SQL。所有写操作在 application transaction boundary；append-only version/event 约束由 DB constraint + application 双重保障。

## Workers

Go background worker。Job payload 只传 identity/version/reference，不放大块敏感数据。每类 job 有 timeout/retry/backoff/dead-letter/idempotency key。

## External adapters

Gitea、S3、Redis、email、LLM、scientific adapter 都通过 port。不得让外部 provider ID 变成 domain primary identity。
