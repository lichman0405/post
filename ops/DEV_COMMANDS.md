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

- `test-integration` 先经 `scripts/pg-ready.py` 探测 `POSTGRES_TEST_ADMIN_URL`（默认 `postgres://postgres:postgres_dev_pw@127.0.0.1:5432/post`，即 `make infra-up` 实际发布的端口）；不可达时打印探测结论与修复提示后以非零退出 —— **绝不静默跳过**。
- `bash scripts/ci.sh [stage...]` 逐 stage 运行，每个 stage 是独立子进程（errexit 真正生效），失败会打印 `STAGE FAILED: <name>` 并立即停止 —— 与 `.github/workflows/ci.yml` 的 job/step 命名一一对应。不带参数运行全部 8 个 stage（integration 需要数据库）。
- `python3 scripts/validate_task_state.py` 校验 `tasks/tasks.json` 与 `tasks/task_status.json` 覆盖**同一任务集合**（双向：DAG 多出的任务、状态表多出的条目都会失败）、状态枚举、时间戳格式、`tasks/tests.json` 的测试引用与覆盖 —— 堵住「DAG 加了任务但状态表漏登记、被静默当 todo」的漂移类。
- 历史基线（gofmt 10 个文件、staticcheck 7 条、ruff/mypy 逐文件忽略）只放行**已存在的**旧债，新代码必须全绿；修复基线中条目后应删除对应行，新代码永不进基线。

## CI 工作流（.github/workflows/ci.yml）

10 个 job，全部跑在 `ubuntu-24.04`（docs/25、docs/64）：

1. `spec-validation` —— spec bundle + 任务 DAG 校验、OpenAPI 契约检查及其 fixture 测试（取代原 spec-validation workflow）
2. `task-state` —— DAG/state/test 覆盖与 JSON 形状校验 + fixture 测试
3. `go` —— gofmt / vet / staticcheck / unit（不含 `tests/integration`）
4. `web` —— typecheck（ui/web）/ lint / node --test / 生产构建（`POST_ENV=prod`）
5. `acceptance` —— 四层 Gate 环自身：four-gate / rejection-retry / supervisor-git / driver-persistence 四套 e2e、gitea-e2e-guard 单测，外加阶段边界 checkpoint
6. `python` —— ruff / mypy / pytest
7. `migration-integration` —— 需要基础设施的三个 job 之一，独立 job + `pgvector/pgvector:0.8.6-pg16` service container（与 docker-compose.yml 同镜像），跑 `make test-integration`
8. `observability` —— metrics / alerts / 分布式 trace 套件（`make observability-smoke`；`needs: [go]`，自拉 pgvector 镜像）
9. `a11y` —— 全站 WCAG 2.2 AA（T1104）：生产构建 `@post/web`、真 PostgreSQL + 真 Chromium 走 docs/42 每个有路由的核心页，axe-core 的 WCAG 遍与 best-practice 遍都必须为 0，且动态页必须渲染出 fixture 内容而不是空壳
10. `i18n` —— 语言基线（T1105）：同一套 Go 挂具 + 真 PostgreSQL + 真 Chromium，在 en/zh-CN 两种偏好下走核心路由，断言文案切换、`<html lang>` 跟随、两种语言下的 API 请求逐字节相同、以及科学量与单位逐字相同（`make i18n`）

**`specs/orchestrator/gates.json` 与这份列表是 lockstep**：G2 跑的就是这里的 job 与逐步命令（含 per-step `env`），`internal/devorchestrator/gate_spec_test.go` 在 CI 上挡住任何单边改动。加一个新 job 就要同时改 ci.yml、gates.json 的 `required_jobs` / `G2.runs_jobs` / `G4.asserts_jobs` 三处，再 `python3 scripts/spec_version.py --write`。

## 本地基础设施（Docker Compose，T0003 起可用）

基础设施容器化，应用 host-native（docs/66 §2）。端口、dev-only 凭证、覆盖方式与 reset 指南见 `infra/docker/README.md`。

```bash
make infra-up     # 启动 Postgres+pgvector / Redis / MinIO / Gitea / Mailpit，等待全部 healthy
make infra-init   # 一键幂等初始化：MinIO bucket + Gitea admin/测试 org/服务账户（可重复执行，no-op）
make infra        # = infra-up + infra-init
make migrate      # 把仓库内嵌的 migration 应用到 dev 数据库（幂等；已到 head 则报 0 applied）
make infra-down   # 停止（保留 named volume，数据不丢）
make infra-ps     # 查看状态
make infra-logs   # 跟踪日志
```

