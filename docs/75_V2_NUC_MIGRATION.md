# 75 — V2 开发环境迁移到 NUC（2026-10-29）

本文件记录从笔记本迁移到 NUC 的最小可复现步骤，以及迁移瞬间的代码/账本状态。
它不是产品验收，也不改变任何 Gate。

## 1. 迁移瞬间的远端状态

- canonical remote：`git@github.com:lichman0405/post.git`（HTTPS 等价）。
- `origin/main`：`e442ae2`（T1339 修复已合并，PR #403）。
- 未验收的在飞工作已推成 WIP 快照，不在 `main`：
  - `wip/20261029-T1307-web06a`：WEB-06a（分支/草稿 + Question/Hypothesis/Experiment/Calculation），Review 已 approve，G2 被迁移中止。
  - `wip/20261029-T1338-member-ui`：WEB-03 项目 Settings 成员邀请 UI + 旅程，已 rebaseline 到 `e442ae2` 并合成 T1339 的 overview 旅程；Worker 被迁移中止。
- T1312（WEB-11 Issues）在等 T1338 的成员邀请 UI，状态 blocked/rejected；无代码 diff。
- T1301–T1306、T1329、T1332–T1335、T1339 已合并。

## 2. 不随 GitHub 迁移的东西

- DeepSeek/Kimi 的 API 凭据（在用户本机 cc-switch 配置里，绝不提交）。NUC 上需重新配置；
  Worker 启动统一走 `python3 ~/with_deepseek.py ./bin/rddev ...`，由包装器在进程内注入，
  不写文件、不提交。
- Docker 卷（PostgreSQL/Gitea/MinIO/Redis 数据）、`.rddev/` runtime、G2 venv、Playwright
  浏览器与系统库。NUC 上按下面重建；测试库/卷都是可丢弃的。
- 本机 `docker-compose.local.yml`（quay.io MinIO 401 的本地镜像 workaround）。

## 3. NUC 初始化（Ubuntu 24.04 amd64，native 优先）

```bash
# 基础
sudo apt-get update
sudo apt-get install -y build-essential git git-lfs curl jq make shellcheck \
  bubblewrap socat postgresql-client redis-tools docker.io docker-compose-v2 \
  python3 python3-venv python3-pip libnspr4 libnss3 libasound2t64 \
  libatk1.0-0t64 libatk-bridge2.0-0t64 libcups2t64 libdrm2 libxkbcommon0 \
  libxcomposite1 libxdamage1 libxfixes3 libxrandr2 libgbm1 libpango-1.0-0 libcairo2

# Go 1.27.3（docs/64 基线 1.27.x；staticcheck 2026.2.2/gosec 2.30 已验证）
# 从 https://go.dev/dl/ 下载 go1.27.3.linux-amd64.tar.gz，校验 SHA256，解压到 /usr/local/go

# Node 24 LTS + pnpm 12.4.1 + gh 2.102.0 + uv 0.12.13
# Node 从 nodejs.org 下载 v24.x linux-x64 tarball，校验 SHASUMS256
npm install -g pnpm@12.4.1 @anthropic-ai/claude-code
# gh 2.102.0 从 github.com/cli/cli releases 安装；uv 从 astral.sh/uv 安装
```

G2 需要的 Python 工具用独立 venv，避免 PEP 668：

```bash
python3 -m venv ~/g2-venv
~/g2-venv/bin/pip install pyyaml uv==0.12.13 ruff mypy
```

```bash
git clone git@github.com:lichman0405/post.git ~/post
cd ~/post
make rddev
./bin/rddev doctor          # 期望 25/27 required；RAM 31GiB 与 bubblewrap 见已知项
```

## 4. 本地真实服务

`docker-compose.yml` 的 MinIO 镜像 `quay.io/minio/minio` 在 2026-10-26 对匿名拉取返回 401。
本机 workaround 是 `docker-compose.local.yml`（bitnamilegacy/minio + `user: "0"` + curl healthcheck），
不提交。NUC 上先试官方镜像；仍 401 时复用该 override：

```bash
docker compose -f docker-compose.yml -f docker-compose.local.yml up -d --wait
docker exec -u git post-gitea-1 gitea admin user create --username postadmin \
  --password postadmin_dev_pw --email postadmin@example.com --admin --must-change-password=false
docker exec -u git post-gitea-1 gitea admin user create --username post-git-svc \
  --email post-git-svc@localhost --user-type bot
```

G2 运行前把工具放进 PATH（`rddev` 的 step 继承 Supervisor 环境）：

```bash
export PATH=$HOME/g2-venv/bin:$HOME/.local/bin:$HOME/go/bin:$PATH
```

## 5. 恢复在飞任务

1. `git fetch origin`，确认 `origin/main = e442ae2`。
2. T1307：`git branch task/T1307-web-06a origin/wip/20261029-T1307-web06a`，用
   `rddev worker spawn` 的新 attempt 接手（或重新走 Review/G2）；Review 已 approve，
   但 G2 未在迁移前完成，需重跑。
3. T1338：同上从 `origin/wip/20261029-T1338-member-ui` 恢复；它已包含 T1339 的
   overview 旅程合成，先跑 `make web-v2` 验证，再 Review/G2。
4. T1338 合并后重派 T1312；T1312 需要成员邀请 UI 才能让 B 作为成员回复。
5. 账本中 `T1307/T1338/T1312` 迁移时置为 `blocked`（原因：环境迁移），恢复后重新
   ready/running；不得把 WIP 分支当成已验收。

## 6. 不变的安全与验收边界

- Worker 仍无 Git/GitHub/Gitea 凭据；commit/push/PR/merge 只由 Supervisor 做。
- 所有 Worker 启动（含 rebaseline 的自动 rework）必须带 provider env 包装器。
- G1–G4、12 个 required CI job（含 `web-v2`）、U01–U14 真网页验收不因迁移放宽。
- Kimi 仍为外部 429；本轮 Worker 模型是 owner 批准的 deepseek-flash。
