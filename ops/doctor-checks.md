# Doctor 检查契约（Preflight Check Contract）

> 本文件是环境预检的**唯一实现契约**。参考实现为 `ops/doctor.sh`（T0000，纯 bash）；
> T0009 的 Go 实现（`rddev doctor`）必须按本契约逐条重实现，不得重新发明检查语义。
> 输出 JSON Schema 见 `ops/doctor-output.schema.json`；版本基线的人类可读权威见
> `docs/64_UBUNTU_DEV_ENV.md`（本文件 §8 的基线表与 docs/64 §4 保持一致，如有冲突以
> docs/64 为准并需同时更新两处）。

## 1. 调用方式

```
ops/doctor.sh [--json] [--fixture DIR] [--check-docker-daemon] [-h|--help]
```

- 从仓库根目录运行；所有基于文件系统的检查（磁盘、package.json、WSL 路径）锚定在**调用目录**（`pwd -P`）。`rddev doctor` 同样在仓库根目录运行。
- `--json`：stdout 仅输出 JSON（见 §5）；stderr 正常运行时为空。
- 默认（无 `--json`）：人类可读文本。每条检查一行 `[PASS|FAIL|WARN|SKIP] <ID> <measured> — baseline: <baseline>`，失败/警告行下方输出 `fix: <remediation>`，跳过行下方输出 `note: <reason>`，末尾输出 Summary 与 `Verdict: <verdict> (exit N)`。
- `--check-docker-daemon`：启用 Docker daemon 探测（`docker info`）与 data-root 磁盘检查。**默认关闭**：探测 daemon 需要 Docker socket 权限，Worker 环境（docs/63 §4）不允许；`rddev doctor` 保留同一开关语义。
- 未知参数 / 缺失 fixture 目录 → stderr 输出 usage，退出码 3，**不输出 JSON**。

## 2. 退出码与 verdict（稳定契约）

| 退出码 | verdict | 含义 |
|---|---|---|
| 0 | `ok` | 所有 required 检查 passed 或 skipped；advisory 允许 warn |
| 1 | `toolchain_failure` | 任一 required 检查 failed，且失败不来自 env 类 |
| 2 | `unsupported` | 任一 **env 类** required 检查 failed（非 Linux / Windows Native / 非 Ubuntu 24.04 / 非 amd64） |
| 3 | （无 JSON） | usage 错误 |

判定优先级：`unsupported` > `toolchain_failure` > `ok`。若同时存在 env 失败与工具失败，verdict 为 `unsupported`（退出码 2）。

**skipped 不影响 verdict**（skipped = 无法评估，例如 package.json 尚不存在、daemon 探测被关闭）。advisory 的 `warn` 不影响 verdict。

## 3. 确定性规则（determinism）

同一台机器上相同输入必须产生**字节级相同**的输出。因此：

1. 全程 `LC_ALL=C`；不得使用时间戳、随机数、并发 map 遍历。
2. 检查顺序固定：先 ENV-*（4 条），再 RES-*（13 条），最后 T-*（18 条），各段按 §7 目录顺序。
3. 每个失败状态的 remediation 文本是该检查的**固定字符串**（见 §7 表），不是动态拼接的自由文本。
4. JSON 字段顺序固定（见 `ops/doctor-output.schema.json` 与参考实现输出）。
5. 版本解析：取版本命令输出中的**第一个 `X.Y` 数字对**（`grep -oE '[0-9]+\.[0-9]+' | head -n1`，作用于完整输出，多行也解析）。数字按十进制整数比较（前导零用 `10#` 防护）。

## 4. 检查语义总则

- **severity**：`required`（失败 → 非零退出）或 `advisory`（失败 → warn，退出码不变）。
- **status**：required 检查 ∈ {passed, failed, skipped}；advisory 检查 ∈ {passed, warn, skipped}。advisory 检查**永不** failed。
- **category**：env / resource / tool。仅 env 类 required 失败映射为 `unsupported`。
- **版本比较算子**（§7 工具表 `kind` 列）：
  - `major`：解析出的 X.Y 对取 X，必须 == 基线 major。
  - `majorminor`：解析出的 X.Y 对必须 == 基线（如 `1.27`）。
  - `min`：解析出的 X.Y 对必须 >= 基线（如 `3.12`）。
  - `presence`：只检查存在性，版本仅记录（`measured`/`version` 字段），不比较。
  - 解析失败（输出中无 X.Y 对）→ failed，remediation 用缺失类文本。
