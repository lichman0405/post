# 安全与隐私标准

## 1. 威胁模型重点

最大风险不是传统 XSS  alone，而是：private research 泄漏、错误 publish、对象级权限绕过、signed URL 泄漏、Agent token 权限过大、Git transport 绕过 main protection、search side-channel、webhook secret 泄漏。

## 2. Authentication

安全 cookie/session；MFA 结构预留；PAT 只显示一次、hashed storage、scopes、expiry、last used；Git token 与 MCP/API token 可分别 scope。

## 3. Authorization

默认 deny。每个 command 明确 `actor + org + project + object + action + policy context`。权限单元测试和 property-based matrix test。

## 4. Private→Public

这是最高风险 operation。必须：human explicit action、impact preview、rights validation、no hidden private dependency leak、audit event、optional org policy approvals。Agent token 默认无 `visibility:publish` scope。

## 5. Data isolation

所有 query/search/export/download 进行 tenant/project/object policy 过滤。私有对象计数也不能通过 public API 泄漏；公开 asset 页面“8 private projects use this”仅在产品策略明确允许的匿名聚合阈值下显示，V1 可直接不显示 private count。

## 6. Blob

private bucket/object encryption、signed URL 短 TTL、Content-Disposition 安全、恶意文件扫描 hook、HTML/SVG active content sandbox、preview sanitization。

**V1 范围**：V1 不含用户可见的上传入口（`L3-②`，`docs/02` §4），本节要求适用于 V1 内部流程
产生的 blob 与 V1.x 的上传路径；下载侧（signed URL、access policy）V1 照常适用。

**出平台**：V1 不把平台内容发送到平台之外的第三方服务（模型、向量、分析）。分类规则与
「哪一级数据永不出平台」见 `docs/55`（`L3-⓪` 裁定）。

## 7. Web

CSP、CSRF、XSS escape、secure headers、SameSite、rate limiting、login brute force protection、SSRF guard for External Reference fetch、URL allow/deny strategy。

## 8. Agent/MCP

Tool input validate；prompt text 不直接成为 SQL/path；workspace path traversal 防护；service token least privilege；sensitive tool calls trace。外部 URL 内容视为不可信数据，不作为工具指令。

## 9. Git

主分支保护双层；server hooks validate ref; SSH key/token revoke；禁止 force push protected refs；Git LFS/large blob 限制。

## 10. Secrets

仅环境/secret manager；`.env` 不提交；日志 redact。CI secrets 最小权限。

## 11. Security Gate

每个 Release 运行 dependency audit、SAST、secret scan、container scan、OWASP smoke、permission E2E。Critical/High 必须修复或有书面 ADR/risk acceptance（V1 不接受 Critical）。
