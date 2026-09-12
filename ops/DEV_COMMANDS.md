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
