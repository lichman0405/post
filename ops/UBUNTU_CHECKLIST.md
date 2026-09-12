# Ubuntu 开发机准备清单

- [ ] Ubuntu 24.04 LTS amd64
- [ ] >=16 vCPU 推荐
- [ ] >=64 GB RAM 推荐
- [ ] >=1 TB NVMe 推荐
- [ ] 16 GB swap
- [ ] repo 位于 Linux filesystem
- [ ] Git + Git LFS
- [ ] Docker Engine + Compose v2
- [ ] Go 1.27.x
- [ ] Node 24 LTS + pinned pnpm
- [ ] Python 3.12+ + uv
- [ ] Claude Code stable，`claude doctor` 通过
- [ ] bubblewrap + socat + Ubuntu 24.04 AppArmor profile
- [ ] fd limit >=65535
- [ ] Supervisor 可以联网调用 Claude
- [ ] Worker 环境无 Git remote/production credentials
