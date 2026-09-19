# V1 Roadmap — 逐任务要求与验收

> 机器真相源：`tasks/tasks.json`。每个 Worker 的实际 allowed_scope 必须由 Supervisor 在 spawn 前进一步收窄。

## P0 — Canonical 开发环境、Orchestrator 与工程脚手架

### T0000 — Ubuntu 24.04 Canonical Environment Preflight

**依赖：** 无
**最高自主决策级别：** L1

**要求**
- 实现/提供 Ubuntu 24.04 LTS 环境检查
- 检查 amd64、CPU/RAM/disk、fd limit、Git/Claude/Go/Node/pnpm/Python/uv/Docker Compose
- 非 canonical 环境给出明确失败或 WSL2 指引
- 记录 toolchain 版本，不允许 Worker 静默 major upgrade

**交付物**
- rddev doctor/preflight specification
- ops bootstrap documentation

**Allowed scope 上限**
- `ops/**`
- `docs/64_UBUNTU_DEV_ENV.md`
- `cmd/rddev/**`
- `internal/devorchestrator/**`

**验收标准**
- [ ] Ubuntu 24.04 clean host/WSL2 可得到确定性检查结果
- [ ] 缺少任一关键工具时错误明确并给出修复动作
- [ ] Windows Native 被标记为 unsupported canonical environment

**Required tests**
- environment preflight unit
- ubuntu smoke

---

### T0001 — 验证规格仓库与任务依赖图

**依赖：** T0000
**最高自主决策级别：** L1

**要求**

- 校验 canonical source repository：`origin` normalized owner/name 为 `lichman0405/post`，integration branch 为 `main`；首次 push 前重新读取 visibility。
- 校验 Markdown/spec JSON/YAML 可读取
- 校验 tasks.json 依赖无缺失/无环
- 生成规格版本标记

**Allowed scope 上限**
- `scripts/**`
- `specs/**`
- `tasks/**`

**验收标准**

- remote/branch/visibility preflight 可审计；visibility 与 owner 预期不明确时进入 `SPEC_BLOCKED` 并禁止首次 push。
- [ ] CI 中 spec-validation 通过
- [ ] 无 orphan dependency
- [ ] 将验证命令写入 scripts

**Required tests**
- spec validation unit

---

### T0002 — 初始化 Go + Next.js + Python Adapter Monorepo

**依赖：** T0001
**最高自主决策级别：** L1

**要求**
- 初始化 root Go module：cmd/api,worker,mcp-server,rddev 与 internal domain/application/ports/adapters
- 初始化 apps/web：Next.js Active LTS + React + strict TypeScript + Primer/Octicons
- 初始化 services/scientific-adapter：Python 3.12+ + uv
- 初始化 packages/schemas,api-contracts,ui,domain-fixtures
- 统一 Makefile/format/lint/test/build，不使用 Turborepo 控制 Go/Python

**Allowed scope 上限**
- `go.mod`
- `go.sum`
- `cmd/**`
- `internal/**`
- `apps/web/**`
- `services/scientific-adapter/**`
- `packages/**`
- `Makefile`
- `package.json`
- `pnpm-lock.yaml`

**验收标准**
- [ ] 根目录一条命令完成 Go/Web/Python 基础检查
- [ ] Go API/Web/Python adapter 均有最小 build/smoke
- [ ] 不存在 TypeScript API backend placeholder
- [ ] workspace/跨语言契约边界与 docs/65 一致

**Required tests**
- go build smoke
- go test smoke
- web typecheck
- web lint
- python adapter pytest smoke

---

### T0003 — 本地基础服务 Docker Compose

**依赖：** T0002
**最高自主决策级别：** L1

**要求**
- Docker Compose: PostgreSQL+pgvector、Redis、MinIO、Gitea、Mailpit
- healthcheck 与 named volume
- 一键 init bucket/Gitea service account/test organization
- 不要求应用本身在 dev 时运行于容器

**Allowed scope 上限**
- `docker-compose.yml`
- `infra/docker/**`
- `infra/gitea/**`
- `ops/**`
- `Makefile`

**验收标准**
- [ ] docker compose up 后所有依赖 healthy
- [ ] 重复启动幂等
- [ ] README/脚本说明完整

**Required tests**
- service health integration

---

### T0004 — 配置与 Secret 管理基线

**依赖：** T0002, T0003
**最高自主决策级别：** L1

**要求**
- Go typed config loader
- Web/Python 独立 env validation
- env schema/config validation
- dev/test/prod config 分层
- .env.example 不含 secrets

**Allowed scope 上限**
- `internal/config/**`
- `apps/web/**/config*`
- `services/scientific-adapter/**`
- `*.env.example`
- `Makefile`

**验收标准**
- [ ] 缺关键配置时 fail fast 且错误清晰
- [ ] secret 不出现在 log/test snapshot

**Required tests**
- config unit
- secret scan

---

### T0005 — 数据库 migration 与 pgx/sqlc 数据访问层

**依赖：** T0003
**最高自主决策级别：** L1

**要求**
- 将 specs/database/postgres.sql 转为 forward-only migrations
- Go 使用 pgx + sqlc 或等价显式强类型 SQL 方案
- 保留 append-only/FK/check/unique 约束
- 提供 transaction helper 与 test DB namespace

**Allowed scope 上限**
- `infra/migrations/**`
- `internal/persistence/**`
- `sqlc.yaml`
- `specs/database/**`
- `tests/integration/**`

**验收标准**
- [ ] fresh DB migration 成功
- [ ] upgrade/repeat migrate 安全
- [ ] sqlc/generator 无 drift
- [ ] 关键约束 integration test 通过

**Required tests**
- db migration integration
- sql generation drift

---

### T0006 — 基础 Go API/Worker/MCP + Web + Python Adapter 健康检查

**依赖：** T0002, T0004
**最高自主决策级别：** L1

**要求**
- Go api/worker/mcp 三个 binary 可启动
- Next.js web 可启动并显示 dev status
- Python scientific-adapter 可启动
- /healthz,/readyz
- worker 可消费 test job 或执行最小 job loop

**Allowed scope 上限**
- `cmd/**`
- `internal/**`
- `apps/web/**`
- `services/scientific-adapter/**`
- `Makefile`

**验收标准**
- [ ] host-native 一键启动所有 app
- [ ] CI smoke 通过
- [ ] 不依赖 TypeScript backend

**Required tests**
- application smoke

---

### T0007 — 日志、request id 与基础 telemetry

**依赖：** T0006
**最高自主决策级别：** L1

**要求**
- Go structured logs
- request/correlation id
- 敏感字段 redact
- background worker/job correlation
- Web/Python adapter correlation propagation

**Allowed scope 上限**
- `internal/observability/**`
- `cmd/**`
- `apps/web/**`
- `services/scientific-adapter/**`
- `tests/**`

**验收标准**
- [ ] 请求跨 API→job 可追踪
- [ ] token/password 不记录

**Required tests**
- logging integration

---

### T0008 — CI 基线与多语言 Gate

**依赖：** T0002, T0005, T0006
**最高自主决策级别：** L1

**要求**
- CI: spec validation + Go fmt/vet/static/unit + Web lint/typecheck/unit/build + Python lint/type/pytest + migration/integration
- 校验 task_status/tests JSON
- OpenAPI/schema generation drift check
- 提供 progress update helper

**Allowed scope 上限**
- `.github/**`
- `.gitlab-ci.yml`
- `Makefile`
- `scripts/**`
- `ops/**`
- `tasks/**`

**验收标准**
- [ ] PR CI 全绿
- [ ] 失败 stage 明确
- [ ] Ubuntu runner 与 canonical env 对齐
- [ ] Supervisor 可从 task_status 恢复

**Required tests**
- ci self test

---

### T0009 — 实现 rddev Go CLI 与任务状态机

**依赖：** T0002, T0004
**最高自主决策级别：** L1

**要求**
- 实现 rddev doctor/env/task/worker/git/pr 命令骨架
- 读取 tasks.json/task_status.json 并验证合法状态转换
- 状态写入原子化并带 run_id
- CLI 输出支持人类文本与 JSON

**交付物**
- cmd/rddev
- internal/devorchestrator

**Allowed scope 上限**
- `cmd/rddev/**`
- `internal/devorchestrator/**`
- `specs/orchestrator/**`
- `tasks/**`

**验收标准**
- [ ] rddev task next 只返回 dependencies 满足的任务
- [ ] 非法状态转换拒绝
- [ ] 并发写状态不会静默覆盖
- [ ] CLI help/JSON output 可测试

**Required tests**
- rddev unit
- task state integration

---

### T0010 — Worktree 与独立 Claude Code Worker 调度

**依赖：** T0009
**最高自主决策级别：** L1

**要求**
- Supervisor-owned branch/worktree lifecycle
- 通过 claude -p 启动独立完整 Claude Code Worker
- 记录 PID/session/version/model/budget/logs/baseline SHA
- 默认并行 3、硬上限 4
- worker crash/timeout/exit status 可恢复

**交付物**
- worker registry
- prompt renderer
- process manager

**Allowed scope 上限**
- `cmd/rddev/**`
- `internal/devorchestrator/**`
- `specs/orchestrator/**`
- `.rddev.example/**`
- `tests/**`

**验收标准**
- [ ] 可同时启动两个不同 task Worker 且 worktree 互不污染
- [ ] Supervisor 重启后能发现 running/stale worker
- [ ] Worker 不是 Claude subagent
- [ ] 任务完成可收集结构化 RESULT

**Required tests**
- worktree integration
- two-worker concurrency smoke
- worker crash recovery

---

### T0011 — Worker 权限隔离与结果收集

**依赖：** T0010
**最高自主决策级别：** L1