- **漂移策略**：major/majorminor/min 检查失败时输出显式 `remediation_drift`（含"需 Supervisor 显式批准"语义），**绝不自动修复**。版本"高于基线"与"低于基线"同样 failed（Worker 不得静默大版本升级）。
- **测量失败**（如 `/proc` 不可读、df 失败）→ 该检查 failed，remediation 说明无法测量。这不会发生在 canonical 环境，属于防御路径。

## 5. JSON 输出

Shape 见 `ops/doctor-output.schema.json`。关键字段：

- `verdict` 与 `exit_code` 必须与真实退出码一致（§2 映射）。
- `baselines`：本次运行所执行的基线记录（工具 major 基线 + 资源阈值），供 `rddev` 记录与审计。
- `checks[]`：全部 35 条检查，固定顺序；每条含 id/name/category/severity/status/measured/version/baseline/remediation/reason。`remediation` 非空当且仅当 status ∈ {failed, warn}；`reason` 非空当且仅当 status = skipped。
- `summary`：required {total, passed, failed, skipped} 与 advisory {total, passed, warn, skipped}，必须与 `checks[]` 逐条计数一致。

## 6. Fixture 契约（测试钩子）

`--fixture DIR` 用 fixture 文件覆盖测量输入，用于在**不触碰宿主工具链**的情况下测试决策逻辑。Go 版 `rddev doctor` 必须保留等价注入点（如 `--fixture` 或等价 env 覆盖），否则 §12 的单元测试无法移植。

Fixture 目录内文件（均为纯文本，取第一行，去 `\r`）：

| 文件 | 覆盖内容 |
|---|---|
| `os.kernel` `os.arch` `os.id` `os.version_id` `os.pretty` `os.codename` | 对应 uname/os-release 字段 |
| `os.wsl` | `none` / `wsl1` / `wsl2` |
| `os.repo_path` | 覆盖 `pwd -P`（锚定磁盘、package.json、WSL 路径检查） |
| `res.nproc` `res.mem_total_kb` `res.swap_total_kb` `res.fd_limit` `res.disk_free_kb` | 资源测量（整数，内存/磁盘单位 kB） |
| `res.docker_reachable` | `true` / `false`（仅 `--check-docker-daemon` 时生效） |
| `res.docker_root_free_kb` | docker data-root 可用空间 kB（同上） |
| `res.bwrap` `res.socat` | `present` / `absent` |
| `<tool>`（如 `go`、`docker-compose`） | 工具存在且版本命令输出为该文件内容（取前 5 行） |
| `<tool>.absent` | 强制视为缺失（即使宿主存在该工具） |

规则：fixture 文件存在即覆盖；不存在则回退真实测量。fixture 模式下各检查相互独立（例如 `docker.absent` 不影响 `docker-compose` fixture 的判定）；真实模式下 `T-DOCKER-COMPOSE` 依赖 docker CLI（缺失 → skipped）。fixture 模式**从不**执行真实版本命令。

## 7. 检查目录（35 条）

### 7.1 环境检查（env，顺序固定）

| ID | 断言 | severity | baseline | 失败语义 | remediation |
|---|---|---|---|---|---|
| ENV-OS-KERNEL | kernel == Linux（`uname -s`；或 `$OS == Windows_NT`） | required | Linux | `MINGW*/MSYS*/CYGWIN*`/Windows_NT → 显式报 "Windows Native"（unsupported） | Windows：使用 WSL2 + Ubuntu 24.04，仓库必须放在 Linux filesystem（如 `/home/<user>/src/post`，绝不在 `/mnt/c/...`），见 docs/64 §1。其他：在 Ubuntu 24.04 LTS amd64 上运行（docs/64 §1） |
| ENV-OS-DISTRO | `/etc/os-release` ID == ubuntu 且 VERSION_ID == 24.04 | required | ubuntu 24.04 (noble) | 任一不满足 → unsupported | 安装 Ubuntu 24.04 LTS (noble)，或 WSL2 Ubuntu 24.04（docs/64 §1） |
| ENV-OS-ARCH | `uname -m` == x86_64 | required | x86_64 (amd64) | 不满足 → unsupported | canonical 架构为 amd64/x86_64（docs/64 §1） |
| ENV-WSL | WSL 版本（`/proc/version` 含 microsoft 且含 WSL2 → wsl2；仅 microsoft → wsl1；否则 none） | advisory | wsl2 or none | wsl1 → warn | 升级 WSL2：`wsl --set-version <distro> 2`（docs/64 §1） |

### 7.2 资源检查（resource，顺序固定）

