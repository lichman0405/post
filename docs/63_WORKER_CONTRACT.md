# 63 — Independent Claude Code Worker Contract

## 1. Worker 是什么

Worker 是一个新的完整 Claude Code 进程，不是 subagent。Worker 没有全局项目管理职责，也不需要完整产品上下文。

## 2. 输入 Task Package

必须包含：

- task_id / goal；
- dependencies 已满足声明；
- allowed_scope / forbidden_scope；
- requirements；
- acceptance_criteria；
- required_tests；
- relevant ADR/spec 路径；
- baseline SHA；
- result schema；
- 禁止操作。

## 3. Worker 可以做

- 搜索/读取整个 worktree；
- 修改 allowed scope；
- 新增任务所需测试；
- 运行编译、lint、unit/integration/browser tests；
- 记录风险和 follow-up；
- 在任务 scope 内做 L0/L1 实现决策。

## 4. Worker 禁止做

- commit/push/merge/rebase/tag/branch/worktree 操作；
- Issue/PR/Release；
- 修改产品规格来适配实现；
- 越过 allowed scope；
- 删除或弱化测试；
- 引入生产 secret；
- 使用 Supervisor credentials；
- 自行扩大 visibility/permissions；
- 把 follow-up issue 顺手实现。

## 5. 完成条件

只有全部 acceptance criterion 有 evidence、要求测试执行通过、无越界 diff 时可返回 `completed`。否则返回 `blocked` / `failed`，必须明确具体原因。

## 6. RESULT.json

结果至少包含：

```json
{
  "task_id": "T0204",
  "status": "completed",
  "summary": "...",
  "files_changed": [],
  "tests": [{"command":"...","status":"passed"}],
  "acceptance": [{"id":"AC1","status":"passed","evidence":"..."}],
  "risks": [],
  "follow_up_issues": [],
  "notes_for_supervisor": ""
}
```

Worker 的 completed 只是“提交验收”，不是任务最终 accepted。