**要求**
- Worker 环境剥离 GitHub/Gitea/prod credentials
- Claude dontAsk + allow/deny/hooks 阻断 Git control-plane 操作
- collect 校验 HEAD 等于 baseline 且无非法 ref mutation
- 按 allowed_scope 校验 diff
- 验证 RESULT.json schema

**交付物**
- worker permission policy
- collect validator

**Allowed scope 上限**
- `cmd/rddev/**`
- `internal/devorchestrator/**`
- `specs/orchestrator/**`
- `.claude/**`
- `tests/**`

**验收标准**
- [ ] 尝试 git commit/push/branch mutation 的测试 Worker 被阻断或最终强制 rejected
- [ ] 越界文件修改被 rejected
- [ ] 合法代码修改可正常测试和收集
- [ ] Worker 不需要 Git remote credentials

**Required tests**
- git control denial e2e
- scope violation e2e
- result schema unit

---

### T0012 — Supervisor 四层验收 Gate 与自动开发闭环

**依赖：** T0008, T0011
**最高自主决策级别：** L2

**要求**
- 实现 G1/G2/G3/G4 task metadata 与执行器
- Supervisor 可 accept/reject/rework/respawn worker
- accepted 后仅 Supervisor 执行 commit/push/PR/merge workflow
- 支持 independent Review Worker
- 所有动作写 audit/progress/result evidence

**交付物**
- gate runner
- supervisor workflow docs/tests

**Allowed scope 上限**
- `cmd/rddev/**`
- `internal/devorchestrator/**`
- `specs/orchestrator/**`
- `tasks/**`
- `tests/acceptance/**`
- `docs/61_DEVELOPMENT_ORCHESTRATION.md`

**验收标准**
- [ ] 一个示例任务可完整 Worker→G2→commit→PR 流程演练
- [ ] 失败 Worker 不会进入 accepted
- [ ] blocking test 红灯无法 merge
- [ ] Supervisor 可恢复中断 workflow

**Required tests**
- four-gate acceptance e2e
- rejection/retry e2e
- supervisor git control e2e

---

## P1 — 身份、组织与 Project Shell

### T0101 — 用户认证与 session

**依赖：** T0012
**最高自主决策级别：** L1

**要求**
- 实现安全登录与 session
- 支持 email + 至少一个 OAuth/OIDC adapter
- CSRF/secure cookie/rate limit 基础

**Allowed scope 上限**
- `cmd/api/**`
- `internal/domain/**`
- `internal/application/**`
- `internal/authz/**`
- `internal/persistence/**`
- `apps/web/**`
- `packages/api-contracts/**`
- `tests/**`

**验收标准**
- [ ] 未认证写 API 401
- [ ] 登录/退出 E2E
- [ ] 账号枚举防护

**Required tests**
- auth unit
- auth e2e

---

### T0102 — Research Profile 基础

**依赖：** T0101
**最高自主决策级别：** L1

**要求**
- handle/display name/bio/公开 profile
- 公开/私有字段区分
- profile URL 稳定

**Allowed scope 上限**
- `cmd/api/**`
- `internal/domain/**`
- `internal/application/**`
- `internal/authz/**`
- `internal/persistence/**`
- `apps/web/**`
- `packages/api-contracts/**`
- `tests/**`

**验收标准**
- [ ] 未登录可读公开 profile
- [ ] 用户只能改自己可编辑字段

**Required tests**
- profile api
- research profile e2e

---

### T0103 — Organization 创建与成员关系

**依赖：** T0101
**最高自主决策级别：** L1

**要求**
- org CRUD governance
- membership role
- affiliation 时间与 verified 字段

**Allowed scope 上限**
- `cmd/api/**`
- `internal/domain/**`
- `internal/application/**`
- `internal/authz/**`
- `internal/persistence/**`
- `apps/web/**`
- `packages/api-contracts/**`
- `tests/**`

**验收标准**
- [ ] Owner 可邀请/调整 role
- [ ] 普通成员不能提升自己
- [ ] 离职不删除历史

**Required tests**
- org permission integration

---

### T0104 — Project 创建与 Purpose/Program

**依赖：** T0103
**最高自主决策级别：** L1

**要求**
- Public/Private preset
- purpose 与可选 program
- 创建者成为 owner
- provision pending 状态

**Allowed scope 上限**
- `cmd/api/**`
- `internal/domain/**`
- `internal/application/**`
- `internal/authz/**`
- `internal/persistence/**`
- `apps/web/**`
- `packages/api-contracts/**`
- `tests/**`

**验收标准**
- [ ] Project 创建成功且 visibility 正确
- [ ] slug conflict 清晰

**Required tests**
- project api

---

### T0105 — Project 成员权限框架

**依赖：** T0104
**最高自主决策级别：** L1

**要求**
- Owner/Maintainer/Contributor/Viewer server-side auth
- default deny policy engine interface

**Allowed scope 上限**
- `cmd/api/**`
- `internal/domain/**`
- `internal/application/**`
- `internal/authz/**`
- `internal/persistence/**`
- `apps/web/**`
- `packages/api-contracts/**`
- `tests/**`

**验收标准**
- [ ] 权限矩阵核心动作与 CSV 一致
- [ ] 前端隐藏不代替后端拒绝

**Required tests**
- permission matrix tests

---

### T0106 — Public/Private 读取隔离

**依赖：** T0104, T0105
**最高自主决策级别：** L1

**要求**
- 匿名可读 public project shell
- private project 对未授权返回不泄漏式 404/403 policy
- 列表过滤

**Allowed scope 上限**
- `cmd/api/**`
- `internal/domain/**`
- `internal/application/**`
- `internal/authz/**`
- `internal/persistence/**`
- `apps/web/**`
- `packages/api-contracts/**`
- `tests/**`

**验收标准**
- [ ] 匿名搜索/list 不出现 private
- [ ] 直接猜 ID 不泄漏 metadata

**Required tests**
- privacy negative e2e

---

### T0107 — 全局 GitHub-style 导航与 Layout

**依赖：** T0102, T0104
**最高自主决策级别：** L1

**要求**
- Primer 风格 header/nav
- Home/Explore/Search/Projects/Assets/People/Organizations/Notifications
- responsive 基线

**Allowed scope 上限**
- `cmd/api/**`
- `internal/domain/**`
- `internal/application/**`
- `internal/authz/**`
- `internal/persistence/**`
- `apps/web/**`
- `packages/api-contracts/**`
- `tests/**`

**验收标准**
- [ ] 无渐变紫/玻璃拟态
- [ ] 键盘可达
- [ ] 桌面信息密度符合 06_UI_UX_SPEC

**Required tests**
- visual smoke
- a11y smoke

---

### T0108 — Project Shell 与 tabs

**依赖：** T0104, T0107
**最高自主决策级别：** L1

**要求**
- Overview/Research/Issues/PR/Releases/Assets/Files/Activity/Settings tabs
- visibility/frozen badge
- permission-aware settings

**Allowed scope 上限**
- `cmd/api/**`
- `internal/domain/**`
- `internal/application/**`
- `internal/authz/**`
- `internal/persistence/**`
- `apps/web/**`
- `packages/api-contracts/**`
- `tests/**`

**验收标准**
- [ ] 所有 route 可导航
- [ ] private unauthorized 不渲染 shell

**Required tests**
- project shell e2e

---

### T0109 — Project Settings 与成员管理 UI

**依赖：** T0105, T0108
**最高自主决策级别：** L1

**要求**
- member list/role changes
- visibility setting 仅预览，publishing guard 后续增强
- project purpose/activity status

**Allowed scope 上限**
- `cmd/api/**`
- `internal/domain/**`
- `internal/application/**`
- `internal/authz/**`
- `internal/persistence/**`
- `apps/web/**`
- `packages/api-contracts/**`
- `tests/**`

**验收标准**
- [ ] viewer 无 settings
- [ ] owner/maintainer 动作有 audit placeholder

**Required tests**
- settings e2e

---

### T0110 — 基础 Audit Log

**依赖：** T0105
**最高自主决策级别：** L1

**要求**
- 记录 auth/governance/member/project visibility/action
- append-only repository
- Activity page 基础查询

**Allowed scope 上限**
- `cmd/api/**`
- `internal/domain/**`
- `internal/application/**`
- `internal/authz/**`
- `internal/persistence/**`
- `apps/web/**`
- `packages/api-contracts/**`
- `tests/**`

**验收标准**
- [ ] 高风险动作有 actor/via/request id
- [ ] 无 update/delete audit endpoint

**Required tests**
- audit integration

---

## P2 — RSG 核心与 Scientific Objects

### T0201 — Core Scientific Object Schema registry

**依赖：** T0005, T0105
**最高自主决策级别：** L1

**要求**
- 加载 JSON schemas
- schema id/version registry
- validation service
- 旧版本不被覆盖

**Allowed scope 上限**
- `cmd/api/**`
- `internal/rsg/**`
- `internal/domain/**`
- `internal/application/**`
- `internal/persistence/**`
- `packages/schemas/**`
- `specs/schemas/**`
- `tests/**`

**验收标准**
- [ ] 所有 V1 schema 可 validate
- [ ] 未知 schema 明确失败或 namespaced extension path

**Required tests**
- schema unit

---

### T0202 — Scientific Object immutable version repository

**依赖：** T0201
**最高自主决策级别：** L1

**要求**
- create object + v1
- create next version with expected_version
- 版本不可更新 payload

**Allowed scope 上限**
- `cmd/api/**`
- `internal/rsg/**`
- `internal/domain/**`
- `internal/application/**`
- `internal/persistence/**`
- `packages/schemas/**`
- `specs/schemas/**`
- `tests/**`

