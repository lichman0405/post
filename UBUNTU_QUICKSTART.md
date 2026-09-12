# POST — Ubuntu 24.04 开发机 Quickstart

推荐用于 Supervisor + 3–4 Independent Claude Code Workers：

- 16+ CPU cores / vCPU
- 64 GB RAM
- 1 TB NVMe SSD
- 16 GB swap
- Ubuntu 24.04 LTS amd64
- stable internet
- 无 GPU 要求

Canonical repository：

```bash
mkdir -p ~/src
cd ~/src
git clone https://github.com/lichman0405/post.git
cd post
git remote -v
```

把本规格包内容复制到 `~/src/post/` 根目录后：

```bash
./ops/bootstrap-ubuntu.sh
# 安装 Docker Engine + Compose v2（Docker 官方 Ubuntu repo）
# 安装 Go 1.27.x、Node 24 LTS、Python 3.12+ + uv、项目锁定的 pnpm
# 安装 Claude Code stable
claude --version
claude doctor
python3 scripts/validate_specs.py
```

**首次 push 前重新确认 GitHub repository visibility。** 本包生成时 `lichman0405/post` 为 Public 且为空，但 Supervisor 不得假设该状态仍然成立。

Windows 11：用 WSL2 Ubuntu 24.04；repo 放 `/home/<user>/src/post`，不要放 `/mnt/c/...`。长期 4 Worker 无人值守更推荐独立 Ubuntu 主机。

开发真正开始后，由 Supervisor 完成 P0，构建 `rddev`，之后业务任务才交独立 Worker。
