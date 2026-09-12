# ADR-001：Go + TypeScript + Python Adapter 的 Monorepo

**Status:** Accepted

## Context
V1 同时包含公开 Web、核心领域后端、后台任务、MCP、Scientific Adapter、共享 Schema 和自主开发 Orchestrator。核心产品偏 Git/storage/event/permission/HTTP 系统工程，不应以 Python monolith 承担。

## Decision
- 单 Monorepo。
- Core backend / worker / MCP / rddev：Go。
- Web：Next.js/React/TypeScript。
- Scientific adapter：Python，隔离在稳定接口后。
- OpenAPI/JSON Schema 是跨语言契约。

## Consequences
跨语言复杂度由明确边界控制；核心服务获得简单部署、并发与强编译反馈；科学 Python 生态保留而不污染产品核心。
