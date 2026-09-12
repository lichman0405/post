# 部署 Runbook

## 前置
域名、TLS、Postgres、Redis、S3、Gitea、SMTP/Email provider、OIDC credentials、secret manager。

## 顺序
1. 备份/确认 DB。
2. 部署 migrations job。
3. 部署 API/worker/MCP。
4. 部署 Web。
5. 检查 Gitea webhook/service account。
6. 检查 object storage bucket policy/CORS。
7. smoke：health/auth/public/private/branch/PR/upload/search。
8. run canonical minimal E2E。

## 回滚
应用可回滚 previous image；DB 只 forward repair。若 migration 破坏兼容，停止流量并按 ADR/runbook 修复，不做手工生产 SQL“救一下”后不记录。
