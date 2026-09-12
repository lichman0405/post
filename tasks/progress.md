# 开发进度

状态：P0 进行中。
最后更新：2026-09-12（Supervisor bootstrap）

## 当前阶段

P0 — Canonical 开发环境、Orchestrator 与工程脚手架（T0000–T0012）。

## 已完成

- **Supervisor bootstrap 前置校验**
  - `python3 scripts/validate_specs.py` → OK：131 tasks、依赖无 orphan、无环、JSON specs 合法。
  - Canonical remote 校验：`origin` normalized = `lichman0405/post`，default branch = `main`，
    本地 `main` == `origin/main` == `fecf24d`（规格包已推送）。
  - 环境 preflight：Ubuntu 24.04.5 LTS amd64 / 16 cores / 46 GB RAM / 64 GB swap /
    195 GB free；Go 1.27.1、Node 24.21.0、pnpm 12.4.1、Python 3.12.7、uv 0.7.9、
    Docker 29.8.0、claude 2.1.269 —— 满足 canonical 环境要求。
  - Repository visibility：实测 `PRIVATE`，与规格记录的生成期基线 `public` 不一致 → 按
    `docs/69` §5 进入 `SPEC_BLOCKED` → owner 裁定 PRIVATE 是有意的 → 解除阻塞。
    见 `tasks/decisions.md` L3-20260912-1。
- **Worker 隔离层验证**：SMOKE isolation smoke test 12/12 通过（commit/push/branch/tag/
  remote/worktree/gh/docker/sudo/token-probe 全部阻断，HEAD 未变）。
  见 `tasks/results/SMOKE/RESULT.json`、`tasks/decisions.md` L1-20260912-3。

## 进行中

- **T0000** — Ubuntu 24.04 Canonical Environment Preflight
  - Worker：`worker-T0000`，branch `task/T0000-ubuntu-preflight`，baseline `fecf24d`。
  - 交付目标：确定性 preflight 脚本 + `rddev doctor` 检查契约 + preflight 单测/ubuntu smoke。

## 阻塞

- 无（repository visibility 的 L3 已由 owner 裁定解除）。

## 已知风险 / 需 owner 关注

- **Branch protection 不可用**：私有仓库在当前 GitHub plan 下无法启用 branch protection
  （403 要求 Pro 或 public）。`docs/69` §6 的目标规则目前只能靠 Supervisor 纪律保证，
  无法由平台强制。见 `tasks/decisions.md` F-20260912-1。

## 下一步

1. T0000 完成后执行 G2 验收（独立重跑测试、diff scope 校验、acceptance 逐条核对）。
2. 通过后 commit → push → PR → merge，并把 `tasks/results/T0000/RESULT.json` 入库。
3. 解锁 T0001（规格仓库/依赖图校验脚本化），随后 T0002（Monorepo 初始化）。
4. 尽早并行化：T0002 完成后 T0003 / T0004 / T0005 / T0009 具备并行条件
   （需确认写冲突面，默认并行度 3）。
