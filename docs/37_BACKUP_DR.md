# Backup / Disaster Recovery

## 需要备份
Postgres、S3 blobs/manifests、Gitea repositories、critical secrets/config metadata（secret 本身按 secret manager backup policy）。

## 一致性
备份需记录 snapshot timestamp；恢复后运行 reconciliation：DB ↔ Git refs ↔ blob hashes ↔ release manifests。

## V1 验收
必须至少做一次空环境 restore test，并成功打开 Seed Project、Release、Asset、Files、Evidence Graph。

## RPO/RTO 目标
V1 staging 先验证流程；生产初始目标可设 RPO 24h/RTO 4h，后续按实际客户要求收紧。
