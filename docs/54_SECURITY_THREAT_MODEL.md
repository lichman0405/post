# V1 Threat Model

## Assets to protect
私有科研问题/方向、实验/计算数据、未公开 Claim/Finding、客户/合作关系、Rights、用户身份/token、Git repo、Asset integrity。

## Actors
匿名攻击者、恶意注册用户、越权项目成员、被盗 token、恶意 external URL、错误配置 Agent、供应链依赖。

## Highest-severity scenarios
1. Private Project/Branch 内容出现在 Search/Explore/API error。
2. Agent 自动 publish private data。
3. Gitea direct push 修改 frozen main。
4. Signed blob URL 被长期复用或跨项目访问。
5. External Reference fetch SSRF 内网。
6. PR merge/publish race 绕过 Review/Policy。
7. Search LLM 引用未授权 entity id。
8. Webhook secret/token 进入日志。

每个场景必须有对应 automated negative test，映射见 traceability 文档。
