# 开发进度

状态：P0 进行中。
最后更新：2026-09-12（T0000 merged；自主推进授权生效）

## 当前阶段

P0 — Canonical 开发环境、Orchestrator 与工程脚手架（T0000–T0012）。

## 治理状态（★ 影响每次调度）

owner 于 2026-09-12 授予**默认自主推进**授权（`L3-20260912-4`，持久化于 `CLAUDE.md` §5.1、
`docs/62` §3.1、`docs/69` §3.1）：

- L0/L1 常规实现任务与产品代码 PR，满足六项条件即由 Supervisor 自行 review + merge，
  **不再逐 PR 请求人工批准**。
- 仅 L3、重大 L2、新外部凭证/付费服务/账号授权、无法用规格解决的 `SPEC_BLOCKED` 才停止请求人工。
- 授权不降低 Gate 标准；仅对本仓库 `main` 有效；每次 merge 仍须留完整可追溯链。

## 已完成

- **Supervisor bootstrap 前置校验**
  - `python3 scripts/validate_specs.py` → OK：131 tasks、依赖无 orphan、无环、JSON specs 合法。
  - Canonical remote 校验：`origin` normalized = `lichman0405/post`，default branch = `main`。
  - 环境 preflight：Ubuntu 24.04.5 LTS amd64 / 16 cores / 46 GB RAM / 63 GB swap / 194 GiB free；
    Go 1.27.1、Node 24.21.0、pnpm 12.4.1、Python 3.12.7、uv 0.7.9、Docker 29.8.0、claude 2.1.269。
  - Repository visibility：实测 `PRIVATE`，与规格记录的生成期基线 `public` 不一致 → `SPEC_BLOCKED`
    → owner 裁定 PRIVATE 是有意的 → 解除。见 `tasks/decisions.md` L3-20260912-1。
- **Worker 隔离层**：建立并加固（credential 剥离 + permission deny + PreToolUse hook + HEAD/scope
  校验）。发现并修复 `Read`/`Grep`/`Glob` 绕过 guard 的真实漏洞。回归 25/25 通过。
  见 `tasks/decisions.md` L1-20260912-3 与 `tasks/results/T0011/isolation-smoke/`。
- **T0000 完成并通过 G2**（`accepted`，PR #3 待 owner review）
  - 交付：`ops/doctor.sh`（718 行，35 项检查）、`ops/doctor-checks.md`（Go 移植契约）、
    `ops/doctor-output.schema.json`、两个测试脚本（19 例 / 84 assertion + 真实主机 smoke）、
    `docs/64` 阈值与漂移策略。
  - G2：Supervisor 独立重跑两个 required test 均 PASS；独立复现确定性、缺失工具负路径、
    Windows Native 负路径；scope/HEAD/refs/RESULT schema 全部通过。
  - 汇总：Worker→G2→commit→PR 的完整闭环已经跑通一次。

## 已完成任务

| Task | 状态 | PR | 交付 |
|---|---|---|---|
| T0000 | merged | #3 | `ops/doctor.sh` 35 项确定性 preflight + 检查契约 + 2 套测试 |
| T0001 | merged | #6 | 12 项 spec 校验 + source-repo preflight（三态 visibility、fail-closed）+ 派生版本标记 + CI 强制 |
| T0002 | merged | #10 | Monorepo：Go module 4 binary + Next.js 16.3.5 + uv Python adapter + Makefile，无 TS backend |
| T0003 | merged | #13 | Docker Compose 基设：pgvector/Redis/MinIO/Gitea/Mailpit，全 pinned + healthcheck + 幂等 init |
| T0004 | merged | #16 | 配置与密钥基线：Go typed loader（stdlib）+ web/python 独立校验 + 分层 + RedactURL + secret scan |
| T0005 | merged | #19 | 13 个 forward-only migration + pgx/sqlc + tx helper + run-scoped 测试库命名空间 |
| T0009 | merged | #23 | `rddev` CLI：doctor 复刻 35 项契约、任务状态机、原子+加锁状态写入、诚实 stub |
| T0013 | merged | #28 #29 | append-only 不可变性：13 表 × (BEFORE UPDATE/DELETE 行级 + BEFORE TRUNCATE 语句级) 触发器 |
| T0006 | merged | #32 | 健康检查（/healthz 200、/readyz 503 如实）+ 三处接线修复 + Docker-free `make smoke` |

**T0006 第一次派发为 `worker_failed`**（预算耗尽，见 `decisions.md` L1-20260912-22）：Worker 在即将写
RESULT.json 时用尽 $15 上限，导致证据全失；已在**同一 worktree** 续做并成功。同时记录了我自己的
scope glob 错误（`internal/config/wiring*` 不匹配任何真实文件），制造出 3 条本不该存在的"越界"。
| 安全修复 | merged | #8 #11 #14 #18 | preflight 凭据泄漏/fail-open；schema 注解；infra init token/SQL；config 三条泄漏路径 |

