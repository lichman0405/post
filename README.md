# POST V1 — Claude Code Autonomous Development Specification v0.3

**POST — Platform for Open Science & Technology** 是一个以版本化科研状态、Research Asset 与可验证贡献为基础的 Open R&D Network。

本规格包绑定到真实源码仓库 `lichman0405/post`，目标不是让 Claude Code“参考一下文档”，而是让一个长期 Supervisor Claude Code 从零开始，持续调度独立完整 Claude Code Workers，直到 V1 Master Acceptance 通过。

## v0.3 的关键变化

- 正式产品名：**POST — Platform for Open Science & Technology**。
- Canonical source repository：`https://github.com/lichman0405/post.git`。
- 源码 GitHub 与产品运行时 Gitea 明确分离：GitHub 管 POST 自身开发；Gitea 是 POST 产品内部 Git infrastructure。
- Supervisor 独占 `lichman0405/post` 的 Issue/Branch/Commit/PR/Merge/Release control plane。
- Worker 不得获得 GitHub write credentials；`rddev` 必须以环境、权限与 HEAD/ref 校验做硬隔离。
- 增加真实 repository machine spec、GitHub PR/Issue 模板、CODEOWNERS、spec-validation workflow、LF 规范和 `.gitignore`。
- 首次 push 前必须重新校验 GitHub repository visibility。生成本包时该仓库为 **Public 且为空**；此状态不得被视为永久事实。

## 已锁定技术/开发基线

- Core Backend：Go 1.27.x
- Web：Next.js 16 Active LTS + React + TypeScript + GitHub Primer/Octicons
- Scientific Adapter：Python 3.12+
- Canonical development environment：Ubuntu 24.04 LTS amd64
- Runtime Git infrastructure：内部 Gitea，不 fork、不暴露 UI
- PostgreSQL 为科研语义 canonical store；Git 管 repository/file truth；S3/MinIO 管大 blob
- Go CLI `rddev` 管 Task DAG、worktree、Independent Worker、权限、结果收集和 Gate
- 开发模型：1 Supervisor + 默认 3、最大 4 个 Independent Claude Code Workers，不使用 subagent 作为主要编码单元

## 第一次使用

先读：`PROJECT_REPOSITORY.md` → `START_HERE.md` → `docs/64_UBUNTU_DEV_ENV.md` → `BOOTSTRAP_PROMPT.md`。

产品定义以 `docs/01...19` 为准；开发实现以 `CLAUDE.md`、ADR、`docs/20+`、`tasks/tasks.json` 和 machine specs 为准。
should not land
should not land
should not land
should not land
should not land
should not land
should not land