| ID | 断言 | severity | baseline | 测量来源 | remediation（warn/failed 时） |
|---|---|---|---|---|---|
| RES-CPU | CPU 核数 ≥ 8 | required | >= 8 | `nproc` | 配置 ≥ 8 vCPU（docs/64 §2） |
| RES-CPU-TIER | CPU 核数 ≥ 16（推荐档） | advisory | >= 16 (recommended) | 同上 | 推荐 ≥ 16 vCPU（Supervisor + 3–4 Worker，docs/64 §2） |
| RES-RAM | MemTotal ≥ 32 GiB | required | >= 32 GiB | `/proc/meminfo` MemTotal (kB) | 配置 ≥ 32 GiB RAM（docs/64 §2） |
| RES-RAM-TIER | MemTotal ≥ 64 GiB（推荐档） | advisory | >= 64 GiB (recommended) | 同上 | 推荐 ≥ 64 GiB RAM（docs/64 §2） |
| RES-SWAP | SwapTotal ≥ 8 GiB | required | >= 8 GiB | `/proc/meminfo` SwapTotal (kB) | 配置 ≥ 8 GiB swap（docs/64 §2） |
| RES-SWAP-TIER | SwapTotal ≥ 16 GiB（推荐档） | advisory | >= 16 GiB (recommended) | 同上 | 推荐 ≥ 16 GiB swap（docs/64 §2） |
| RES-DISK | 仓库文件系统可用空间 ≥ 20 GiB | required | >= 20 GiB free | `df -Pk <repo_path>` 第 2 行第 4 列 | 释放仓库所在文件系统空间（docs/64 §2/§6） |
| RES-DISK-TIER | 可用空间 ≥ 100 GiB（推荐档） | advisory | >= 100 GiB free (recommended) | 同上 | 推荐 ≥ 100 GiB 可用（docs/64 §2：250 GB NVMe 级） |
| RES-FD | `ulimit -n` ≥ 65535 | required | >= 65535 | `ulimit -n` | 立即 `ulimit -n 65535`；持久化 `/etc/security/limits.conf`（nofile）（docs/64 §6） |
| RES-DOCKER-DISK | docker data-root 可用空间 ≥ 100 GiB | required（可评估时） | >= 100 GiB free on docker data-root | `docker info --format '{{.DockerRootDir}}'` + df（需 `--check-docker-daemon`） | 确保 data-root ≥ 100 GiB 可用；用 `rddev env gc` 清理（docs/64 §6）。不可评估（flag 关闭 / daemon 不可达）→ skipped，reason 说明原因 |
| RES-REPO-FS | 仓库路径不在 `/mnt/` 下（WSL 下必查） | required（仅 WSL） | repo not under /mnt/ | `pwd -P` | 把仓库移到 Linux filesystem（如 `/home/<user>/src/post`），绝不在 `/mnt/c/...`（docs/64 §1）。非 WSL → skipped |
| RES-BWRAP | bubblewrap 存在（Claude Code sandbox 前置） | advisory | present | `command -v bubblewrap` | `sudo apt-get install -y bubblewrap`（docs/64 §3） |
| RES-SOCAT | socat 存在（Claude Code sandbox 前置） | advisory | present | `command -v socat` | `sudo apt-get install -y socat`（docs/64 §3） |

### 7.3 工具链检查（tool，顺序固定）

版本命令输出取前 5 行；`measured` = 首行（截断 120 字符）；`version` = 首个 X.Y 对。

