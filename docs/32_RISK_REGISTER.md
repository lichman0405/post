# 风险登记

| 风险 | 概率 | 影响 | 缓解 |
|---|---|---|---|
| RSG 过度抽象导致用户难用 | 中 | 高 | GitHub 风格 UI + Agent 操作 + progressive schema；Seed 用户测试 |
| 权限/公开误操作泄密 | 中 | 极高 | default deny；publish gate；agent 无 publish scope；负向 E2E |
| Git 与 RSG drift | 中 | 高 | 双写 saga、state hash、reconciliation、protected refs |
| Evidence 模型过度复杂 | 中 | 高 | V1 关系枚举有限；UI 隐藏内部复杂度 |
| Search 产生“看似科学”的 hallucination | 中 | 极高 | planner structured；answer only cited entity ids；fallback structured results |
| 贡献体系被刷 | 中 | 中 | 不做单一分数；区分独立/同组织 reuse；ledger 可审计 |
| Gitea 依赖过深 | 低中 | 中 | GitPort adapter；domain 不依赖 Gitea data model |
| Blob 成本/大文件 | 中 | 中 | object storage、multipart、quotas、preview limits |
| Primer 组件变化 | 中 | 低 | token wrapper + internal UI package，锁版本 |
| Claude Code 长任务失忆/重复 | 中 | 高 | task_status/tests/progress/Git checkpoint，CLAUDE.md |
| V1 范围膨胀 | 高 | 高 | 02_V1_SCOPE；feature flags；P12 前不引入 execution/migration/federation |
