# 50 — 通用 Coding Standard

## Go

- `gofmt` 必须；`go vet` + static analysis。
- Context 必须沿 request/job boundary 传播。
- 错误保留 cause，用 typed/domain error 映射稳定 API error code；不得吞错。
- Domain/Application 不 import Gitea/MinIO/HTTP concrete implementation。
- DB 使用 pgx/sqlc/显式 transaction；禁止随意动态 SQL 拼接。
- goroutine 必须有生命周期/取消策略，禁止无界 fan-out。
- public action 必须显式 authz，不靠前端隐藏。

## TypeScript/React

- strict TypeScript；不以 `any` 逃避类型。
- API types 来自 OpenAPI generated client；禁止手抄第二套 DTO。
- React component 不承载 domain state transition；通过 Go API command。
- UI 遵循 Primer tokens/spacing/states。

## Python Scientific Adapter

- `pyproject.toml` + uv lock；类型检查与 pytest。
- 不直接写 canonical PostgreSQL domain tables。
- parser 输出结构化、版本化、可验证 payload。

## 数据/命名

DB snake_case；Go exported PascalCase/internal camelCase；JSON API 命名在 OpenAPI 统一。ID 变量必须带实体语义。

## 安全日志

不得记录 token/secret、完整 private raw scientific blob、session cookie。错误响应不得暴露 SQL/Gitea/S3 raw error。
