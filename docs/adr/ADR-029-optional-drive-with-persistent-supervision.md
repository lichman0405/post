# ADR-029：取消 drive 强绑定，保留持续监督与验收边界

- 状态：Accepted（Owner 明确授权）
- 日期：2026-10-26
- 范围：研发编排；不改变 POST 产品或科研语义

## 背景

原 CLAUDE.md §8.2 要求机械步骤由 `rddev drive` 驱动。Owner 在 V2 规划时明确要求：「取消rddev drive的硬约束使用条件。」

当前 drive 能推进派工、收集、评审、验收、Git/PR，但会把需要判断的问题写入磁盘，无法替代长期 Supervisor。工具是否在运行不能代表任务是否得到持续判断。

## 决定

1. `rddev drive` 为可选执行器，不再是启动或继续开发的必要条件。
2. Supervisor 可直接编排既有 `rddev task/worker/review/gate/git/pr` 子命令，无须就此再次请求批准。
3. 完整交付链必须保留：Issue → Branch/Worktree → Worker → 独立验收 → Commit → Push → PR → CI → Merge → 同步主线。
4. 保留 Supervisor/Independent Worker 模型、Worker 无 Git 控制权限、scope/HEAD 校验、RESULT、状态账本及 G1–G4。取消 drive 强制不等于取消 rddev 全部机制，也不等于批准一个 Agent 同时承担业务编码、独立验收和 Git 控制。
5. 直接编排也必须有持久状态、会话恢复、重复执行保护和工程失败处理；不能因一轮对话结束而把未完成任务留置。
6. 使用 drive 时避免第二个调度器同时修改同一任务。切换执行方式前检查现有进程、锁、Worker 与状态，先接管再继续。

## 后果

- V2 主交接方案采用 Supervisor 直接编排子命令，先串行验证，再按无冲突任务并行。
- 可以保留 drive 用于合适的机械步骤，无须删除或重写现有驱动器。
- 对工具进行大规模重构不是产品工作的前置条件；只修复影响交付的确定性阻塞。
- 产品、安全、权限、科研语义、重大架构或新外部凭据等原停止条件继续适用。

## 验证与记录

同步更新 CLAUDE.md §8.2、docs/49、docs/61、docs/62 和 V2 交接包。历史决策／日志中关于 drive 的描述保留为历史，不倒写。