**验收标准**
- [ ] 并发 expected version 冲突返回稳定 code
- [ ] 旧版本内容保持不变

**Required tests**
- object repository integration

---

### T0203 — Typed Relation repository

**依赖：** T0202
**最高自主决策级别：** L1

**要求**
- version-pinned source/target
- relation type catalog validation
- relation immutable version

**Allowed scope 上限**
- `cmd/api/**`
- `internal/rsg/**`
- `internal/domain/**`
- `internal/application/**`
- `internal/persistence/**`
- `packages/schemas/**`
- `specs/schemas/**`
- `tests/**`

**验收标准**
- [ ] 禁止引用不存在版本
- [ ] dependency/provenance type 可查询

**Required tests**
- relation integration

---

### T0204 — Project State 与 State Commit

**依赖：** T0202, T0203
**最高自主决策级别：** L1

**要求**
- state snapshots/projections
- state commit actor/via/message/operations
- parent state
- transaction boundary

**Allowed scope 上限**
- `cmd/api/**`
- `internal/rsg/**`
- `internal/domain/**`
- `internal/application/**`
- `internal/persistence/**`
- `packages/schemas/**`
- `specs/schemas/**`
- `tests/**`

**验收标准**
- [ ] 每次 semantic write 形成可追溯 state transition
- [ ] 失败 transaction 不产生半状态

**Required tests**
- state integration

---

### T0205 — Research Branch Domain

**依赖：** T0204
**最高自主决策级别：** L1

**要求**
- branch from base state
- public/private visibility
- branch lifecycle
- current head state

**Allowed scope 上限**
- `cmd/api/**`
- `internal/rsg/**`
- `internal/domain/**`
- `internal/application/**`
- `internal/persistence/**`
- `packages/schemas/**`
- `specs/schemas/**`
- `tests/**`

**验收标准**
- [ ] branch head 可独立演化
- [ ] main 与 branch state 不混淆

**Required tests**
- branch unit/integration

---

### T0206 — RSG Manifest 导出与 hash

**依赖：** T0204, T0208
**最高自主决策级别：** L1

**要求**
- canonical serialization
- stable ordering
- object/relation/blob/git refs
- state hash

**Allowed scope 上限**
- `cmd/api/**`
- `internal/rsg/**`
- `internal/domain/**`
- `internal/application/**`
- `internal/persistence/**`
- `packages/schemas/**`
- `specs/schemas/**`
- `tests/**`

**验收标准**
- [ ] 同一 state 多次导出 hash 相同
- [ ] 任何 semantic change 改变 hash

**Required tests**
- golden manifest tests

---

### T0207 — Progressive Validation Gates

**依赖：** T0201, T0204
**最高自主决策级别：** L1

**要求**
- draft/pr/main/release/asset gate
- required field/provenance/schema checks
- warning vs blocking

**Allowed scope 上限**
- `cmd/api/**`
- `internal/rsg/**`
- `internal/domain/**`
- `internal/application/**`
- `internal/persistence/**`
- `packages/schemas/**`
- `specs/schemas/**`
- `tests/**`

**验收标准**
- [ ] 不同 gate 严格度不同且可解释
- [ ] command 再次 server validate

**Required tests**
- validation unit/integration

---

### T0208 — V1 Scientific Object Domain Services

**依赖：** T0202, T0205, T0207
**最高自主决策级别：** L1

**要求**
- ResearchQuestion/Hypothesis/Material/Sample/Experiment/Calculation/Dataset/Protocol/Claim/Finding/ExternalReference create/version commands
- domain-specific semantic checks

**Allowed scope 上限**
- `cmd/api/**`
- `internal/rsg/**`
- `internal/domain/**`
- `internal/application/**`
- `internal/persistence/**`
- `packages/schemas/**`
- `specs/schemas/**`
- `tests/**`

**验收标准**
- [ ] 每类对象 API+service 可创建版本
- [ ] Claim atomicity只做提示不硬 NLP 判断

**Required tests**
- object type tests

---

### T0209 — RSG Query API

**依赖：** T0203, T0205, T0208
**最高自主决策级别：** L1

**要求**
- 按 type/relation/question/finding/material 查询
- recursive relation traversal 限深
- permission-aware

**Allowed scope 上限**
- `cmd/api/**`
- `internal/rsg/**`
- `internal/domain/**`
- `internal/application/**`
- `internal/persistence/**`
- `packages/schemas/**`
- `specs/schemas/**`
- `tests/**`

**验收标准**
- [ ] 查询可返回 state-specific graph slice
- [ ] private relation 不泄漏

**Required tests**
- rsg query integration

---

### T0210 — Scientific Object Detail UI

**依赖：** T0208, T0108
**最高自主决策级别：** L1

**要求**
- object header/id/version/state
- metadata/relations/history placeholder/files/evidence tabs
- Work with Agent CTA

**Allowed scope 上限**
- `cmd/api/**`
- `internal/rsg/**`
- `internal/domain/**`
- `internal/application/**`
- `internal/persistence/**`
- `packages/schemas/**`
- `specs/schemas/**`
- `tests/**`

**验收标准**
- [ ] 对象版本可切换
- [ ] 无 web scientific edit form

**Required tests**
- object detail e2e

---

### T0211 — Research Outline 与基础 Research 页面

**依赖：** T0209, T0210
**最高自主决策级别：** L1

**要求**
- 按 Research Question/Findings 聚合 outline
- 对象 counts 只作辅助
- list fallback

**Allowed scope 上限**
- `cmd/api/**`
- `internal/rsg/**`
- `internal/domain/**`
- `internal/application/**`
- `internal/persistence/**`
- `packages/schemas/**`
- `specs/schemas/**`
- `tests/**`

**验收标准**
- [ ] 默认不是 files/type dump
- [ ] 可从 question drill-down

**Required tests**
- research page e2e

---

### T0212 — Project Overview Research Summary

**依赖：** T0211
**最高自主决策级别：** L1

**要求**
- key questions/findings/active branches/needs attention/current main
- empty state 引导 agent/project start

**Allowed scope 上限**
- `cmd/api/**`
- `internal/rsg/**`
- `internal/domain/**`
- `internal/application/**`
- `internal/persistence/**`
- `packages/schemas/**`
- `specs/schemas/**`
- `tests/**`

**验收标准**
- [ ] 首屏可理解项目在做什么
- [ ] 不展示文件树为主内容

**Required tests**
- overview e2e

---

### T0213 — Project Schema Extension 与 Custom Metadata

**依赖：** T0201, T0208
**最高自主决策级别：** L1

**要求**
- 允许 Project 注册 namespaced schema profile 扩展官方 base schema
- Custom metadata 永远有逃生口
- schema 版本化且旧 object version 固定 schema ref

**Allowed scope 上限**
- `cmd/api/**`
- `internal/rsg/**`
- `internal/domain/**`
- `internal/application/**`
- `internal/persistence/**`
- `packages/schemas/**`
- `specs/schemas/**`
- `tests/**`

**验收标准**
- [ ] 用户可为 Experiment 添加项目专用字段而不改平台 core
- [ ] schema v2 不使 v1 历史失效

**Required tests**
- schema extension integration

---

### T0214 — 官方材料研发 Project Templates

**依赖：** T0213, T0211
**最高自主决策级别：** L1

**要求**
- 实现 Materials Discovery/Computational Screening/Experimental Validation/Paper Reproduction/Dataset Construction/Benchmarking 模板
- 模板只初始化 project/schema/review/map 默认，不永久控制项目
- 记录 template id/version

**Allowed scope 上限**
- `cmd/api/**`
- `internal/rsg/**`
- `internal/domain/**`
- `internal/application/**`
- `internal/persistence/**`
- `packages/schemas/**`
- `specs/schemas/**`
- `tests/**`

**验收标准**
- [ ] 用户可从模板创建 Project
- [ ] 创建后可独立演化
- [ ] 模板升级不会自动改已有项目

**Required tests**
- template creation e2e

---

## P3 — Git 兼容层与只读 Files

### T0301 — Gitea adapter 与 repo provisioning

**依赖：** T0104, T0003
**最高自主决策级别：** L1

**要求**
- GitPort interface
- project→repo 1:1
- service account
- webhook secret

**Allowed scope 上限**
- `internal/gitprovider/**`
- `cmd/api/**`
- `apps/web/**`
- `infra/gitea/**`
- `tests/**`

**验收标准**
- [ ] 新 Project 可 provision repo
- [ ] domain 不引用 Gitea DB

**Required tests**
- gitea integration

---

### T0302 — Git main 双层保护

**依赖：** T0301, T0205
**最高自主决策级别：** L1

**要求**
- Gitea protected branch
- platform ref guard
- 禁止 force/direct push

**Allowed scope 上限**
- `internal/gitprovider/**`
- `cmd/api/**`
- `apps/web/**`
- `infra/gitea/**`
- `tests/**`

**验收标准**
- [ ] owner 使用 Git 也无法 direct push main
- [ ] 平台 merge service 可受控写

**Required tests**
- git protection e2e

---

### T0303 — Branch Git ref 同步

**依赖：** T0301, T0205
**最高自主决策级别：** L1

**要求**
- Research Branch 创建对应 Git ref
- branch abort/merge ref strategy
- mapping table

**Allowed scope 上限**
- `internal/gitprovider/**`
- `cmd/api/**`
- `apps/web/**`
- `infra/gitea/**`
- `tests/**`

**验收标准**
- [ ] semantic branch 与 Git ref 一致

**Required tests**
- git branch integration

---

### T0304 — Git 用户认证/PAT/SSH key 基础

**依赖：** T0101, T0301
**最高自主决策级别：** L1

**要求**
- scoped git token 或 SSH key
- revocation
- repo access mapping

