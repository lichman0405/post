# MCP Tool Catalog（说明版）

> **V1 不含 MCP 工具面**（`L3-③` 裁定，`docs/02` §3/§4）：本目录是 V1.x 的设计输入，
> V1 不交付、不接线（`cmd/mcp-server` 的 `/mcp` 按设计返回 501）。机器定义因此保留原样，
> 并在 `specs/mcp/tools.json` 里带 `"scope": "post-v1"` 标记。

机器定义：`specs/mcp/tools.json`。

## Read
project.get/list、branch.get/list、rsg.query、object.get/list、relation.list、issue.get/list、pr.get/diff/list、release.get/list、asset.get/search、knowledge.get、search.query、files.list/read_metadata。

## Scientific write（用户权限委托）
branch.create、object.create_version、relation.create_version、blob.request_upload/finalize_attach、evidence.create_assertion、issue.create/update、pr.create、validation.run。

## Proposal-only / Governance
abort/reopen main object、release.create、asset.publish、knowledge.publish、visibility change、merge：Agent 可 prepare/request，最终 action 必须由 Web governance 或显式 human-approved token flow。V1 最简单可让 MCP 只返回 proposal id。

## Tool design
每个 tool 明确 project/branch/expected version/idempotency key；返回 domain ids/version/validation，不返回任意数据库对象。
