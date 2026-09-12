# POST Source Repository

本规格包已经绑定到真实源代码仓库，而不是占位 repository。

## Canonical source repository

- Product: **POST — Platform for Open Science & Technology**
- Repository: `lichman0405/post`
- HTTPS: `https://github.com/lichman0405/post.git`
- SSH: `git@github.com:lichman0405/post.git`
- Default branch: `main`
- Repository owner: `lichman0405`
- Package generation baseline: 2026-09-12
- Baseline repository state when this package was generated: empty repository, default branch `main`, GitHub visibility `public`
- Owner-confirmed visibility expectation (as of 2026-09-12): `private` — recorded machine-readably in
  `specs/orchestrator/source-repository.yaml` under `visibility_expectation` (owner decision:
  `tasks/decisions.md` L3-20260912-1)

> 重要：repository visibility 是外部状态，可能随后改变。Supervisor 在任何 push 前必须重新读取 GitHub remote 状态并让本地 `origin` 与本文件匹配。不得根据本文件假设当前 visibility。生成时记录（`public`）是历史事实；当前预期是 owner 确认的 `private`；push 前必须用
> `python3 scripts/source_repo_preflight.py --check-visibility`（或 `--observed-visibility`）实测并与 `visibility_expectation` 比对——不一致、未记录或无法确定时 preflight 输出 `SPEC_BLOCKED` 并拒绝 bless，禁止 push（`docs/69` §5）。

## Source GitHub 与产品内 Gitea 的区别

这两个系统承担完全不同的职责：

1. `github.com/lichman0405/post` 是 **POST 产品自身源代码开发仓库**。Supervisor 使用它管理 POST 的 Issue、Branch、Commit、PR、CI 和 Release。
2. Gitea 是 **未来 POST 产品运行时内部的 Git infrastructure**，用于承载 POST 用户的科研项目 Git repository。Gitea 不是 POST 自身的源码托管平台，也不能替代本 GitHub repository。

任何代码、文档或变量命名必须避免混淆 `source_repo` 与 `product_git_provider`。

## Git control plane

只有 Supervisor Claude Code 可以持有本 repository 的 GitHub credentials，并执行：

- issue create/update/close
- branch/worktree lifecycle
- commit
- push
- pull request
- review/integration decisions
- merge
- tag/release

Independent Worker 不得获得 GitHub token、SSH private key、`gh` authenticated session 或可写 Git refs。

## 首次初始化要求

在提交本规格包前（以及此后每次 push 前），Supervisor 必须：

1. `python3 scripts/validate_specs.py` —— 校验任务 DAG（无 orphan dependency、无环）、spec
   JSON/YAML/Markdown 可读、`source-repository.yaml` 规格完整性（含 `visibility_expectation` 已记录）；
2. `python3 scripts/source_repo_preflight.py --check-visibility`（或 `--observed-visibility <value>`）
   —— 校验 `origin` 归一化 owner/name 为 `lichman0405/post`、integration branch 为 `main`、
   实时 visibility 与 owner 确认预期一致。任一失败/不一致/无法确定 → verdict `SPEC_BLOCKED`
   （退出码 1），**禁止 push**，按 `docs/69` §5 处理；
3. `python3 scripts/spec_version.py --check` —— 规格版本标记与内容一致（标记由内容 hash 推导，
   见 `specs/SPEC_VERSION.json`）；
4. 检查 remote 是否已有提交，禁止假设仓库仍为空；
5. 仅 Supervisor 创建首次 commit/push；Worker 不参与 repository bootstrap。
