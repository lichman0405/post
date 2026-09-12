# 68 — 已锁定技术选型

| 层 | V1 选择 | 原则 |
|---|---|---|
| OS | Ubuntu 24.04 LTS | 唯一 canonical dev/CI 基线 |
| Backend | Go 1.27.x | 核心产品服务、RSG、权限、events、Git adapter |
| HTTP | net/http + chi 风格路由 | 轻量，OpenAPI-first |
| DB access | pgx + sqlc | 显式 SQL、强类型、复杂 CTE 友好 |
| Frontend | Next.js 16 Active LTS + React + TS | Public SEO + complex product UI |
| UI | Primer/Octicons | GitHub style，禁止 AI SaaS 渐变视觉 |
| Scientific | Python 3.12+ + uv | 只服务科学 Python 生态 |
| DB | PostgreSQL + pgvector | canonical semantic store |
| Graph | Postgres relations/CTE | V1 不引入 Neo4j |
| Git | internal Gitea | infra only，不 fork、不暴露 UI |
| Blob | S3/MinIO | large scientific data |
| Cache/Jobs | Redis + Go worker | V1 不引入 Kafka/NATS |
| Search | Postgres FTS + pgvector | OpenSearch 后置 |
| Dev orchestration | Go CLI `rddev` | Supervisor/Worker 确定性执行 |
| Local infra | Docker Compose | app host-native |

技术依赖不得因为单个 Worker 个人偏好被替换。
