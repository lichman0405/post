# 66 — Local Development Environment

## 1. 一键体验

目标命令：

```bash
make bootstrap
rddev doctor
rddev env up
make dev
```

`rddev doctor` 检查 OS、CPU/RAM/disk、Claude、Git、Go、Node/pnpm、Python/uv、Docker/Compose、端口、fd limit。

## 2. 运行方式

Infrastructure 用 Docker Compose；应用 host-native：

- `go run ./cmd/api`
- `go run ./cmd/worker`
- `pnpm --filter web dev`
- `uv run ...scientific-adapter...`

这样 Worker 改代码不需要重复 rebuild application container。

## 3. 并行 Worker 隔离

共享基础 infrastructure，但测试资源 namespace 化：

- DB schema/database：`test_<task_id>_<run_id>`
- MinIO bucket prefix：`test/<task_id>/<run_id>`
- Redis key prefix/DB：run-specific
- temporary ports：由 rddev 分配
- Gitea test repo/org：run-specific

高风险完整 E2E 可请求 ephemeral compose project：`COMPOSE_PROJECT_NAME=rddev-<run_id>`。

## 4. Reset

必须提供可重复的：

```bash
rddev env reset --test-only
rddev env reset --all-dev-data --confirm
```

禁止 Worker 自己执行不可控全局 prune/drop。
