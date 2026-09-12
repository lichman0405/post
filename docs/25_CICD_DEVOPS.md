# 25 — CI/CD 与开发运维

## Canonical 环境

Ubuntu 24.04 LTS amd64。CI runner 与开发环境尽可能一致。Windows Native 不作为支持矩阵。

## 本地

目标：clone/spec unpack 后，经 `make bootstrap` + `rddev doctor` + `rddev env up` 可启动 Postgres/Redis/MinIO/Gitea/Mail sink。应用 host-native。

## CI Gate

至少：

1. spec/schema validation；
2. Go fmt/vet/static analysis/unit；
3. Web lint/typecheck/unit/build；
4. Python adapter lint/type/pytest；
5. DB migration fresh + upgrade；
6. OpenAPI/schema generation drift check；
7. integration tests；
8. Playwright E2E；
9. security/dependency/secret scan；
10. container build/SBOM。

## Claude Code 开发系统

P0 完成后，普通 feature task 不应由 Supervisor 直接编码，而是 `rddev` 创建 worktree 并启动独立 Claude Code Worker。CI 只接受 Supervisor 产生的 commit/PR。

## 发布

所有容器使用 immutable digest；production deploy 先 staging smoke + migration check + backup checkpoint。禁止 Worker 直接触达生产部署 credentials。