**Allowed scope 上限**
- `internal/gitprovider/**`
- `cmd/api/**`
- `apps/web/**`
- `infra/gitea/**`
- `tests/**`

**验收标准**
- [ ] 无权限用户 clone private 失败
- [ ] token revoke 生效

**Required tests**
- git auth integration

---

### T0305 — Push Webhook 与 Semantic Ingestion

**依赖：** T0303
**最高自主决策级别：** L1

**要求**
- push event 验签
- inspect changed manifests/files
- 候选 semantic diff
- idempotent duplicate webhook

**Allowed scope 上限**
- `internal/gitprovider/**`
- `cmd/api/**`
- `apps/web/**`
- `infra/gitea/**`
- `tests/**`

**验收标准**
- [ ] branch push 被 ingest
- [ ] 重复 webhook 不重复 state

**Required tests**
- git ingestion integration

---

### T0306 — Unstructured Change 状态

**依赖：** T0305
**最高自主决策级别：** L1

**要求**
- 无法解析文件可保留
- branch semantic completeness flag
- PR gate 阻止不完整 state

**Allowed scope 上限**
- `internal/gitprovider/**`
- `cmd/api/**`
- `apps/web/**`
- `infra/gitea/**`
- `tests/**`

**验收标准**
- [ ] 任意文件 push 不丢失
- [ ] 但不可绕过 semantic validation merge

**Required tests**
- unstructured negative

---

### T0307 — Files Tree/Preview API

**依赖：** T0301, T0304
**最高自主决策级别：** L1

**要求**
- read git tree/file/history
- blob pointer display
- permission filter
- safe preview

**Allowed scope 上限**
- `internal/gitprovider/**`
- `cmd/api/**`
- `apps/web/**`
- `infra/gitea/**`
- `tests/**`

**验收标准**
- [ ] Files API 无 mutation method
- [ ] private blob metadata 不泄漏

**Required tests**
- files api

---

### T0308 — 只读 Files Web UI

**依赖：** T0307, T0108
**最高自主决策级别：** L1

**要求**
- tree/preview/history/download/context sidebar
- 无 edit/upload/delete UI
- raw diff link

**Allowed scope 上限**
- `internal/gitprovider/**`
- `cmd/api/**`
- `apps/web/**`
- `infra/gitea/**`
- `tests/**`

**验收标准**
- [ ] DOM/E2E 断言无 mutation controls
- [ ] 可跳到 Scientific Context

**Required tests**
- files read-only e2e

---

### T0309 — Git ↔ RSG reconciliation

**依赖：** T0305, T0206
**最高自主决策级别：** L1

**要求**
- 定时校验 branch ref/state hash/mapping
- drift alert
- repair proposal 不静默修

**Allowed scope 上限**
- `internal/gitprovider/**`
- `cmd/api/**`
- `apps/web/**`
- `infra/gitea/**`
- `tests/**`

**验收标准**
- [ ] 测试构造 drift 可检测
- [ ] 产生高严重 alert/audit

**Required tests**
- reconciliation integration

---

## P4 — PR、Review 与 Semantic Merge

### T0401 — Research State Diff 引擎

**依赖：** T0206, T0208
**最高自主决策级别：** L1

**要求**
- base/source/target object+relation diff
- scientific summary categories
- file diff refs

**Allowed scope 上限**
- `internal/rsg/**`
- `internal/application/**`
- `internal/domain/**`
- `cmd/api/**`
- `apps/web/**`
- `tests/**`

**验收标准**
- [ ] 能稳定列出 create/update/abort/relation changes
- [ ] golden diff 可重复

**Required tests**
- golden diff

---

### T0402 — Pull Request Domain

**依赖：** T0401, T0207
**最高自主决策级别：** L1

**要求**
- open/close/review state
- base/proposed state fixed
- number per project

**Allowed scope 上限**
- `internal/rsg/**`
- `internal/application/**`
- `internal/domain/**`
- `cmd/api/**`
- `apps/web/**`
- `tests/**`

**验收标准**
- [ ] PR base 不随 main 漂移
- [ ] head update 可显式 refresh

**Required tests**
- pr integration

---

### T0403 — Integrity Review Checks

**依赖：** T0402
**最高自主决策级别：** L1

**要求**
- schema/provenance/dependency/rights/visibility/blob integrity checks
- machine results

**Allowed scope 上限**
- `internal/rsg/**`
- `internal/application/**`
- `internal/domain/**`
- `cmd/api/**`
- `apps/web/**`
- `tests/**`

**验收标准**
- [ ] blocking/warning 区分
- [ ] PR 页面可读

**Required tests**
- integrity tests

---

### T0404 — Scientific Review 模型

**依赖：** T0402, T0105
**最高自主决策级别：** L1

**要求**
- scientific/integrity review 分维度
- reviewer responsibility hook
- changes_requested/approved/comment

**Allowed scope 上限**
- `internal/rsg/**`
- `internal/application/**`
- `internal/domain/**`
- `cmd/api/**`
- `apps/web/**`
- `tests/**`

**验收标准**
- [ ] 一人不同 review kind 可记录
- [ ] 权限正确

**Required tests**
- review tests

---

### T0405 — Semantic Conflict Detector

**依赖：** T0401
**最高自主决策级别：** L1

**要求**
- attribute/identity/relation/schema/knowledge/rights/dependency conflict 分类
- safe changes 标 auto_mergeable

**Allowed scope 上限**
- `internal/rsg/**`
- `internal/application/**`
- `internal/domain/**`
- `cmd/api/**`
- `apps/web/**`
- `tests/**`

**验收标准**
- [ ] Protocol 同字段不同值识别 scientific conflict
- [ ] append evidence 不误判文本 conflict

**Required tests**
- conflict unit

---

### T0406 — Semantic Merge Engine

**依赖：** T0405, T0404
**最高自主决策级别：** L1

**要求**
- three-way merge non-conflicting object/relations
- human resolution plan input
- transaction + Git update saga

**Allowed scope 上限**
- `internal/rsg/**`
- `internal/application/**`
- `internal/domain/**`
- `cmd/api/**`
- `apps/web/**`
- `tests/**`

**验收标准**
- [ ] safe merge 结果 deterministic
- [ ] 科学冲突不自动 winner

**Required tests**
- merge integration

---

### T0407 — Scientific Conflict Resolution UI

**依赖：** T0405, T0108
**最高自主决策级别：** L1

**要求**
- base/A/B/evidence context
- Accept A/B/Keep Both/Create Validation Branch/Unresolved options
- agent explanation 仅建议

**Allowed scope 上限**
- `internal/rsg/**`
- `internal/application/**`
- `internal/domain/**`
- `cmd/api/**`
- `apps/web/**`
- `tests/**`

**验收标准**
- [ ] 用户可明确处理冲突
- [ ] 不存在自动平均参数

**Required tests**
- conflict e2e

---

### T0408 — PR Research Diff UI

**依赖：** T0401, T0403, T0404
**最高自主决策级别：** L1

**要求**
- Summary/Scientific changes/Knowledge changes/Evidence/Checks/Raw Files tabs
- review controls

**Allowed scope 上限**
- `internal/rsg/**`
- `internal/application/**`
- `internal/domain/**`
- `cmd/api/**`
- `apps/web/**`
- `tests/**`

**验收标准**
- [ ] 默认首屏非 raw diff
- [ ] 重要风险明显

**Required tests**
- pr ui e2e

---

### T0409 — Merge Governance 与 frozen main 更新

**依赖：** T0406, T0302
**最高自主决策级别：** L1

**要求**
- 仅 maintainer/owner + reviews/policy pass
- platform service Git merge/ref
- new accepted RSG state

**Allowed scope 上限**
- `internal/rsg/**`
- `internal/application/**`
- `internal/domain/**`
- `cmd/api/**`
- `apps/web/**`
- `tests/**`

**验收标准**
- [ ] main direct write 仍禁止
- [ ] merge event/audit/contribution 产生

**Required tests**
- merge governance e2e

---

### T0410 — PR/Branch 完整 E2E

**依赖：** T0409
**最高自主决策级别：** L1

**要求**
- seed branch create→objects→PR→review→merge
- 另一条 scientific conflict flow

**Allowed scope 上限**
- `internal/rsg/**`
- `internal/application/**`
- `internal/domain/**`
- `cmd/api/**`
- `apps/web/**`
- `tests/**`

**验收标准**
- [ ] 两条 E2E 在 CI 稳定通过

**Required tests**
- playwright pr flows

---

## P5 — Evidence、Knowledge 与 External References

### T0501 — Research Question 与 Hypothesis 关系模型

**依赖：** T0208
**最高自主决策级别：** L1

**要求**
- subquestions
- addresses/tests relations
- hypothesis assessment 非 Issue state

**Allowed scope 上限**
- `internal/evidence/**`
- `internal/rsg/**`
- `internal/domain/**`
- `cmd/api/**`
- `apps/web/**`
- `packages/schemas/**`
- `tests/**`

**验收标准**
- [ ] Issue close 不改变 question state

**Required tests**
- knowledge relation tests

---

### T0502 — Claim 结构与 scope

**依赖：** T0208
**最高自主决策级别：** L1

**要求**
- claim type
- subject/property/value/scope
- causal claim basis field
- assessment

**Allowed scope 上限**
- `internal/evidence/**`
- `internal/rsg/**`
- `internal/domain/**`
- `cmd/api/**`
- `apps/web/**`
- `packages/schemas/**`
- `tests/**`

**验收标准**
- [ ] quantitative scope 可结构化
- [ ] causal 无 basis 产生 validation warning

**Required tests**
- claim unit

---

### T0503 — Finding 聚合模型

**依赖：** T0502
**最高自主决策级别：** L1

