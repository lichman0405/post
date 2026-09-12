# API 设计规范

## 1. API 风格

REST/JSON + OpenAPI 3.1。读资源使用 nouns；复杂状态改变使用 command endpoint。MCP server 基于同一 application services，不复制业务逻辑。

## 2. 版本

`/api/v1`。Breaking change 新 major；response 可以增加 optional fields。公开 SDK 由 OpenAPI 生成并加 domain wrappers。

## 3. 必备 headers

- `Authorization`
- `Idempotency-Key` 对 create/command/publish/merge/release
- `X-Request-Id` 可客户端提供，否则生成
- `If-Match`/expected_version 对易并发更新的 draft/projection

## 4. Pagination

Cursor-based，稳定 sort key；不得默认 offset 用于大列表。响应 `items,next_cursor,has_more`。

## 5. Errors

统一 envelope：`code,message,request_id,details,field_errors,retryable`。Domain error code 见 `45_ERROR_MODEL.md`。禁止把 SQL/Gitea/S3 raw error 暴露。

## 6. Commands

重要命令：freeze main、create branch、commit state、abort/reopen object、open PR、submit review、merge、create release、publish asset/knowledge、change visibility、create attestation。高风险 command 必须 server-side authorization + policy validation。

## 7. Validation gates

`POST .../validate?gate=pr|main|release|asset` 返回完整结果，不修改 state。Command 必须再次 server-side run validation，不能信任前端预检。

## 8. Search API

接受 raw query + structured optional filters；服务端保存 query plan、selected entity ids、answer citations。Answer Generator 只可引用 planner/retrieval 返回的 entity ids/version。

## 9. Webhook

签名、timestamp 防重放、event id、at-least-once delivery。consumer 应幂等。平台提供 delivery logs 与 retry。
