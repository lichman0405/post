# Machine-readable Specs

- `api/`：OpenAPI contract。
- `schemas/`：Scientific Object / RSG / Asset JSON Schemas。
- `database/`：PostgreSQL logical seed/spec。
- `mcp/`：MCP tool catalog。
- `events/`：Research Event types。
- `policies/`：permissions/rights machine specs。
- `ui/`：routes/page inventory。
- `orchestrator/`：`rddev`、Worker Task Package、Worker Result、权限和决策等级。

Markdown 与 machine spec 冲突时，不得让 Worker自行猜测；Supervisor 判断是否 L1/L2 或进入 SPEC_BLOCKED。
