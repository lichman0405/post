# ADR-011：Agent 默认只能通过 Semantic API/MCP 修改科研状态
Status: Accepted

Agent 不直接以任意 Git/YAML 写入作为主路径。Git CLI 仅 Compatibility Mode；治理动作受限。