## ★ SPEC_BLOCKED（待 owner 裁定）

**版本历史在存储层可变**（`decisions.md` L2-SPEC-20260912-15）。实测：对
`scientific_object_versions` 已提交行执行 `UPDATE` **成功**（`UPDATE 1`）。`ON DELETE RESTRICT`
只防删除、不防修改。

- **不是 T0005 的缺陷**：canonical `specs/database/postgres.sql` 未声明任何不可变性机制，T0005
  的任务是忠实移植，已做到并独立复核通过。
- **但与 Master Gate A「Object/relation 版本不可改写历史」、`CLAUDE.md` §9 不变量 5/8、
  ADR-007 冲突**。
- 如何在存储层强制属 L2/L3（存储边界 + 科研完整性），three 个可选实现各有权衡，Supervisor 不
  自行决定。**不影响其他任务继续执行**（T0006/T0009 与其无关）。
- V1 Master Gate A 在此项解决前不应判为满足。
| 安全修复 | merged | #8 #11 #14 | preflight 凭据泄漏 + fail-open 分支门；schema 注解；infra init token/SQL/环境变量 |

## 进行中

- **T0002 运行中** — Monorepo 初始化（Go module + Next.js + Python adapter + Makefile）。
  branch `task/T0002-monorepo-init`，baseline `7c8a9d2`。
- **T0001 已 merged**（PR #6，`7c8a9d2`）。交付：`scripts/validate_specs.py` 扩到 12 项检查、
  `scripts/source_repo_preflight.py`（三方 visibility 模型 + SPEC_BLOCKED 拒绝语义）、
  `scripts/spec_version.py` + `specs/SPEC_VERSION.json` 派生版本标记、两个测试脚本
  （57 + 17 assertions）、CI 实际执行新检查。
  G2 独立复现：visibility verdict matrix（match=bless / mismatch=SPEC_BLOCKED / unknown=拒绝 /
  无输入=拒绝）、真实 gh probe = bless、12/12 检查通过。

## 阻塞

- 无。

## Harness 修复（本轮）

T0001 Worker 报告了两个由我引入的隔离缺陷（见 `decisions.md` L1-20260912-9）：
`RESULT.json` 契约路径被自己的 Read-deny 封死；`git remote` deny 与 Worker 契约矛盾。
另有一处 spawn.sh registry 写入在多行值上崩溃。三者已修复，guard 回归 20/20。

## 安全修复（本轮）

自动 commit security review 报出 T0001 preflight 的两个真实缺陷（`decisions.md` L1-20260912-10）：
remote URL 内嵌 token 会被原样写进输出/CI 日志；`BRANCH-DEFAULT` 在无法验证时 fail-open。
两者均已修复并补回归测试，PR #8（`862b78b`）。真实 operator 路径现在会同时验证
visibility 与 integration branch。

**已关闭的 T0000 follow-up**：`ops/doctor.sh --check-docker-daemon` 已在 Supervisor 上下文
真实执行 —— daemon reachable、data-root 130 GiB free、verdict ok。该检查此前仅有 fixture 覆盖。

## 已知风险 / 需 owner 关注

- **Branch protection 不可用**：私有仓库在当前 GitHub plan 下无法启用 branch protection
  （403 要求 Pro 或 public）。`docs/69` §6 的目标规则目前只能靠 Supervisor 纪律保证。
  见 `tasks/decisions.md` F-20260912-1。
- **`bubblewrap` 缺失**（advisory warn）：若启用 Claude Code sandbox 需安装（`ops/bootstrap-ubuntu.sh`）。
- **Docker daemon 探测未真实执行**：`ops/doctor.sh --check-docker-daemon` 仅经 fixture 测试，
  需在 Supervisor/operator 上下文跑一次。
- **Worker 长思考特征**：`effort=max` 下 Worker 会先做 20–30 分钟设计再动手（见 L1-20260912-7）。
  目前不影响正确性，但会影响长链路吞吐；若 P0 后半段成为瓶颈需重新评估 effort。
- **`GITHUB_PERSONAL_ACCESS_TOKEN`** 存在于 Supervisor 环境，Worker spawn 时已剥离；
  任何绕过 `spawn.sh` 的调度方式都会破坏该隔离。

## 下一步

1. owner review 并 approve PR #3 → Supervisor squash-merge → `task_status.T0000 = merged`。
2. 派发 **T0001**（规格仓库与任务依赖图校验脚本化 + source-repository preflight）。
3. T0001 合并后派发 **T0002**（Monorepo 初始化：Go module / Next.js / Python adapter / Makefile）。
4. T0002 完成后进入并行区：T0003 / T0004 / T0005 / T0009 具备并行条件，按写冲突面选择
   （默认并行 3，硬上限 4）。
