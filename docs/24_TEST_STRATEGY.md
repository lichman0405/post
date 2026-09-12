# 测试策略

## 1. 测试金字塔

Domain unit → repository/integration → API contract → multi-service integration → Playwright E2E → security/permission → canonical workflow。

关键状态机和权限不依赖 UI E2E 才验证，必须有快速 unit/integration 覆盖。

## 2. 必测不变量

- frozen main 无 direct write/push。
- private→public 不可由 Agent 自动发生。
- release/asset versions immutable。
- abort 不删除历史。
- citation/dependency 不混淆。
- external evidence 不可被 origin maintainer 静默删除。
- published version pin 不随 upstream current 变化。
- Files UI 无 mutation controls/hidden mutation endpoint。
- RSG/Git reconciliation。
- search 不泄漏 private objects。

## 3. Test data

固定 Seed MOF 项目，见 `34_SEED_DEMO_PROJECT.md`。不要大量随机不可读 fixture；重要 E2E 使用可解释 domain data。

## 4. Browser E2E

至少覆盖：注册/登录、建 public/private project、freeze main、Agent/API branch change、PR review merge、release、publish asset、未登录发现、外部 fork/contribute、network evidence、profile credit、private visibility negative cases。

## 5. Concurrency

同时修改同 Object、同时 merge PR、重复 idempotency key、worker retry、Gitea webhook duplicate、outbox duplicate。

## 6. Golden tests

RSG manifest serialization/hash、Research State Diff、Search query plan/answer citation、Publication Impact Preview 使用 golden fixtures；更新需显式 review。

## 7. tests.json

所有 blocking test suite 与阶段 Gate 必须同步写入 `tasks/tests.json`，便于 Claude Code 长周期追踪。
