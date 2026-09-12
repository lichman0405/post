# 30 — Task 执行标准

## 1. Task 必备字段

ID、phase、goal、dependencies、allowed_scope、forbidden_scope、requirements、deliverables、acceptance_criteria、required_tests、relevant_specs、risk/decision level。

## 2. 状态

`todo | ready | running | worker_failed | verification | rejected | blocked | accepted | merged`。

V1 required 不允许 skipped，除非正式 scope change + ADR/版本升级。

## 3. Supervisor 开始任务

1. 验证 dependencies merged/满足。
2. 读取相关规格/ADR。
3. 建立 Issue（若项目托管已可用）和 supervisor-owned branch/worktree。
4. `rddev` 写 baseline SHA、scope、task package。
5. 启动独立 Claude Code Worker。

## 4. Worker 完成任务

Worker：实现 → G1 tests → `RESULT.json` → 退出。Worker 不 commit。

## 5. Supervisor 验收

1. `rddev worker collect`：校验 HEAD 未改变、scope、result schema。
2. Supervisor diff review。
3. G2 独立 tests/acceptance。
4. 跨系统任务执行 G3。
5. 若通过：状态 accepted → Supervisor commit/push/PR。
6. G4 通过后 merge → 状态 merged。

## 6. Blocked

- `SPEC_BLOCKED`：L3 规格/产品/安全决策缺失。
- `ENV_BLOCKED`：真实外部资源不可获得。
- 工程难题默认不是 blocked；应拆分或 diagnostic spike。

## 7. 明确不算完成

UI mock 无 backend、endpoint 无 auth、仅 happy path、关键用户链路没有真实浏览器/infra、TODO 代替 required behavior、Worker 自己 commit 后自称完成。
