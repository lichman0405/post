# 64 — Ubuntu Canonical Development Environment

## 1. 唯一标准环境

Canonical OS：**Ubuntu 24.04 LTS amd64**。

Windows Native 不作为支持的开发环境。Windows 11 用户使用 WSL2 Ubuntu 24.04，并把仓库放在 Linux filesystem，例如 `/home/<user>/src/post`，不要放在 `/mnt/c/...`。

CI、Supervisor、Worker、`rddev`、Docker Compose 与生产容器全部以 Linux 行为为准。

**预检验证**：在仓库根目录运行 `ops/doctor.sh`（T0000 起的参考实现；T0009 后为 `rddev doctor`）。检查契约见 `ops/doctor-checks.md`。退出码：0 = required 全过；1 = 工具链缺失/漂移；2 = 非 canonical 环境（含 Windows Native）；3 = 用法错误。默认不探测 Docker daemon（Worker 无 socket 权限）；宿主可用 `--check-docker-daemon` 开启 daemon 与 data-root 磁盘检查。

## 2. 硬件

### 最低可用（Supervisor + 1–2 Worker）

- CPU：8 vCPU / 8 核
- RAM：32 GB
- SSD：250 GB NVMe 可用空间
- Swap：8 GB
- Network：稳定公网，建议 >=100 Mbps
- GPU：不需要

### 推荐（Supervisor + 3–4 Worker，V1 标准）

- CPU：16 vCPU / 16 核以上
- RAM：64 GB
- SSD：1 TB NVMe
- Swap：16 GB
- Network：稳定低丢包，建议 300 Mbps–1 Gbps
- GPU：不需要

### 舒适长期开发机

- CPU：24–32 核
- RAM：96–128 GB
- SSD：2 TB NVMe

更高配置主要改善并行 Next.js build、Go tests、Playwright、Docker services 与多 Worker 同时运行，不提升 Claude 模型本身推理速度。

### 2.1 doctor 检查阈值（ops/doctor.sh / rddev doctor 执行）

预检按以下阈值判定（required 失败 → 退出码 1；推荐档不足 → advisory 警告，不影响退出码）：

| 检查 | required | 推荐（advisory） |
|---|---|---|
| CPU（`nproc`） | ≥ 8 vCPU | ≥ 16 vCPU |
| RAM（`/proc/meminfo` MemTotal） | ≥ 32 GiB | ≥ 64 GiB |
| Swap（`/proc/meminfo` SwapTotal） | ≥ 8 GiB | ≥ 16 GiB |
| 仓库文件系统可用空间（`df`） | ≥ 20 GiB | ≥ 100 GiB |
| docker data-root 可用空间（需 `--check-docker-daemon`） | ≥ 100 GiB | — |
| `ulimit -n` | ≥ 65535 | — |

推荐档对应上面"推荐（Supervisor + 3–4 Worker）"配置；"最低可用"档的 250 GB NVMe 属整机采购指引，运行时检查以"可用空间"为准。

## 3. 基础系统软件

建议安装：

`build-essential git git-lfs curl wget ca-certificates gnupg jq unzip zip make pkg-config openssh-client rsync ripgrep fd-find tree lsof tmux htop shellcheck bubblewrap socat postgresql-client redis-tools`

Ubuntu 24.04 上若启用 Claude Code sandbox，需要 `bubblewrap` + `socat`，并按 Claude Code 官方文档配置 bwrap 的 AppArmor user namespace 权限。

## 4. 语言与框架版本基线（2026-09）

- Go：1.27.x；规格生成时稳定补丁为 1.27.1。
- Node.js：24 LTS；规格生成时为 24.21.0 LTS。
- pnpm：项目 `packageManager` 字段锁定具体版本，禁止 Worker 自行升级 major。
- Next.js：16.3.x Active LTS；规格生成时安全版本为 16.3.3。
- Python：3.12+；Scientific Adapter 使用 `uv` 管理虚拟环境与 lock。
- Docker Engine + Compose v2。
- Claude Code：使用 Ubuntu apt stable channel或 native stable 安装；团队开发机应锁定/记录版本，升级由 Supervisor/人工显式执行，不在 Worker 运行中自动漂移。

### 4.1 预检基线表与漂移策略（ops/doctor.sh 执行）

预检对以下版本做 **major 级基线比对**（详见 `ops/doctor-checks.md` §7–8，Go 版 `rddev doctor` 按同一契约实现）：

| 工具 | 基线 | 比较 |
|---|---|---|
| Go | 1.27.x | == 1.27 |
| Node.js | 24 LTS | == 24 |
| Python | ≥ 3.12 | >= 3.12 |
| pnpm | package.json `packageManager` 精确 pin | 精确 ==（无 package.json 时仅记录） |
| psql | 16.x（noble 仓库） | == 16 |
| redis-cli | 7.x（noble 仓库） | == 7 |
| git | 2.x | == 2 |
| git-lfs | 3.x | == 3 |
| jq | 1.x | == 1 |
| make | 4.x | == 4 |
| shellcheck | 记录（noble 仓库 0.9.x，未 pin） | presence |
| Docker Compose | v2 插件 lineage（major ≥ 2；major 记录审计，v1 不合法） | >= 2 |
| Docker Engine / uv / ripgrep(rg) / Claude Code | 记录版本，不比较 | presence |

**漂移策略**：任何 major 版本漂移（含"高于"基线）都会被预检判为 required 失败并**显式报告**（remediation 注明需 Supervisor 批准），绝不自动修复、绝不静默放行。基线变更必须由 Supervisor 显式决策：先更新本文件 §4.1 与 `ops/doctor-checks.md`，再升级开发机；反向操作会被预检拦截。

## 5. 基础设施容器

Docker Compose：

- PostgreSQL + pgvector
- Redis
- Gitea
- MinIO
- Mailpit / mail sink
- 可选 OpenTelemetry Collector

应用开发态 host-native：Go API/worker/mcp/rddev、Next.js、Python adapter。CI/发布使用容器镜像。

## 6. 文件系统与资源

- repo、worktree、node_modules、Go build cache 必须在 ext4/Linux filesystem。
- `.rddev/worktrees` 与主仓库在同一高性能 SSD。
- Docker data-root 保证至少 100 GB free（预检在 `--check-docker-daemon` 时测量，required）；定期 prune 由 `rddev env gc` 管理，不能在 Worker 中无条件 `docker system prune -a`。
- `ulimit -n` required >= 65535（预检失败时：立即 `ulimit -n 65535`，并持久化到 `/etc/security/limits.conf` 的 nofile）；并行浏览器/Node 文件 watcher 容易耗 fd。

## 7. Claude Code

验证：

```bash
claude --version
claude doctor
```

Worker 用 headless `claude -p`。Supervisor 可交互运行。`rddev` 记录每个 Worker 的 Claude version/model/session/budget/exit status。

## 8. 安全原则

- Worker 不得到 GitHub/Gitea/production credentials。
- Worker 不得到 Docker socket，除非特定 integration task 明确允许；多数测试经 `rddev`/Make target 驱动。
- 不使用 `bypassPermissions` 作为 Worker 默认模式；使用 `dontAsk` + 明确 allow/deny/hook。
- Supervisor 也不在仓库 `.env` 存 production secrets。

## 9. WSL2 建议

若在 Windows 11 上开发：建议为 WSL2 分配约 12–16 processors、48 GB RAM（若宿主 64 GB）或 64 GB RAM（若宿主 >=96 GB），20–32 GB swap。长期无人值守和 4 Worker 并行更推荐独立 Ubuntu 主机。
