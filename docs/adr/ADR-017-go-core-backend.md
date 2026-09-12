# ADR-017：核心后端采用 Go

**Status:** Accepted

核心产品 API、RSG、权限、events、Git adapter、background workers、MCP 使用 Go。Python 仅承担 Scientific Adapter。优先 net/http + 轻量 router、pgx/sqlc、显式 SQL 与清晰 adapter 边界。
