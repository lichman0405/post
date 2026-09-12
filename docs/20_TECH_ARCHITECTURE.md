# 20 — V1 技术架构

## 1. 目标

架构优先满足：科研领域一致性、可测试、可导出、权限无泄漏、Agent 可操作、Git 兼容、未来可替换基础设施。V1 不追求微服务数量和分布式复杂度。

## 2. 推荐拓扑

```text
Browser
  │
  ▼
Next.js Web (TypeScript / React / Primer)
  │ HTTP/OpenAPI
  ▼
Go Core API
  ├──────────────┬──────────────┬───────────────┐
  ▼              ▼              ▼               ▼
PostgreSQL      Redis         S3/MinIO         Gitea
+ pgvector      jobs/cache    blobs            Git infra
  │              │
  │              └── Go Background Worker
  │
  ├── RSG / versions / evidence / rights / contribution
  ├── append-only domain event + transactional outbox
  └── search projections

Go MCP Server ───────────────► Go application/domain services
Python Scientific Adapter ───► stable API/RPC boundary
```

## 3. Monorepo

目标结构见 `65_MONOREPO_STRUCTURE.md`。核心后端使用单 Go module + `cmd/*` binaries + `internal/*`；Web 使用 pnpm workspace；Python adapter 独立 `uv` project。

## 4. Frontend

Next.js 16 Active LTS + React + TypeScript。优先 Primer React/tokens/Octicons。Next.js 提供 routing、SSR/SEO、public pages；不得通过 Server Actions 形成第二套绕过 Go API 的 canonical business backend。

Public Project、Asset、Claim/Finding、Researcher/Org、Search Answer 页面需要服务端可索引输出。复杂交互通过 typed OpenAPI client 调 Go API。

## 5. Go Core Backend

- Go 1.27.x。
- 标准 `net/http` + 轻量 router（chi 或同级，属于 L1，但不得引入重型魔法框架）。
- OpenAPI-first；推荐从 contract 生成 TS client，并对 Go server contract 做 compile/contract checks。
- DB：pgx + sqlc；复杂 RSG traversal 使用明确 SQL/recursive CTE。
- Domain/Application layer 不依赖 HTTP/Gitea/MinIO 具体实现。
- 命令型 endpoint 明确表达状态转换，如 abort/merge/publish/reopen，不把科研领域退化成 CRUD。

## 6. PostgreSQL / RSG

PostgreSQL 是 Scientific/R&D semantic canonical store。Scientific Object identity/version、relations/version、state transitions、Evidence Assertion、Rights、Contribution、Release metadata 均落 PostgreSQL。

RSG 虽是图模型，V1 使用关系表 + indexes + recursive CTE；Neo4j/其他图数据库只能是未来可重建 projection/index，不能成为唯一真相源。

领域 payload 使用 Core relational identity + versioned schema-validated JSONB 的组合；高频可查询字段使用显式列/index/generated projection。

## 7. Git

Gitea 只负责 Git transport/object hosting/LFS 等成熟基础能力。不 fork Gitea、不向用户暴露 Gitea UI、不把 Gitea PR/Org/User 当产品 canonical object。

产品通过 `GitProvider` interface 调用 Gitea adapter。Our Project/Research PR/Release 与 Gitea repository/PR/release 仅 mapping，不同身份。

## 8. Blob

S3-compatible，dev 使用 MinIO。大科学文件不直接进入 Git。对象版本只引用 immutable blob identity/hash/metadata/access policy。

## 9. Async / Cache

Redis 负责 cache 和 V1 async queue。Go worker 处理：git ingestion、blob hash/preview、embedding、event delivery、impact analysis、notification digest、reconciliation。

Job 必须 idempotent、retry/backoff、dead-letter/failed evidence、trace correlation。V1 不引入 Kafka/NATS；只有明确吞吐/多订阅瓶颈证据后通过 ADR 引入。

## 10. Search

PostgreSQL FTS + pgvector + structured filters + RSG relation traversal。LLM 只做 query planning/answer interpretation，不做 source of truth。Embedding/LLM provider 通过 port abstraction。

## 11. Python Scientific Adapter

Python 不是核心产品后端。它只承载 pymatgen/ASE/RDKit/MDAnalysis 等科学 Python 生态、格式解析与科学计算辅助能力。不得直接绕过 Go application service 写 canonical product state。

## 12. 数据真相与 Release

- PostgreSQL：semantic truth。
- Git：repository/file truth。
- S3：large blob truth。
- Release：immutable binding：RSG snapshot hash + Git SHA + blob manifest/hash + schema/policy/rights versions。

正常读取 current relational projection，不靠 replay 全 event log。每次状态变更同一 DB transaction 写 version/current pointer/domain_event/outbox；外部 Git/S3 用 saga + reconciliation。
