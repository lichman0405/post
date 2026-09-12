# 65 — Monorepo 结构

```text
post/
├── apps/
│   └── web/                       # Next.js/React/Primer
├── cmd/
│   ├── api/                       # Go HTTP API
│   ├── worker/                    # Go background worker
│   ├── mcp-server/                # Go MCP endpoint
│   └── rddev/                     # Go development orchestrator
├── internal/
│   ├── domain/
│   ├── application/
│   ├── authz/
│   ├── rsg/
│   ├── evidence/
│   ├── assets/
│   ├── rights/
│   ├── contribution/
│   ├── search/
│   ├── events/
│   ├── gitprovider/
│   ├── storage/
│   └── persistence/
├── services/
│   └── scientific-adapter/        # Python 3.12+ / uv
├── packages/
│   ├── schemas/                   # JSON Schema canonical copies
│   ├── api-contracts/             # OpenAPI + generated TS client
│   ├── ui/                        # shared React/Primer components
│   └── domain-fixtures/
├── infra/
│   ├── docker/
│   ├── migrations/
│   ├── gitea/
│   └── observability/
├── tests/
│   ├── integration/
│   ├── contract/
│   ├── e2e/
│   ├── security/
│   └── acceptance/
├── tasks/
├── docs/
├── specs/
├── ops/
├── .rddev/                        # runtime state，不提交敏感内容
├── CLAUDE.md
├── Makefile
├── go.mod
├── package.json
└── docker-compose.yml
```

## 规则

- Go root module 承载核心产品后端；不要人为拆多个微服务 repo。
- pnpm workspace 只负责 web/ui/generated TS client，不控制 Go/Python。
- OpenAPI/JSON Schema 是跨语言契约；生成物禁止人工分叉。
- Python adapter 只通过稳定 API/RPC/queue 与 Go core 交互，不直接连 canonical product tables 修改状态。
- Worker 可读全仓库，但 task allowed_scope 控制写范围。