| ID | 工具（二进制名） | 版本命令 | kind / 基线 | severity | remediation_missing | remediation_drift |
|---|---|---|---|---|---|---|
| T-GIT | git | `git --version` | major 2 | required | `sudo apt-get install -y git` | 恢复 git 2.x — major 漂移需 Supervisor 显式批准（docs/64 §4） |
| T-GIT-LFS | git-lfs | `git lfs version` | major 3 | required | `sudo apt-get install -y git-lfs && git lfs install` | 恢复 git-lfs 3.x — major 漂移需 Supervisor 显式批准（docs/64 §4） |
| T-CLAUDE | claude | `claude --version` | presence | required | 安装 Claude Code stable：`npm install -g @anthropic-ai/claude-code`（或 Ubuntu apt stable channel） | —（版本仅记录；升级由 Supervisor/人工显式执行，docs/64 §4） |
| T-GO | go | `go version` | majorminor 1.27 | required | 安装 Go 1.27.x：https://go.dev/doc/install（解压到 /usr/local） | 恢复 go 1.27.x — major 漂移需 Supervisor 显式批准（docs/64 §4） |
| T-NODE | node | `node --version` | major 24 | required | 安装 Node.js 24 LTS（NodeSource 或 nvm） | 恢复 Node.js 24.x — major 漂移需 Supervisor 显式批准（docs/64 §4） |
| T-PNPM | pnpm | `pnpm --version` | presence | required | `corepack enable && corepack prepare --activate`（或 `npm install -g pnpm@<packageManager pin>`） | —（精确 pin 由 T-PNPM-PIN 检查） |
| T-PNPM-PIN | pnpm vs package.json | 见下 | 精确 == packageManager 字段 | required | — | 对齐 pin：`corepack enable && corepack prepare pnpm@<pin> --activate` — pin 漂移需 Supervisor 显式批准 |
| T-PYTHON3 | python3 | `python3 --version` | min 3.12 | required | 安装 Python 3.12+（发行版包或 deadsnakes PPA） | 恢复 Python ≥ 3.12（docs/64 §4） |
| T-UV | uv | `uv --version` | presence | required | `curl -LsSf https://astral.sh/uv/install.sh | sh` | —（版本仅记录） |
| T-DOCKER | docker | `docker --version` | presence | required | 安装 Docker Engine：https://docs.docker.com/engine/install/ubuntu/ | —（版本仅记录；CLI 版本查询不访问 socket） |
| T-DOCKER-COMPOSE | docker compose | `docker compose version` | min 2（v2 插件 lineage；major 仅记录审计） | required | `sudo apt-get install -y docker-compose-plugin`（Compose v2；legacy docker-compose v1 不是 canonical） | 恢复 Docker Compose v2 插件 lineage（`docker compose`，major ≥ 2）— legacy docker-compose v1 不是 canonical（docs/64 §4） |
| T-DOCKER-DAEMON | docker daemon | `docker info`（仅 `--check-docker-daemon`） | reachable | advisory | — | 启动 Docker：`sudo systemctl enable --now docker`（宿主权限，绝不在 Worker 上下文执行）。flag 关闭 → skipped |
| T-JQ | jq | `jq --version` | major 1 | required | `sudo apt-get install -y jq` | 恢复 jq 1.x — major 漂移需 Supervisor 显式批准（docs/64 §4） |
| T-PSQL | psql | `psql --version` | major 16 | required | `sudo apt-get install -y postgresql-client`（noble 上为 16.x） | 恢复 psql 16.x — major 漂移需 Supervisor 显式批准（docs/64 §4） |
| T-REDIS-CLI | redis-cli | `redis-cli --version` | major 7 | required | `sudo apt-get install -y redis-tools`（7.x） | 恢复 redis-cli 7.x — major 漂移需 Supervisor 显式批准（docs/64 §4） |
| T-MAKE | make | `make --version` | major 4 | required | `sudo apt-get install -y make` | 恢复 GNU Make 4.x — major 漂移需 Supervisor 显式批准（docs/64 §4） |
| T-RG | rg | `rg --version` | presence | required | `sudo apt-get install -y ripgrep` — 注意：二进制名是 `rg`，不是 `ripgrep`（`command -v ripgrep` 会误报缺失） | —（版本仅记录） |
| T-SHELLCHECK | shellcheck | `shellcheck --version` | presence | required | `sudo apt-get install -y shellcheck` | —（noble 仓库为 0.9.x，docs/64 未 pin；版本仅记录） |

T-PNPM-PIN 语义：读取 `<repo_path>/package.json` 的 `packageManager` 字段（形如 `pnpm@12.4.1`）。package.json 不存在 → skipped（T0002 落地前）；字段缺失/非 pnpm pin → skipped；pnpm 缺失 → skipped（依赖 T-PNPM）；否则 pnpm 版本命令输出首行必须与 pin 版本**逐字符相等**。

T-DOCKER-COMPOSE 语义：真实模式下若 docker CLI 缺失 → skipped（依赖 T-DOCKER）。fixture 模式下两检查相互独立。基线语义为"v2 插件 lineage"：`docker compose` 子命令可用且解析出的 major ≥ 2 即通过，major 版本（如 5.x）记录用于审计；major < 2（legacy v1 语义）→ failed。docs/64 §4 的"Compose v2"即指该 lineage（相对已废弃的 v1 `docker-compose`），不是字面 major==2。

## 8. 版本基线表（与 docs/64 §4 一致的实现快照）