**`make migrate` 是 `make dev` 的前置步骤。** 先前的文档只写了 `infra-up` → `dev`，
但没有任何一步创建 schema，因此应用是连着一个**空数据库**起来的——
唯一会执行 migration 的代码是集成测试，而它建的是用完即删的临时库。
需要真实数据库的检查（G3、手工联调）必须先把 `post` 迁到 head：

```bash
make infra && make migrate && make dev
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

## 备份/恢复演练（T1110 起可用，docs/37_BACKUP_DR.md）

```bash
./ops/backup-restore-drill.sh                   # make infra-up + infra-init + migrate，然后跑演练
./ops/backup-restore-drill.sh --no-infra        # 栈已经起好并迁到 head 时用这个
./ops/backup-restore-drill.sh --artifacts DIR   # 指定产物目录（默认 <repo>/.backup-dr/drill-<run id>）
```

演练（`cmd/api/backupdr`，由 blocking 测试 `restore drill` = `tests/integration/restore_drill_test.go` 跑完）做四件事：

1. **备份四类**，逐字照 `docs/37_BACKUP_DR.md:4`：Postgres、S3 blobs/manifests、Gitea repositories、critical secrets/config **metadata**（secret 的**值**按 secret manager 策略走，不进产物）；
2. **恢复进空环境**——空是**证明**出来的，不是假设的：`report.json` 的 `restore.emptiness_checks` 逐类给出证据（0 tables / 0 rows、bucket 不存在、organization 不存在）；空环境本身照既有工具建（`make infra-up` + `make infra-init` + `make migrate`），不另起一套；
3. **对账四条轴** DB ↔ Git refs ↔ blob hashes ↔ release manifests，且**只报不改**：drift 记 high-severity finding 加一行 audit（`audit_log`，append-only），`repairs_applied` 恒为 0，报告里带 `repair_proposal` 但不执行；
4. **打开五样**：Seed Project、Release、Asset、Files、Evidence Graph（`docs/37:10`，五样都要能打开）。

产物落在 `.backup-dr/`（`.gitignore` 覆盖）。脚本在**创建目录之前**先拒绝工作树里任何没被 gitignore 的产物目录——dump 与 git mirror 不能有一次 `git add -A` 就能进仓库的机会。产物里不含凭证值：dump 的凭证列被替换成占位符，运行结束前再扫一遍整个产物目录，扫到就**拒绝**这次运行。

退出码：`0` 跑通；`1` 失败；`2` **skip，同样是 Gate 失败**；`3` 用法错误；`4` 栈起不来；`5` 产物目录没被 gitignore。

`2` 值得单独说：`go test` 在测试 skip 时**退出 0**，只看退出码的 Gate 会在没有 Docker daemon 的机器上变绿。所以这个脚本要求看到运行自己的 PASS 行，并把 skip 的理由原样打出来。重测试缺依赖时的形状照 `tests/integration/git_reconciliation_test.go:18-24`：**显式 skip 并打印理由**，绝不静默变绿。

**这轮验收是「流程能跑通」，不是「达到 RPO 24h / RTO 4h」**（`docs/37_BACKUP_DR.md:12-13`）：脚本与报告都不测量 RPO/RTO，也都不断言、不声称任何 RPO/RTO。

## 部署/发布/回滚 Runbook 演练（T1204 起可用，docs/35、docs/36、docs/37）

```bash
bash tests/acceptance/runbook-drill.sh              # blocking test `runbook drill`
python3 ops/runbook-verify.py                       # 只跑规则引擎：文档与实际命令是否一致
python3 ops/runbook-verify.py --list-checks         # 打印每条规则断言什么；不验证任何东西
python3 ops/runbook-verify.py --selftest            # 每条规则各来一次变异，必须被自己这条规则拒绝
python3 ops/runbook-verify.py --json                # 同样的结论，机器可读
```

把 `docs/35_DEPLOYMENT_RUNBOOK.md`、`docs/36_RELEASE_RUNBOOK.md`、`docs/37_BACKUP_DR.md`、`ops/deploy/README.md`、`ops/DEV_COMMANDS.md` 当作语料，把**每条命令、每个标志、每个文件、每个变量、每条关于本仓库有/没有某物的断言**重新在这棵树上解一遍（D1–D7、T1–T2 共 9 条规则）。**发现即失败，0 发现才算通过。**

`ops/runbook-steps.json` 是这三本 runbook 的**清单**：每个章节都要在这里按**怎么执行**分类——`mechanised`（有命令，命令在树上解析得到，且语料里有文档点名它）、`named-unavailable`（有命令且能解析，但今天跑不了，`blocked_by` 必须指向一条**仍然成立**的 tree claim）、`manual`、`provider`、`absent`。**runbook 里新加一节而不在这里分类，就是一条 finding** —— 这就是这个任务存在的意义：没人说过怎么执行的步骤，正是要被抓出来的缺口。文档里的结构性标题（如 docs/35 的 `## 顺序`）在 `containers` 里声明，且**双向校验**。

