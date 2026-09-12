# Dev Commands（目标接口）

开发环境完成 P0 后应支持：

```bash
make bootstrap
rddev doctor
rddev env up
make dev
make test
make test-integration
make test-e2e
rddev task next
rddev worker spawn Txxxx
rddev worker collect Txxxx
rddev task verify Txxxx
rddev task accept Txxxx
rddev git commit Txxxx
rddev pr open Txxxx
rddev pr merge Txxxx
```

Supervisor 使用 `rddev` 管 Worker/Git；Worker 不直接执行 `rddev git/pr` control-plane 命令。

## CI 基线检查（T0008 起可用）

Gate 分层契约：**`make check` / `make test` 是基础设施无关的** —— 不需要 Docker、不需要数据库，裸机必须通过。一切需要真实 PostgreSQL 的测试只属于 `make test-integration`：

```bash
make check            # Go vet/build/unit + Web typecheck/lint/unit + Python pytest + schema/OpenAPI drift
make test             # 三语言单元测试（Go unit + web config test + Python pytest）
make test-integration # 真实 PostgreSQL 上的迁移+集成套件；数据库不可达时大声失败并打印原因
make fmt-check        # gofmt 门禁：新文件必须格式化（历史基线见 ops/ci/gofmt-baseline.txt）
make staticcheck      # honnef.co 静态分析（逐条历史基线见 ops/ci/staticcheck-baseline.txt）
make lint-python      # ruff（配置 ops/ci/ruff.toml）
make type-python      # mypy（配置 ops/ci/mypy.ini）
make check-openapi    # OpenAPI 契约校验（解析 + $ref 完整性 + 结构）
make progress         # 从 tasks/task_status.json 重新生成 tasks/progress.md 的自动段落
make ci               # 本地复刻 CI 全部 6 个 stage（= bash scripts/ci.sh）
```

语义要点：

- `test-integration` 先经 `scripts/pg-ready.py` 探测 `POSTGRES_TEST_ADMIN_URL`（默认 `postgres://postgres:postgres_dev_pw@127.0.0.1:15432/post`）；不可达时打印探测结论与修复提示后以非零退出 —— **绝不静默跳过**。
- `bash scripts/ci.sh [stage...]` 逐 stage 运行，每个 stage 是独立子进程（errexit 真正生效），失败会打印 `STAGE FAILED: <name>` 并立即停止 —— 与 `.github/workflows/ci.yml` 的 job/step 命名一一对应。不带参数运行全部 6 个 stage（integration 需要数据库）。
- `python3 scripts/validate_task_state.py` 校验 `tasks/tasks.json` 与 `tasks/task_status.json` 覆盖**同一任务集合**（双向：DAG 多出的任务、状态表多出的条目都会失败）、状态枚举、时间戳格式、`tasks/tests.json` 的测试引用与覆盖 —— 堵住「DAG 加了任务但状态表漏登记、被静默当 todo」的漂移类。
- 历史基线（gofmt 10 个文件、staticcheck 7 条、ruff/mypy 逐文件忽略）只放行**已存在的**旧债，新代码必须全绿；修复基线中条目后应删除对应行，新代码永不进基线。

## CI 工作流（.github/workflows/ci.yml）

6 个 job，全部跑在 `ubuntu-24.04`（docs/25、docs/64）：

1. `spec-validation` —— spec bundle + 任务 DAG 校验、OpenAPI 契约检查及其 fixture 测试（取代原 spec-validation workflow）
2. `task-state` —— DAG/state/test 覆盖与 JSON 形状校验 + fixture 测试
3. `go` —— gofmt / vet / staticcheck / unit（不含 `tests/integration`）
4. `web` —— typecheck（ui/web）/ lint / node --test / 生产构建（`POST_ENV=prod`）
5. `python` —— ruff / mypy / pytest
6. `migration-integration` —— 唯一需要基础设施的 stage，独立 job + `pgvector/pgvector:0.8.6-pg16` service container（与 docker-compose.yml 同镜像），跑 `make test-integration`

## 本地基础设施（Docker Compose，T0003 起可用）

基础设施容器化，应用 host-native（docs/66 §2）。端口、dev-only 凭证、覆盖方式与 reset 指南见 `infra/docker/README.md`。

```bash
make infra-up     # 启动 Postgres+pgvector / Redis / MinIO / Gitea / Mailpit，等待全部 healthy
make infra-init   # 一键幂等初始化：MinIO bucket + Gitea admin/测试 org/服务账户（可重复执行，no-op）
make infra        # = infra-up + infra-init
make infra-down   # 停止（保留 named volume，数据不丢）
make infra-ps     # 查看状态
make infra-logs   # 跟踪日志
```

要点：

- 所有 host 端口只绑定 `127.0.0.1`；默认端口：Postgres `5432`、Redis `6379`、MinIO `9000/9001`、Gitea `3000/2222`、Mailpit `8025/1025`。全部可通过环境变量覆盖。
- 凭证全部为 dev-only 默认值（如 `postgres/postgres_dev_pw`、`postadmin/postadmin_dev_pw`），仅限本地开发。
- Gitea 仅为内部 Git infrastructure（ADR-019），不面向产品用户。
- `docker-compose.yml` 不写死 `COMPOSE_PROJECT_NAME`；并行 Worker 用不同 project name + 端口覆盖互相隔离（docs/66 §3）。
- 镜像全部 pin 精确 tag（无 `latest`）；MinIO 官方镜像在 quay.io（Docker Hub 已停更）。

## 环境预检（当前可用，`rddev doctor` 落地前）

从仓库根目录运行：

```bash
./ops/doctor.sh                    # 人类可读输出；退出码 0/1/2/3（见下）
./ops/doctor.sh --json             # 机器可读 JSON（ops/doctor-output.schema.json）
./ops/doctor.sh --check-docker-daemon   # 额外探测 docker daemon 与 data-root 磁盘（默认关闭，需 socket 权限）
bash ops/tests/doctor-unit-test.sh  # fixture 驱动的决策逻辑单元测试（bash + python3）
bash ops/tests/doctor-smoke-test.sh # 宿主端到端自洽性冒烟测试（bash + python3）
```

退出码契约：`0` = required 检查全过（advisory 警告不影响）；`1` = 工具链缺失或版本超出基线（显式漂移报告，不自动修复）；`2` = 非 canonical 环境（非 Linux / Windows Native / 非 Ubuntu 24.04 / 非 amd64）；`3` = 用法错误。

检查契约（id、severity、baseline、remediation）见 `ops/doctor-checks.md` —— 这是 T0009 实现 `rddev doctor` 的规范，不可在实现时重新发明检查语义。
