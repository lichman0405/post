# Definition of Done

一个 feature/task 完成必须：

- Domain 行为符合 spec。
- API/schema 与实现同步。
- Authorization server-side。
- Audit/event 视需要产生。
- Unit/integration/negative tests。
- UI 有 loading/empty/error/permission states。
- Browser E2E（用户可见关键流程）。
- Accessibility basics。
- Observability/log correlation。
- 文档更新。
- 无新增高危安全问题。
- `task_status/tests/progress` 更新并 Git commit。

“能演示”不等于 done。

## Claude Code Task DoD

对由 Worker 实现的任务，`done/merged` 还要求：Worker RESULT schema valid、HEAD baseline 未被 Worker 改变、scope validation 通过、Supervisor G2 独立验收、需要时 G3、Merge 前 G4，以及 Task→Worker→Result→Commit→PR→Merge 可追踪。