`ops/runbook-recovery`（`go run ./ops/runbook-recovery --admin-url …`）是**回滚/forward-fix 场景的执行体**，跑在真实 PostgreSQL 上：建一个 run-scoped 空库（`test_T1204_runbook_drill_*`，跑完必删，失败路径也删），把它**钉在旧 schema 版本**上（模拟从旧备份恢复），forward repair 到 head，在 head 上重跑 migrate job（必须是 0 条），然后**真的尝试一次回滚**（`MigrateTo` 给一个低于当前的版本），要求它**什么都没动**。退出码：`0` 场景复现了 runbook；`1` 没复现；`2` 用法错误；`3` 这台机器没有 PostgreSQL。

退出码：`0` 全部通过；`1` 有检查失败；`2` 缺输入/工具/服务（**算失败，不静默跳过** —— 与 `tests/acceptance/deploy-staging-smoke.sh` 和 docs/37 同一约定）；`3` 用法错误。

**这不是一次部署。** 仓库里没有 Dockerfile（tree claim `no-dockerfile`），没有镜像，起不来任何东西，所以**文档里那些 docker compose 子命令只做到「解析得到」**（规则 D4），真跑要等镜像落地。干跑能诚实做到的三件事：命令全部解析得到；拿现有工具去跑现有产物（模板校验器、schema 快照、secret 扫描、备份演练的 `--help` 与 fail-closed 出口码）；以及**用真实数据库执行回滚故事里唯一不需要运行中部署的那一半**。

**没跑的都打出来，不丢。** 真实 Compose parser 与重型备份/恢复演练是**显式 opt-in**（`RUNBOOK_DRILL_DOCKER=1` / `RUNBOOK_DRILL_BACKUP=1`）：它们要么需要 Docker daemon，要么需要镜像，而且「某个工具在不在机器上」不该决定一个 Gate 的结论。默认不打这两个开关时，脚本会逐条打印**为什么没跑**以及它替代检查了什么。

**这份 gate 自己能被打开开关的只有这些，其余全是硬失败。** 七处 `MUTATION CHECK` 故意让检查失败一次：引擎的 `--selftest`、从副本里删掉一条规则（它的变异必须变成「存活」）、喂一份点名了不存在脚本的 runbook 副本、喂一份多出一节/一个标题的 runbook 副本、拿一份改过一条迁移的**副本**去查 schema 快照（必须报 STALE）、备份演练的 fail-closed 出口码逐个真跑、以及给恢复程序一个不存在的目标版本（它的中心断言必须失败）。**这份 gate 自己的退出码接线不在其中**（那等于让 gate 递归跑自己），只做过一次性手工断链验证并记在 T1204 的 RESULT.json 里。

## Supervisor 无人值守驱动（2026-09-13）

```bash
setsid rddev drive --parallel 2 > .rddev/runtime/driver.out 2>&1 &   # long-lived process
rddev status                                                          # driver alive/dead, Workers, decisions
```

`rddev drive` 是**独立于 Supervisor 会话生命周期**的长期进程：

- **单实例**：`flock` 抢占 `.rddev/runtime/driver.lock`；第二个 driver **立即拒绝并指名持有者**
  （两个 driver 会重复 collect、重复 merge）；
- **心跳**：`.rddev/runtime/driver.json`——"是否活着"是**可从磁盘读出的事实**，
  而不是翻进程表推断；`rddev status` 由心跳年龄判定 alive/dead（崩溃的 driver 显示 dead，不是"联系不上"）；
- **接管**：启动时读磁盘状态，**接管已在运行的 Worker**，绝不重新 spawn；
- **待决事项**：需要判断的一律写入 `.rddev/runtime/decisions.json`，且**记录它所针对的 run_id**——
  rework/respawn 改变了那个 run，待决事项即自动清除，driver 自行恢复；
- **不降低任何 Gate**：每个动作都是**调用 rddev 本身**完成的；driver 只决定"下一步尝试什么"，
  rddev 按自己的规则拒绝。

契约：driver 是长期进程，**Supervisor 会话是它的一个窗口，不是它的前提**。

回归测试：`tests/acceptance/driver-persistence-e2e.sh`（已接入 acceptance stage / CI / gates.json）——
断言从"启动后立即退出的父进程"启动的 driver **仍然活着**、被 reparent 到 init、心跳在推进、
**接管**了已有 Worker 而未重新 spawn、第二个 driver 被拒、以及退出后锁会释放。
