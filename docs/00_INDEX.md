# 文档索引

本目录按“产品 → 领域 → 交互 → 技术 → 交付”组织。Claude Code 不应随机阅读；首次启动按下列顺序。

## A. 产品与边界

- `01_PRODUCT_VISION.md`：愿景、非目标、核心原则。
- `02_V1_SCOPE.md`：V1 必做、明确不做、扩展预留。
- `03_GLOSSARY_DOMAIN_MODEL.md`：术语与领域对象定义。
- `04_USERS_ROLES.md`：用户类型、权限角色、科学责任、贡献角色。
- `05_INFORMATION_ARCHITECTURE.md`：全站 IA 与导航。
- `06_UI_UX_SPEC.md`：GitHub 风格视觉和交互规范。

## B. 科研领域模型

- `07_RSG_SPEC.md`：Research State Graph canonical model。
- `08_SCIENTIFIC_OBJECTS.md`：V1 Scientific Objects。
- `09_VERSION_CONTROL.md`：Branch/Commit/PR/Main/Merge/Conflict。
- `10_EVIDENCE_PROVENANCE.md`：Provenance 与 Evidence Graph。
- `11_RELEASE_ASSET_HUB.md`：Release、Research Asset、Asset Hub。
- `12_PERMISSIONS_RIGHTS_POLICY.md`：Public/Private、Rights、Org Policy。
- `13_CONTRIBUTION_REPUTATION.md`：Contribution ledger、credit、reputation。
- `14_SEARCH_DISCOVERY.md`：Search Answer、Explore、Research Map。
- `15_AGENT_MCP_API.md`：Claude Code-style Agent 边界与 MCP/API。
- `16_GIT_COMPATIBILITY.md`：Git CLI/Gitea 兼容层。
- `17_FILES_STORAGE.md`：只读 Files、Blob、对象存储。
- `18_EVENTS_SUBSCRIPTIONS.md`：Research Event、RSS/Email/Webhook。
- `19_EXTERNAL_REFERENCES.md`：DOI/专利/数据库 live identity + snapshot。

## C. 工程实现

- `20_TECH_ARCHITECTURE.md`
- `21_DATA_MODEL.md`
- `22_API_DESIGN.md`
- `23_SECURITY_PRIVACY.md`
- `24_TEST_STRATEGY.md`
- `25_CICD_DEVOPS.md`
- `26_OBSERVABILITY.md`
- `27_PERFORMANCE_SLO.md`
- `28_ACCESSIBILITY_I18N.md`

## D. 路线、验收与运营

- `29_ROADMAP.md`
- `30_TASK_EXECUTION_STANDARD.md`
- `31_MASTER_ACCEPTANCE.md`
- `32_RISK_REGISTER.md`
- `33_ADR_GUIDE.md`
- `34_SEED_DEMO_PROJECT.md`
- `35_DEPLOYMENT_RUNBOOK.md`
- `36_RELEASE_RUNBOOK.md`
- `37_BACKUP_DR.md`
- `38_LEGAL_BOUNDARY.md`
- `39_FUTURE_EXTENSIONS.md`
- `40_OPEN_SOURCE_LICENSES.md`
- `41_DESIGN_TOKENS.md`
- `42_PAGE_SPECS.md`
- `43_STATE_MACHINES.md`
- `44_SCHEMA_RELATION_CATALOG.md`
- `45_ERROR_MODEL.md`
- `46_ABORT_RETENTION.md`
- `47_MCP_TOOL_CATALOG.md`
- `48_DEFINITION_OF_DONE.md`
- `49_AUTONOMOUS_HANDOFF.md`

## 机器可读规格

以 `specs/` 为准；若 Markdown 与 JSON Schema/OpenAPI 冲突，先停止相关实现并创建 ADR/decision，不得擅自猜测。

## E. 自主开发执行与工程标准

- `50_CODING_STANDARD.md`：Go/TypeScript/Python 通用编码规范。
- `51_FRONTEND_STANDARD.md`：Next.js/Primer 前端标准。
- `52_BACKEND_STANDARD.md`：Go 后端边界与规则。
- `53_DATABASE_STANDARD.md`：PostgreSQL/RSG 数据库规则。
- `54_SECURITY_THREAT_MODEL.md`：威胁模型。
- `55_DATA_CLASSIFICATION.md`：数据分级。
- `56_REQUIREMENTS_TRACEABILITY.md`：需求追踪。
- `57_DEMO_UAT.md`：Demo/UAT。
- `58_CHANGE_MANAGEMENT.md`：变更治理。
- `59_PRODUCT_ANALYTICS.md`：产品分析。
- `60_AGENT_SAFETY_BOUNDARIES.md`：Agent 安全边界。
- `61_DEVELOPMENT_ORCHESTRATION.md`：Supervisor / Independent Worker / rddev。
- `62_SUPERVISOR_GOVERNANCE.md`：长期自主开发治理与决策分级。
- `63_WORKER_CONTRACT.md`：Worker 输入、权限、RESULT 契约。
- `64_UBUNTU_DEV_ENV.md`：Ubuntu 24.04 硬件/软件标准。
- `65_MONOREPO_STRUCTURE.md`：Go + Next.js + Python Adapter Monorepo。
- `66_LOCAL_DEV_ENVIRONMENT.md`：本地 Docker/host-native/并行隔离。
- `67_TEST_GATES.md`：G1–G4 四层 Gate。
- `68_TECH_STACK_DECISIONS.md`：已锁定技术栈摘要。

`specs/orchestrator/` 为 `rddev`/Worker 机器可读协议。

- `69_GITHUB_SOURCE_REPOSITORY.md`：POST 源码 GitHub repository、Supervisor control plane 与 Worker credential boundary。