**要求**
- Finding 引用固定 Claim versions
- assessment/status
- negative finding

**Allowed scope 上限**
- `internal/evidence/**`
- `internal/rsg/**`
- `internal/domain/**`
- `cmd/api/**`
- `apps/web/**`
- `packages/schemas/**`
- `tests/**`

**验收标准**
- [ ] Claim 后续新版本不静默改变旧 Finding

**Required tests**
- finding tests

---

### T0504 — Evidence Assertion Domain

**依赖：** T0203, T0502
**最高自主决策级别：** L1

**要求**
- 9种 relation
- scope/directness/inference/review
- version pin

**Allowed scope 上限**
- `internal/evidence/**`
- `internal/rsg/**`
- `internal/domain/**`
- `cmd/api/**`
- `apps/web/**`
- `packages/schemas/**`
- `tests/**`

**验收标准**
- [ ] 支持与反驳可同时存在
- [ ] 不生成 Truth Score

**Required tests**
- evidence tests

---

### T0505 — Provenance Graph Projection

**依赖：** T0203, T0208
**最高自主决策级别：** L1

**要求**
- uses/produces/derived/follows etc traversal
- lineage view
- impact hooks

**Allowed scope 上限**
- `internal/evidence/**`
- `internal/rsg/**`
- `internal/domain/**`
- `cmd/api/**`
- `apps/web/**`
- `packages/schemas/**`
- `tests/**`

**验收标准**
- [ ] 可回答 Dataset 从哪来
- [ ] graph list/JSON 输出

**Required tests**
- provenance integration

---

### T0506 — Evidence Graph Projection

**依赖：** T0504
**最高自主决策级别：** L1

**要求**
- Claim/Hypothesis→evidence grouping
- origin/external placeholder
- conflicting evidence

**Allowed scope 上限**
- `internal/evidence/**`
- `internal/rsg/**`
- `internal/domain/**`
- `cmd/api/**`
- `apps/web/**`
- `packages/schemas/**`
- `tests/**`

**验收标准**
- [ ] 可回答为什么相信/质疑
- [ ] 与 provenance API 分开

**Required tests**
- evidence graph integration

---

### T0507 — Evidence/Provenance UI

**依赖：** T0505, T0506, T0210
**最高自主决策级别：** L1

**要求**
- Object detail 两类 graph/tab
- list fallback
- relation semantics label

**Allowed scope 上限**
- `internal/evidence/**`
- `internal/rsg/**`
- `internal/domain/**`
- `cmd/api/**`
- `apps/web/**`
- `packages/schemas/**`
- `tests/**`

**验收标准**
- [ ] 用户不把两图混淆
- [ ] a11y list 可访问

**Required tests**
- graph e2e

---

### T0508 — External Reference live identity + snapshot

**依赖：** T0208
**最高自主决策级别：** L1

**要求**
- external id uniqueness
- snapshot metadata/hash/accessed_at
- manual refresh adapter
- citation/dependency relations

**Allowed scope 上限**
- `internal/evidence/**`
- `internal/rsg/**`
- `internal/domain/**`
- `cmd/api/**`
- `apps/web/**`
- `packages/schemas/**`
- `tests/**`

**验收标准**
- [ ] 旧 snapshot 固定
- [ ] upstream refresh 不改历史

**Required tests**
- external ref tests

---

### T0509 — Literature evidence extraction data model

**依赖：** T0508, T0504
**最高自主决策级别：** L1

**要求**
- specific figure/table/section/location metadata
- human confirmed flag
- snapshot source

**Allowed scope 上限**
- `internal/evidence/**`
- `internal/rsg/**`
- `internal/domain/**`
- `cmd/api/**`
- `apps/web/**`
- `packages/schemas/**`
- `tests/**`

**验收标准**
- [ ] 禁止只有 DOI 就标 supports
- [ ] API validation

**Required tests**
- literature evidence negative

---

### T0510 — Knowledge workflow E2E

**依赖：** T0501, T0503, T0507, T0509
**最高自主决策级别：** L1

**要求**
- Q→H→Exp/Calc/Dataset→Evidence→Claim→Finding
- 含 conflicting evidence

**Allowed scope 上限**
- `internal/evidence/**`
- `internal/rsg/**`
- `internal/domain/**`
- `cmd/api/**`
- `apps/web/**`
- `packages/schemas/**`
- `tests/**`

**验收标准**
- [ ] 完整知识链可浏览、版本化、PR merge

**Required tests**
- knowledge e2e

---

## P6 — Frozen Main、Abort、Policy 与 Release

### T0601 — Freeze Main Governance

**依赖：** T0409
**最高自主决策级别：** L1

**要求**
- project main freeze action
- audit/event
- 新项目可 setup 后 freeze

**Allowed scope 上限**
- `internal/domain/**`
- `internal/application/**`
- `internal/persistence/**`
- `cmd/api/**`
- `apps/web/**`
- `tests/**`

**验收标准**
- [ ] freeze 后 semantic/Git direct write 全拒绝

**Required tests**
- freeze e2e

---

### T0602 — Abort/Reopen State Transition

**依赖：** T0208, T0409
**最高自主决策级别：** L1

**要求**
- branch 中 abort/reopen
- main 对象需 PR
- reason/replacement

**Allowed scope 上限**
- `internal/domain/**`
- `internal/application/**`
- `internal/persistence/**`
- `cmd/api/**`
- `apps/web/**`
- `tests/**`

**验收标准**
- [ ] 历史版本/Release 不变
- [ ] 对象当前状态正确

**Required tests**
- abort tests

---

### T0603 — Organization/Project Policy Engine

**依赖：** T0105, T0208
**最高自主决策级别：** L1

**要求**
- versioned policy
- org lower-bound + project stricter
- policy evaluate interface

**Allowed scope 上限**
- `internal/domain/**`
- `internal/application/**`
- `internal/persistence/**`
- `cmd/api/**`
- `apps/web/**`
- `tests/**`

**验收标准**
- [ ] Project 不可放宽 org rule
- [ ] 旧 policy version 可查询

**Required tests**
- policy tests

---

### T0604 — Scientific Responsibility / Reviewer Routing

**依赖：** T0404, T0603
**最高自主决策级别：** L1

**要求**
- Research Owners rule by object/schema/type
- required review calculation

**Allowed scope 上限**
- `internal/domain/**`
- `internal/application/**`
- `internal/persistence/**`
- `cmd/api/**`
- `apps/web/**`
- `tests/**`

**验收标准**
- [ ] Protocol change 自动要求对应 reviewer
- [ ] 无权限不自动赋权

**Required tests**
- review routing

---

### T0605 — Release Manifest Builder

**依赖：** T0206, T0603
**最高自主决策级别：** L1

**要求**
- accepted main snapshot
- objects/relations/blobs/git/policy/schema/reviews/hash

**Allowed scope 上限**
- `internal/domain/**`
- `internal/application/**`
- `internal/persistence/**`
- `cmd/api/**`
- `apps/web/**`
- `tests/**`

**验收标准**
- [ ] manifest deterministic
- [ ] 包含 pinned policy/schema

**Required tests**
- release golden

---

### T0606 — Immutable Release API/UI

**依赖：** T0605
**最高自主决策级别：** L1

**要求**
- create release governance
- release page
- export manifest
- no edit/delete

**Allowed scope 上限**
- `internal/domain/**`
- `internal/application/**`
- `internal/persistence/**`
- `cmd/api/**`
- `apps/web/**`
- `tests/**`

**验收标准**
- [ ] 同 version 重复 idempotent/conflict
- [ ] 旧 release 不受 current state 改变

**Required tests**
- release e2e

---

### T0607 — Activity/Audit Timeline 增强

**依赖：** T0601, T0602, T0606
**最高自主决策级别：** L1

**要求**
- filter governance/research events
- actor/via links
- abort/reopen/release display

**Allowed scope 上限**
- `internal/domain/**`
- `internal/application/**`
- `internal/persistence/**`
- `cmd/api/**`
- `apps/web/**`
- `tests/**`

**验收标准**
- [ ] 关键历史可追踪

**Required tests**
- activity e2e

---

### T0608 — Release/Abort/Policy E2E

**依赖：** T0606, T0604
**最高自主决策级别：** L1

**要求**
- policy requires reviewers→merge→release→later abort object→old release warning but immutable

**Allowed scope 上限**
- `internal/domain/**`
- `internal/application/**`
- `internal/persistence/**`
- `cmd/api/**`
- `apps/web/**`
- `tests/**`

**验收标准**
- [ ] 链路全通过

**Required tests**
- playwright release flow

---

### T0609 — Project Milestone 基础

**依赖：** T0606
**最高自主决策级别：** L1

**要求**
- Milestone 与 Release 分离
- 支持 Candidate selected/Paper submitted/Patent filed/External validation 等类型和 custom label
- Milestone 可关联 Release 但不要求

**Allowed scope 上限**
- `internal/domain/**`
- `internal/application/**`
- `internal/persistence/**`
- `cmd/api/**`
- `apps/web/**`
- `tests/**`

**验收标准**
- [ ] Project 没有强制 Completed 终态
- [ ] Milestone timeline 正确

**Required tests**
- milestone tests

---

## P7 — Research Asset Hub、Rights 与 Publish

### T0701 — Research Asset Core/PID

**依赖：** T0606
**最高自主决策级别：** L1

**要求**
- asset identity + immutable versions
- 4 types enum
- origin refs
- persistent URLs

**Allowed scope 上限**
- `internal/assets/**`
- `internal/rights/**`
- `cmd/api/**`
- `apps/web/**`
- `packages/schemas/**`
- `tests/**`

**验收标准**
- [ ] asset ID 不随 slug/org 变化

