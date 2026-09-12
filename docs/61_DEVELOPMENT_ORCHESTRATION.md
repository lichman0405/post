# 61 — Supervisor / Worker 开发编排

## 1. 目标

把 Claude Code 从“一个长期写代码的单 Agent”改造成一个可恢复、可并行、可审计的软件研发系统。Supervisor 保留完整产品与架构上下文，Worker 保持最小上下文并只解决单一任务。

## 2. 角色

### Supervisor Claude Code
长期驻留，读取完整规格。负责：

- Task DAG 与优先级；
- 依赖判断与并行调度；
- 创建 Issue/Branch/Worktree；
- 生成 Worker Task Package；
- 启停 Worker；
- 独立验收；
- Commit/Push/PR/Review/Merge；
- ADR 与规格冲突管理；
- Master Gate。

### Implementation Worker
独立完整 Claude Code 进程。只负责一个 task。可读整个 worktree，但只可修改 allowed scope；无 Git control-plane 权限。

### Review Worker
独立完整 Claude Code 进程。输入为 task spec + diff + relevant code；只审查，不负责 Git 操作。用于高风险、跨模块、权限/安全、复杂 merge 任务。

## 3. rddev 责任

`rddev` 是确定性执行器，不做产品判断。

推荐命令：

```bash
rddev doctor
rddev env up
rddev task next
rddev task inspect T0204
rddev task ready
rddev worker spawn T0204
rddev worker list
rddev worker logs T0204
rddev worker collect T0204
rddev task verify T0204
rddev task accept T0204
rddev task reject T0204 --reason-file rejection.md
rddev git commit T0204
rddev pr open T0204
rddev pr merge T0204
rddev env down
```

`rddev` 管：worktree、process、PID、日志、任务状态、Claude CLI 参数、权限配置、RESULT schema validation、scope diff validation、HEAD baseline、超时/异常退出、清理。

## 4. Worktree 生命周期

1. Supervisor 选择 ready task。
2. `rddev` 创建 supervisor-owned branch 与 worktree。
3. 记录 baseline commit SHA。
4. Worker 进入该 worktree。
5. Worker 仅产生 working-tree diff。
6. collect 时校验 HEAD 未变化、无新 refs、diff 未越界。
7. Supervisor 验收。
8. 通过后 Supervisor commit/push/PR/merge。
9. merge 完成后清理 worktree。

## 5. Worker 启动方式

优先使用 Claude Code 非交互 `-p` 模式，由 `rddev` 传入任务 prompt、结构化 RESULT JSON Schema、最大预算/turn、专用权限策略和独立 session name。

示意：

```bash
claude -p \
  --name worker-T0204 \
  --permission-mode dontAsk \
  --output-format stream-json \
  --json-schema @specs/orchestrator/worker-result.schema.json \
  --append-system-prompt-file .rddev/T0204/worker-system.md \
  "$(cat .rddev/T0204/task-prompt.md)"
```

实际 CLI 参数由 `rddev` 生成，不在人工流程里硬编码。

## 6. 权限隔离

至少三层：

1. Worker 环境不注入 GitHub/Gitea/SSH deploy token、production secrets、Docker socket。
2. Claude Code permission rules/hooks 阻断 git commit/push/merge/rebase/tag/branch mutation、gh/gitea control-plane。
3. collect 阶段检查 `HEAD == baseline`；若 Worker 产生 commit/ref 变更，任务强制 rejected。

allowed scope 在 collect 阶段做 path-based diff validation。Worker 可以读取全仓库以调查依赖，不意味着可以修改全仓库。

## 7. 并行策略

- 默认最大并行 Worker：3；硬上限：4。
- 同一核心 package、同一 migration 区、同一 OpenAPI/schema 文件存在写冲突时不得并行。
- schema/API 契约任务优先完成，再并行消费者实现。
- Supervisor 必须考虑 CPU/RAM/数据库 test namespace/端口隔离。

## 8. 任务结果

Worker 只提交结构化结果：状态、修改文件、执行测试、验收条款、已知风险、follow-up issue、失败原因。

Worker 不能把“建议改 scope”直接变成代码；只能写 `follow_up_issues`。

## 9. 可恢复性

Supervisor 崩溃或 context 被压缩后，通过：task_status、worker registry、worktree registry、PID、日志、Git state、RESULT 恢复。不得把关键调度状态只放在聊天上下文。