| 工具 | 基线 | 检查 |
|---|---|---|
| Go | 1.27.x | majorminor == 1.27 |
| Node.js | 24 LTS | major == 24 |
| Python | ≥ 3.12 | min >= 3.12 |
| pnpm | package.json `packageManager` 精确 pin（无 package.json 时仅记录） | 精确 == |
| psql | 16.x（noble 仓库） | major == 16 |
| redis-cli | 7.x（noble 仓库） | major == 7 |
| git | 2.x | major == 2 |
| git-lfs | 3.x | major == 3 |
| jq | 1.x | major == 1 |
| make | 4.x | major == 4 |
| shellcheck | 记录（noble 仓库 0.9.x，未 pin） | presence + 记录 |
| Docker | Engine + Compose v2 插件 lineage | 记录；compose 插件 major >= 2（v1 不合法） |
| Claude Code | stable channel，版本记录，人工升级 | presence + 记录 |
| uv / ripgrep(rg) | 记录 | presence + 记录 |

基线变更流程：任何 major 升级必须由 Supervisor 显式决策，先更新 docs/64 §4，再同步更新本表与 `ops/doctor.sh` 内 `tool_def`/`baselines`，最后升级开发机。反向（先升级机器再改基线）会被预检拦截为 drift。

## 9. 资源阈值表

| 检查 | required | 推荐（advisory） |
|---|---|---|
| CPU | ≥ 8 vCPU | ≥ 16 vCPU |
| RAM | ≥ 32 GiB | ≥ 64 GiB |
| Swap | ≥ 8 GiB | ≥ 16 GiB |
| 仓库文件系统可用空间 | ≥ 20 GiB | ≥ 100 GiB |
| docker data-root 可用空间 | ≥ 100 GiB（需 `--check-docker-daemon`） | — |
| ulimit -n | ≥ 65535 | — |

（来源：docs/64 §2 最低可用档 + §6；推荐档对应 Supervisor + 3–4 Worker。）

## 10. Go 移植（T0009）实现要点

1. 按 §7 目录顺序实现 35 条检查；每条检查的 id、severity、baseline、remediation 字符串必须与本文件一致（字符串建议直接做成常量表 + 单测对照本文件）。
2. 版本解析：输出整体中首个 `X.Y` 对；比较算子按 §4。`docker compose version` 输出形如 `Docker Compose version v2.39.4`；`shellcheck --version` 的版本在第二行（`version: 0.10.0`），因此必须对完整输出解析，不能只看首行。
3. 退出码/verdict 映射按 §2；JSON shape 按 `ops/doctor-output.schema.json`；确定性规则按 §3。
4. daemon 探测默认关闭（`--check-docker-daemon` 显式开启），Worker 环境无 socket 权限。
5. 保留 fixture 注入点（§6），并把 §12 的单元测试移植为 Go 测试（fixture 驱动，不依赖宿主工具）。

## 11. 参考实现位置

- `ops/doctor.sh` — 纯 bash 参考实现（T0000 交付；无 go.mod 依赖）。
- `ops/doctor-output.schema.json` — JSON 输出 schema。
- `ops/tests/doctor-unit-test.sh` — fixture 驱动的决策逻辑单元测试。
- `ops/tests/doctor-smoke-test.sh` — 宿主端到端自洽性测试。

## 12. 已覆盖的测试场景（T0009 移植时必须保持等价覆盖）

1. 全绿 canonical fixture → exit 0 / verdict ok / 无 required failed。
2. 单个必需工具缺失（`<tool>.absent`）→ exit 1 / verdict toolchain_failure / 输出点名工具并给出对应 remediation。
3. 版本漂移（go 1.28）→ exit 1 / T-GO failed / version "1.28" / remediation 点名 1.27.x。
4. 低于基线（Python 3.11）→ exit 1。
5. Windows Native（`os.kernel=MINGW64_NT-...` 或 `OS=Windows_NT`）→ exit 2 / verdict unsupported / 输出含 "Windows Native" 与 "WSL2"。
6. 非 Ubuntu 24.04（os.id/os.version_id 覆盖）→ exit 2。
7. WSL1 → advisory warn，exit 0；WSL2 且 repo 在 `/mnt/` → RES-REPO-FS failed，exit 1。
8. pnpm pin 一致 → passed；不一致 → failed；packageManager 缺失/畸形 → skipped。
9. `--check-docker-daemon` + `res.docker_reachable=false` → T-DOCKER-DAEMON warn、RES-DOCKER-DISK skipped、exit 0。
10. 未知参数 → exit 3，无 JSON。
11. 同一 fixture 两次运行输出逐字节一致。
12. Compose v5（v2 插件 lineage，`Docker Compose version v5.x` fixture）→ passed；major < 2（`Docker Compose version v1.x` fixture）→ failed。