**Required tests**
- asset core

---

### T0702 — 四类 Asset Manifest validator

**依赖：** T0701, T0207
**最高自主决策级别：** L1

**要求**
- Dataset/Protocol/Material Collection/Benchmark required metadata
- asset gate

**Allowed scope 上限**
- `internal/assets/**`
- `internal/rights/**`
- `cmd/api/**`
- `apps/web/**`
- `packages/schemas/**`
- `tests/**`

**验收标准**
- [ ] 缺 provenance/rights/version pin 阻止发布

**Required tests**
- asset validators

---

### T0703 — Rights Model

**依赖：** T0701
**最高自主决策级别：** L1

**要求**
- standard license id
- usage declarations
- custom agreement ref
- metadata vs blob access

**Allowed scope 上限**
- `internal/assets/**`
- `internal/rights/**`
- `cmd/api/**`
- `apps/web/**`
- `packages/schemas/**`
- `tests/**`

**验收标准**
- [ ] 可表达 commercial/derivative/redistribution/model training/attribution
- [ ] UI 不声称法律保证

**Required tests**
- rights tests

---

### T0704 — Publication Impact Preview

**依赖：** T0702, T0703
**最高自主决策级别：** L1

**要求**
- 列将公开对象/metadata/blobs/refs/private dependencies/rights blockers
- 不修改 state

**Allowed scope 上限**
- `internal/assets/**`
- `internal/rights/**`
- `cmd/api/**`
- `apps/web/**`
- `packages/schemas/**`
- `tests/**`

**验收标准**
- [ ] private→public preview 完整且可重复

**Required tests**
- publish preview tests

---

### T0705 — Asset Publish Governance

**依赖：** T0704
**最高自主决策级别：** L1

**要求**
- human maintainer/owner explicit approval
- policy checks
- immutable version + audit/event

**Allowed scope 上限**
- `internal/assets/**`
- `internal/rights/**`
- `cmd/api/**`
- `apps/web/**`
- `packages/schemas/**`
- `tests/**`

**验收标准**
- [ ] Agent token 直接 publish 被拒
- [ ] 重复请求幂等

**Required tests**
- publish security e2e

---

### T0706 — Asset Metadata Revision

**依赖：** T0705
**最高自主决策级别：** L1

**要求**
- description/keywords/contact docs revision independent
- audit

**Allowed scope 上限**
- `internal/assets/**`
- `internal/rights/**`
- `cmd/api/**`
- `apps/web/**`
- `packages/schemas/**`
- `tests/**`

**验收标准**
- [ ] 不改变 scientific asset version/hash

**Required tests**
- asset metadata tests

---

### T0707 — Asset Reference/Dependency

**依赖：** T0705
**最高自主决策级别：** L1

**要求**
- project uses fixed asset version
- citation/reference vs dependency
- visibility_of_usage

**Allowed scope 上限**
- `internal/assets/**`
- `internal/rights/**`
- `cmd/api/**`
- `apps/web/**`
- `packages/schemas/**`
- `tests/**`

**验收标准**
- [ ] upstream current 升级不改变 pinned dependency

**Required tests**
- asset dependency tests

---

### T0708 — Asset Fork/Derive + Lineage

**依赖：** T0707
**最高自主决策级别：** L1

**要求**
- new identity from source version
- lineage graph
- rights check

**Allowed scope 上限**
- `internal/assets/**`
- `internal/rights/**`
- `cmd/api/**`
- `apps/web/**`
- `packages/schemas/**`
- `tests/**`

**验收标准**
- [ ] derived asset 保留 parent version
- [ ] 禁止 rights 不允许的 derive

**Required tests**
- asset lineage

---

### T0709 — Asset Hub Pages/Explore

**依赖：** T0705, T0107
**最高自主决策级别：** L1

**要求**
- asset page versions/origin/rights/lineage/dependencies/public usage
- 4 types filters

**Allowed scope 上限**
- `internal/assets/**`
- `internal/rights/**`
- `cmd/api/**`
- `apps/web/**`
- `packages/schemas/**`
- `tests/**`

**验收标准**
- [ ] 未登录可浏览 public asset
- [ ] restricted blobs 不可下载

**Required tests**
- asset ui e2e

---

### T0710 — Asset 完整 E2E

**依赖：** T0708, T0709
**最高自主决策级别：** L1

**要求**
- Release→Dataset Asset publish→另一 Project reference/depend→fork derived

**Allowed scope 上限**
- `internal/assets/**`
- `internal/rights/**`
- `cmd/api/**`
- `apps/web/**`
- `packages/schemas/**`
- `tests/**`

**验收标准**
- [ ] PID/version/rights/lineage 全正确

**Required tests**
- asset canonical e2e

---

### T0711 — Asset Governance 与 Rights Holder Transfer

**依赖：** T0705
**最高自主决策级别：** L1

**要求**
- 实现 Rights Holder/Custodian/Maintainer/Creator/Contributor 分离
- ownership transfer append-only event
- transfer 不改变 asset id/version/origin/creator

**Allowed scope 上限**
- `internal/assets/**`
- `internal/rights/**`
- `cmd/api/**`
- `apps/web/**`
- `packages/schemas/**`
- `tests/**`

**验收标准**
- [ ] 转移后旧历史可见
- [ ] Creator 不被新 owner 替换

**Required tests**
- asset governance tests

---

## P8 — 开放网络、Fork、贡献与 Research Profile

### T0801 — Public Entity Anonymous Pages

**依赖：** T0606, T0709
**最高自主决策级别：** L1

**要求**
- public project/release/asset/profile/org/knowledge route 无登录可读
- SEO/meta basics

**Allowed scope 上限**
- `internal/contribution/**`
- `internal/application/**`
- `cmd/api/**`
- `apps/web/**`
- `tests/**`

**验收标准**
- [ ] 登录墙不挡 public research
- [ ] private 不索引

**Required tests**
- anonymous e2e

---

### T0802 — Explore 聚合

**依赖：** T0801
**最高自主决策级别：** L1

**要求**
- Projects/Assets/Knowledge/People/Organizations/Open Contributions tabs
- 基于真实 public index

**Allowed scope 上限**
- `internal/contribution/**`
- `internal/application/**`
- `cmd/api/**`
- `apps/web/**`
- `tests/**`

**验收标准**
- [ ] 无 like-based core ranking
- [ ] private 不出现

**Required tests**
- explore e2e

---

### T0803 — Open Contribution Opportunity

**依赖：** T0402, T0501
**最高自主决策级别：** L1

**要求**
- maintainer 将 issue/research need 标 open for contribution
- difficulty/capability metadata
- agent suggestions require approval

**Allowed scope 上限**
- `internal/contribution/**`
- `internal/application/**`
- `cmd/api/**`
- `apps/web/**`
- `tests/**`

**验收标准**
- [ ] Agent 不能自动 publicize opportunity

**Required tests**
- contribution opportunity tests

---

### T0804 — External Fork/Contribution flow

**依赖：** T0803, T0303
**最高自主决策级别：** L1

**要求**
- nonmember fork public project/state allowed according rights
- external PR to upstream
- 权限隔离

**Allowed scope 上限**
- `internal/contribution/**`
- `internal/application/**`
- `cmd/api/**`
- `apps/web/**`
- `tests/**`

**验收标准**
- [ ] 外部用户无需 member 即可贡献
- [ ] fork 不包含 restricted blobs

**Required tests**
- external fork e2e

---

### T0805 — Published Knowledge Object

**依赖：** T0503, T0704
**最高自主决策级别：** L1

**要求**
- Q/H/Claim/Finding explicit Publish to Network
- fixed version/rights/PID-like id
- not auto on main

**Allowed scope 上限**
- `internal/contribution/**`
- `internal/application/**`
- `cmd/api/**`
- `apps/web/**`
- `tests/**`

**验收标准**
- [ ] 进入 main 不等于公开网络
- [ ] publish explicit

**Required tests**
- knowledge publish tests

---

### T0806 — External Evidence Network Aggregation

**依赖：** T0805, T0504
**最高自主决策级别：** L1

**要求**
- 其他 Project evidence 指向 published version
- origin/reviewed/unreviewed 分类
- origin maintainer 无删除权

**Allowed scope 上限**
- `internal/contribution/**`
- `internal/application/**`
- `cmd/api/**`
- `apps/web/**`
- `tests/**`

**验收标准**
- [ ] 反对证据可见
- [ ] private evidence 不泄漏

**Required tests**
- external evidence tests

---

### T0807 — Contribution Ledger projection

**依赖：** T0409, T0705
**最高自主决策级别：** L1

**要求**
- 从 domain events 生成 contribution events
- roles
- accepted/released context
- affiliation at time

**Allowed scope 上限**
- `internal/contribution/**`
- `internal/application/**`
- `cmd/api/**`
- `apps/web/**`
- `tests/**`

**验收标准**
- [ ] 事件 append-only
- [ ] 同一 event 不重复 credit

**Required tests**
- contribution projection

---

### T0808 — Research Profile / Organization Profile

**依赖：** T0807, T0102, T0103
**最高自主决策级别：** L1

**要求**
- 多维贡献/资产/reuse/reproduction/affiliations
- org public research activity
- 无单一 score

**Allowed scope 上限**
- `internal/contribution/**`
- `internal/application/**`
- `cmd/api/**`
- `apps/web/**`
- `tests/**`

**验收标准**
- [ ] 离职后个人历史保留
- [ ] private info 不泄漏

**Required tests**
- profile e2e

---

### T0809 — Credit Attribution/Dispute 基础

**依赖：** T0807
**最高自主决策级别：** L1

**要求**
- creator/major contributor metadata
- dispute open/resolution append-only

