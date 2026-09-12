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

> 重要：repository visibility 是外部状态，可能随后改变。Supervisor 在任何首次 push 前必须重新读取 GitHub remote 状态并让本地 `origin` 与本文件匹配。不得根据本文件假设当前 visibility。

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

在提交本规格包前，Supervisor 必须：

1. `git remote -v` 校验 `origin` 指向 `lichman0405/post`；
2. 校验 default branch 为 `main`；
3. 检查 remote 是否已有提交，禁止假设仓库仍为空；
4. 检查 repository visibility；若与操作者预期不一致，进入 `SPEC_BLOCKED`，禁止 push；
5. 运行 `python3 scripts/validate_specs.py`；
6. 仅 Supervisor 创建首次 commit/push；Worker 不参与 repository bootstrap。
