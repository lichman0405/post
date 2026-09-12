# Agent、MCP 与 Semantic API

## 1. Agent 假设

V1 第一 Agent client 暂按 Claude Code-style agent 设计。平台不得写死供应商；所有能力通过 HTTP API / MCP / SDK 暴露。

## 2. Agent 能做

读取 Project/RSG、创建 branch、创建/更新 draft/branch Scientific Object、建立 relation、上传并 attach blob、创建 evidence assertion proposal、创建 issue/PR、运行 validation、查询 search/context、建议 contribution role。

## 3. Agent 不能独立做

merge to main、publish private→public、改变 rights holder、批准 legal/custom agreement、最终 scientific conflict resolution、删除历史、绕过 Organization policy。可创建 proposal，由 Web governance action 完成。

## 4. 人类责任

Audit 必须记录 `actor=user`, `via=claude-code|api|web|git-compat`, 可选 model/client metadata。不得把 Agent 写成独立法律/科研责任主体。

## 5. Tool design

工具必须 semantic、small surface、schema validated、idempotency key 支持、返回 stable error code。不要设计 `write_arbitrary_manifest` 作为默认工具；高级 raw endpoint 仅 admin/compat。

## 6. Upload

Agent 先请求 signed upload URL → 上传 S3/MinIO → 调 `blob.finalize` → attach 到具体 Scientific Object。Blob 未 attach 前为 temporary，TTL 后可 GC；一旦成为历史引用不可物理删除。