**Allowed scope 上限**
- `internal/contribution/**`
- `internal/application/**`
- `cmd/api/**`
- `apps/web/**`
- `tests/**`

**验收标准**
- [ ] 修正不改 ledger 原事件
- [ ] dispute history 可查

**Required tests**
- credit dispute

---

### T0810 — 最小 Open Network 闭环 E2E

**依赖：** T0802, T0804, T0806, T0808
**最高自主决策级别：** L1

**要求**
- public project→discover→fork/contribute→PR merge→profile credit→external evidence

**Allowed scope 上限**
- `internal/contribution/**`
- `internal/application/**`
- `cmd/api/**`
- `apps/web/**`
- `tests/**`

**验收标准**
- [ ] 完整链路 CI 可跑

**Required tests**
- network e2e

---

### T0811 — Discussion 与 Promote to Research Object

**依赖：** T0805, T0803
**最高自主决策级别：** L1

**要求**
- Project/Knowledge/PR 支持 discussion thread
- Discussion 不进入 Evidence/Reputation
- 有权限用户可将讨论提案转 Issue/Hypothesis/External Evidence proposal，并保留原作者 provenance

**Allowed scope 上限**
- `internal/contribution/**`
- `internal/application/**`
- `cmd/api/**`
- `apps/web/**`
- `tests/**`

**验收标准**
- [ ] 普通评论不会改变 scientific state
- [ ] Promote 后 origin discussion/author 可追溯

**Required tests**
- discussion promotion e2e

---

### T0812 — Private Evidence / Public Attestation 基础

**依赖：** T0806, T0703
**最高自主决策级别：** L1

**要求**
- 私有 Project 可对 public Protocol/Claim/Asset 发布 attestation，不暴露底层 private evidence
- 记录 validation type/result/org visibility/internal review
- 明确 attestation != public evidence

**Allowed scope 上限**
- `internal/contribution/**`
- `internal/application/**`
- `cmd/api/**`
- `apps/web/**`
- `tests/**`

**验收标准**
- [ ] 公共页面可显示 attestation 语义但无法反推出 private project/data
- [ ] organization 可匿名或公开按设置

**Required tests**
- attestation privacy e2e

---

## P9 — Search 与 Evidence-backed Research Answer

### T0901 — Search Document Projection

**依赖：** T0805, T0705
**最高自主决策级别：** L1

**要求**
- public/authorized searchable entity projection
- structured fields + text
- incremental outbox updates

**Allowed scope 上限**
- `internal/search/**`
- `cmd/api/**`
- `apps/web/**`
- `tests/**`

**验收标准**
- [ ] private 默认不进 public projection
- [ ] rebuild command

**Required tests**
- search projection

---

### T0902 — Embedding Provider 与 pgvector

**依赖：** T0901
**最高自主决策级别：** L1

**要求**
- provider interface
- batch embedding worker
- deterministic fake in tests
- version metadata

**Allowed scope 上限**
- `internal/search/**`
- `cmd/api/**`
- `apps/web/**`
- `tests/**`

**验收标准**
- [ ] embedding 失败不阻塞 structured search
- [ ] 可重建

**Required tests**
- embedding tests

---

### T0903 — Scientific Query Planner

**依赖：** T0901
**最高自主决策级别：** L1

**要求**
- 自然语言→structured intent/target/property/scope/evidence preference
- provider-agnostic LLM
- schema-constrained output

**Allowed scope 上限**
- `internal/search/**`
- `cmd/api/**`
- `apps/web/**`
- `tests/**`

**验收标准**
- [ ] invalid plan fallback
- [ ] planner 不访问 private unauthorized entities

**Required tests**
- planner contract

---

### T0904 — Hybrid Retrieval + Graph Expansion

**依赖：** T0902, T0903
**最高自主决策级别：** L1

**要求**
- FTS+vector+structured filter+relation traversal
- version-pinned candidates

**Allowed scope 上限**
- `internal/search/**`
- `cmd/api/**`
- `apps/web/**`
- `tests/**`

**验收标准**
- [ ] Seed queries recall relevant Claims/Materials/Findings
- [ ] 无 private leak

**Required tests**
- retrieval integration

---

### T0905 — Scientific Ranking

**依赖：** T0904
**最高自主决策级别：** L1

**要求**
- scope match/evidence/review/reproduction/conflict/version factors
- 透明 explanation fields

**Allowed scope 上限**
- `internal/search/**`
- `cmd/api/**`
- `apps/web/**`
- `tests/**`

**验收标准**
- [ ] 不按 star/prestige 主排序
- [ ] ranking deterministic fixture

**Required tests**
- ranking golden

---

### T0906 — Evidence-backed Answer Generator/API

**依赖：** T0905
**最高自主决策级别：** L1

**要求**
- 只能引用 retrieval 返回 entity version refs
- 回答含 limitations/conflicts/sources
- structured fallback

**Allowed scope 上限**
- `internal/search/**`
- `cmd/api/**`
- `apps/web/**`
- `tests/**`

**验收标准**
- [ ] 构造 hallucination test：模型不可引用不存在 id
- [ ] source click 可定位

**Required tests**
- answer grounding tests

---

### T0907 — Search Answer Web UI

**依赖：** T0906
**最高自主决策级别：** L1

**要求**
- answer/comparison/evidence map/limitations/sources/underlying results/start project
- loading/fallback/error

**Allowed scope 上限**
- `internal/search/**`
- `cmd/api/**`
- `apps/web/**`
- `tests/**`

**验收标准**
- [ ] 不是 Google list 首屏
- [ ] 可直接探索 source

**Required tests**
- search ui e2e

---

### T0908 — Search → Draft Research Context

**依赖：** T0907, T0104
**最高自主决策级别：** L1

**要求**
- 用户选择 references/dependencies/candidates/hypotheses
- 先 draft 确认
- 生成 initial project state

**Allowed scope 上限**
- `internal/search/**`
- `cmd/api/**`
- `apps/web/**`
- `tests/**`

**验收标准**
- [ ] Agent answer 不自动写 main
- [ ] 确认后 state 可追踪

**Required tests**
- start project e2e

---

## P10 — Research Events、订阅与 Impact Analysis

### T1001 — Transactional Outbox

**依赖：** T0204, T0208
**最高自主决策级别：** L1

**要求**
- domain transaction 写 outbox
- worker publish research_events
- idempotent

**Allowed scope 上限**
- `internal/events/**`
- `internal/application/**`
- `cmd/worker/**`
- `cmd/api/**`
- `apps/web/**`
- `tests/**`

**验收标准**
- [ ] crash/retry 不丢事件不重复副作用

**Required tests**
- outbox integration

---

### T1002 — Subscription Model / Follow/Watch

**依赖：** T1001, T0801
**最高自主决策级别：** L1

**要求**
- target types + event filters + channels
- public/private auth

**Allowed scope 上限**
- `internal/events/**`
- `internal/application/**`
- `cmd/worker/**`
- `cmd/api/**`
- `apps/web/**`
- `tests/**`

**验收标准**
- [ ] 取消订阅生效
- [ ] private target 权限变化后不继续泄漏

**Required tests**
- subscription tests

---

### T1003 — Web Research Inbox

**依赖：** T1002
**最高自主决策级别：** L1

**要求**
- 聚合 meaningful events
- read/unread
- deep links

**Allowed scope 上限**
- `internal/events/**`
- `internal/application/**`
- `cmd/worker/**`
- `cmd/api/**`
- `apps/web/**`
- `tests/**`

**验收标准**
- [ ] 低级每 commit 不默认轰炸用户

**Required tests**
- inbox e2e

---

### T1004 — RSS/Atom Feeds

**依赖：** T1002
**最高自主决策级别：** L1

**要求**
- public Project/Asset/Knowledge feeds
- stable ids/version links

**Allowed scope 上限**
- `internal/events/**`
- `internal/application/**`
- `cmd/worker/**`
- `cmd/api/**`
- `apps/web/**`
- `tests/**`

**验收标准**
- [ ] 未登录可订阅 public feed
- [ ] private 无 feed

**Required tests**
- feed tests

---

### T1005 — Email Digest abstraction

**依赖：** T1002
**最高自主决策级别：** L1

**要求**
- immediate/daily/weekly preference
- dev mail sink
- templates concise

**Allowed scope 上限**
- `internal/events/**`
- `internal/application/**`
- `cmd/worker/**`
- `cmd/api/**`
- `apps/web/**`
- `tests/**`

**验收标准**
- [ ] 测试邮件不含 unauthorized private content

**Required tests**
- email tests

---

### T1006 — Signed Webhooks

**依赖：** T1001
**最高自主决策级别：** L1

**要求**
- endpoint/secret
- HMAC timestamp
- retry/delivery log
- disable failing endpoint policy

**Allowed scope 上限**
- `internal/events/**`
- `internal/application/**`
- `cmd/worker/**`
- `cmd/api/**`
- `apps/web/**`
- `tests/**`

**验收标准**
- [ ] 重放/坏签名测试
- [ ] duplicate event consumer safe

**Required tests**
- webhook security

---

### T1007 — Dependency Impact Analysis

**依赖：** T0707, T1001
**最高自主决策级别：** L1

**要求**
- 沿 depends_on/provenance 下游分析
- direct/indirect affected
- review required alerts
- 不自动 invalidate

**Allowed scope 上限**
- `internal/events/**`
- `internal/application/**`
- `cmd/worker/**`
- `cmd/api/**`
- `apps/web/**`
- `tests/**`

**验收标准**
- [ ] Protocol abort seed 可列受影响 Experiments/Datasets/Claims/Assets

**Required tests**
- impact golden/e2e

---

## P11 — UI、安全、可访问性、性能与运维加固

### T1101 — 统一 Primer-style Design System

