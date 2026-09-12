# START HERE — POST V1

**POST — Platform for Open Science & Technology** 的产品定义已经完成；本仓库规格用于让 Supervisor Claude Code 自主调度 Independent Claude Code Workers 完成 V1。

## Canonical source repository

`https://github.com/lichman0405/post.git`

先阅读 `PROJECT_REPOSITORY.md`。GitHub 是 POST 自身源码 control plane；产品内部 Gitea 是未来 POST runtime 的 Git infrastructure，两者不得混淆。

## 先做什么

1. 在 Ubuntu 24.04 LTS 环境 clone `lichman0405/post`。
2. 首次 push 前重新确认 repository visibility 与项目 owner 预期一致。
3. 将本规格包内容放入 repository root。
4. 阅读 `docs/64_UBUNTU_DEV_ENV.md`，执行/审查 `ops/bootstrap-ubuntu.sh`。
5. 安装并验证 Claude Code：`claude --version && claude doctor`。
6. 启动长期 Supervisor Claude Code，向其提供 `BOOTSTRAP_PROMPT.md`。
7. Supervisor 首先完成 P0：repository preflight、`rddev`、Monorepo、Docker Compose、CI/test gates；之后才进入产品功能。

## 关键执行思想

Supervisor 是唯一全局研发大脑和 GitHub control-plane actor；每个业务任务由 Supervisor 通过 `rddev` 拉起一个独立完整 Claude Code Worker。Worker 只得到任务包、验收条件和必要 ADR，不负责 issue/branch/commit/push/PR/merge。Supervisor 独立验收后才进入 Git 控制面。

## 入口文档

- `PROJECT_REPOSITORY.md`：真实 GitHub 源码仓库绑定。
- `CLAUDE.md`：Supervisor 最高规约。
- `docs/61_DEVELOPMENT_ORCHESTRATION.md`：完整调度模型。
- `docs/62_SUPERVISOR_GOVERNANCE.md`：长期自主开发治理。
- `docs/63_WORKER_CONTRACT.md`：Worker 合同。
- `docs/64_UBUNTU_DEV_ENV.md`：Ubuntu 硬件/软件/安全配置。
- `docs/65_MONOREPO_STRUCTURE.md`：目标代码树。
- `docs/67_TEST_GATES.md`：四层 Gate。
- `docs/69_GITHUB_SOURCE_REPOSITORY.md`：源码 GitHub 管理规则。
- `tasks/ROADMAP_TASKS.md`：逐任务要求与验收。