**依赖：** T0107, T0809, T0907
**最高自主决策级别：** L1

**要求**
- packages/ui 封装 tokens/components
- StateLabel/Table/Timeline/Diff/Sidebar
- 去除不一致自定义样式

**Allowed scope 上限**
- `apps/web/**`
- `internal/**`
- `cmd/**`
- `infra/**`
- `tests/**`
- `ops/**`

**验收标准**
- [ ] 全站无 gradient/glass
- [ ] visual regression 核心页

**Required tests**
- visual regression

---

### T1102 — Research Map 高质量交互

**依赖：** T0211, T0507
**最高自主决策级别：** L1

**要求**
- 聚合图 By Question/By Finding
- drill-down
- keyboard/list fallback
- 大节点数聚合

**Allowed scope 上限**
- `apps/web/**`
- `internal/**`
- `cmd/**`
- `infra/**`
- `tests/**`
- `ops/**`

**验收标准**
- [ ] Seed 项目不成蜘蛛网
- [ ] 1000 node underlying state 仍默认粗粒度

**Required tests**
- research map e2e/perf

---

### T1103 — Publication/Visibility Security UX

**依赖：** T0704, T1101
**最高自主决策级别：** L1

**要求**
- impact preview UI
- double confirmation for visibility expansion
- rights blockers
- private dependency attestation display

**Allowed scope 上限**
- `apps/web/**`
- `internal/**`
- `cmd/**`
- `infra/**`
- `tests/**`
- `ops/**`

**验收标准**
- [ ] 不能一键 generic modal 发布敏感 state

**Required tests**
- publish ux e2e

---

### T1104 — 全站 Accessibility AA

**依赖：** T1101, T1102
**最高自主决策级别：** L1

**要求**
- axe checks
- keyboard
- labels/aria
- contrast
- reduced motion

**Allowed scope 上限**
- `apps/web/**`
- `internal/**`
- `cmd/**`
- `infra/**`
- `tests/**`
- `ops/**`

**验收标准**
- [ ] 核心页面 automated a11y 无 serious/critical
- [ ] 手动键盘 smoke 文档

**Required tests**
- a11y suite

---

### T1105 — I18N 基线

**依赖：** T1101
**最高自主决策级别：** L1

**要求**
- UI strings resource files
- zh-CN/en
- locale routes/preference
- domain enums stable codes

**Allowed scope 上限**
- `apps/web/**`
- `internal/**`
- `cmd/**`
- `infra/**`
- `tests/**`
- `ops/**`

**验收标准**
- [ ] 核心页面可切语言
- [ ] 科学值/单位不被错误本地化

**Required tests**
- i18n e2e

---

### T1106 — API/Upload 安全加固

**依赖：** T0307, T0705
**最高自主决策级别：** L1

**要求**
- rate limit
- CSP/CSRF
- signed upload TTL
- preview sanitization
- SSRF guard external refs

**Allowed scope 上限**
- `apps/web/**`
- `internal/**`
- `cmd/**`
- `infra/**`
- `tests/**`
- `ops/**`

**验收标准**
- [ ] OWASP smoke
- [ ] 恶意 svg/html 不执行
- [ ] URL SSRF negative

**Required tests**
- security tests

---

### T1107 — 权限与 Search Side-channel 安全回归

**依赖：** T0906, T0806
**最高自主决策级别：** L1

**要求**
- anonymous/auth role matrix E2E
- 计数/error/timing 不明显泄漏 private id
- search post-filter guaranteed

**Allowed scope 上限**
- `apps/web/**`
- `internal/**`
- `cmd/**`
- `infra/**`
- `tests/**`
- `ops/**`

**验收标准**
- [ ] permission negative suite 全过

**Required tests**
- privacy suite

---

### T1108 — 性能基线与索引调优

**依赖：** T0907, T1102
**最高自主决策级别：** L1

**要求**
- 生成 100 projects/100k entities benchmark seed
- 查询/graph/search profiling
- 必要 indexes

**Allowed scope 上限**
- `apps/web/**`
- `internal/**`
- `cmd/**`
- `infra/**`
- `tests/**`
- `ops/**`

**验收标准**
- [ ] 满足 27_PERFORMANCE_SLO 开发目标或记录可接受偏差

**Required tests**
- perf benchmark

---

### T1109 — 生产级 Observability/Dashboards

**依赖：** T1007, T0007
**最高自主决策级别：** L1

**要求**
- metrics/traces/log dashboards
- outbox/queue/git drift/visibility audit alerts

**Allowed scope 上限**
- `apps/web/**`
- `internal/**`
- `cmd/**`
- `infra/**`
- `tests/**`
- `ops/**`

**验收标准**
- [ ] 故障注入可触发预期 alert

**Required tests**
- observability smoke

---

### T1110 — Backup/Restore 自动化演练

**依赖：** T0606, T0705
**最高自主决策级别：** L1

**要求**
- Postgres/S3/Gitea backup scripts
- 空环境 restore
- reconciliation

**Allowed scope 上限**
- `apps/web/**`
- `internal/**`
- `cmd/**`
- `infra/**`
- `tests/**`
- `ops/**`

**验收标准**
- [ ] Seed Project/Release/Asset restore 后 hash/refs 正确

**Required tests**
- restore drill

---

## P12 — Canonical Workflow 与最终交付

### T1201 — 完整 Seed Demo Data Builder

**依赖：** T0810, T1007
**最高自主决策级别：** L1

**要求**
- 按 34_SEED_DEMO_PROJECT 自动构建 demo
- synthetic 标记
- idempotent reset

**Allowed scope 上限**
- `tests/acceptance/**`
- `tests/e2e/**`
- `ops/**`
- `docs/**`
- `examples/**`

**验收标准**
- [ ] 一条命令生成完整 demo 网络

**Required tests**
- seed builder

---

### T1202 — Canonical MOF Workflow E2E

**依赖：** T1201, T0908, T1007
**最高自主决策级别：** L1

**要求**
- 从 Q 到 H/branch/exp/calc/evidence/claim/finding/PR/main/release/assets/external project evidence/profile credit
- 真实 DB/Git/S3/浏览器

**Allowed scope 上限**
- `tests/acceptance/**`
- `tests/e2e/**`
- `ops/**`
- `docs/**`
- `examples/**`

**验收标准**
- [ ] Master Gate I 全通过
- [ ] 测试失败可定位 step

**Required tests**
- canonical playwright

---

### T1203 — Staging 部署模板

**依赖：** T1110
**最高自主决策级别：** L1

**要求**
- Docker production compose 或 k8s manifests
- secrets placeholders
- TLS/reverse proxy notes
- healthchecks

**Allowed scope 上限**
- `tests/acceptance/**`
- `tests/e2e/**`
- `ops/**`
- `docs/**`
- `examples/**`

**验收标准**
- [ ] 干净 staging 可按 runbook 部署

**Required tests**
- deployment smoke

---

### T1204 — 生产 Runbook/Release/Recovery 验证

**依赖：** T1203
**最高自主决策级别：** L1

**要求**
- 执行 deployment/release/backup runbook dry-run
- rollback/forward fix scenario

**Allowed scope 上限**
- `tests/acceptance/**`
- `tests/e2e/**`
- `ops/**`
- `docs/**`
- `examples/**`

**验收标准**
- [ ] 文档与实际命令一致

**Required tests**
- runbook drill

---

### T1205 — OpenAPI/MCP/Schema 文档最终同步

**依赖：** T1202
**最高自主决策级别：** L1

**要求**
- 生成 API docs/SDK
- MCP tools 与实现一致
- schema examples

**Allowed scope 上限**
- `tests/acceptance/**`
- `tests/e2e/**`
- `ops/**`
- `docs/**`
- `examples/**`

**验收标准**
- [ ] contract tests 通过
- [ ] 无 undocumented public endpoint

**Required tests**
- contract suite

---

### T1206 — Master Security/Quality Gate

**依赖：** T1202, T1204, T1205
**最高自主决策级别：** L1

**要求**
- 全部 blocking tests
- dependency/secret/container scan
- a11y/perf/privacy

**Allowed scope 上限**
- `tests/acceptance/**`
- `tests/e2e/**`
- `ops/**`
- `docs/**`
- `examples/**`

**验收标准**
- [ ] Critical/High=0
- [ ] tests.json blocking 全 passed

**Required tests**
- master suite

---

### T1207 — V1 最终验收与交付报告

**依赖：** T1206
**最高自主决策级别：** L1

**要求**
- 逐条核对 31_MASTER_ACCEPTANCE
- 输出 build/version/commit/test evidence
- remaining nonblocking risks

**Allowed scope 上限**
- `tests/acceptance/**`
- `tests/e2e/**`
- `ops/**`
- `docs/**`
- `examples/**`

**验收标准**
- [ ] 所有 V1 gate checked
- [ ] task_status 所有 required done
- [ ] progress 标记 completed

**Required tests**
- final audit

---

### T1208 — 完整 Project 可移植导出

**依赖：** T1202, T1007, T0812
**最高自主决策级别：** L1

**要求**
- 导出 Git bundle/repository refs、RSG manifests、Scientific Object/relations、Issues/PR/Reviews、Releases/Assets metadata、Contribution/Audit export、可访问 blobs 清单/打包选项
- 格式文档化且不依赖内部 DB 主键才能理解
- Restricted/private 导出仅授权 owner

**Allowed scope 上限**
- `tests/acceptance/**`
- `tests/e2e/**`
- `ops/**`
- `docs/**`
- `examples/**`

**验收标准**
- [ ] 导出包可在离线 validator 中校验 hash/refs
- [ ] Seed Project 导出后关键对象/历史计数一致

**Required tests**
- project export portability e2e

---
