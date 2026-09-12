# 62 — Supervisor 长周期自主开发治理

## 1. 核心原则

Supervisor 可以自主推进实现，但不能自主改变产品意图。

## 2. 决策等级

| 等级 | 范围 | 处理方式 |
|---|---|---|
| L0 | 格式、命名、小型无语义重构 | 自动决定 |
| L1 | package、索引、库、小型内部接口 | 自动决定并记录 decisions.md |
| L2 | 跨模块架构、基础设施、持久化边界 | 写 ADR，评估与现有 ADR 是否冲突 |
| L3 | 产品语义、安全边界、权限、公开范围、科研语义 | SPEC_BLOCKED，等待人工决定 |

## 3. Supervisor 不应频繁请求人类

普通工程困难不是阻塞理由。优先：调查、拆任务、启动 Research/Review Worker、写 spike、选择可逆实现。

只有 L3，或无法在既定架构内做出可逆 L2 决策时才真正停该路径。

## 4. 失败与重试

- 第一次失败：Supervisor 根据 evidence 决定同 Worker 返工或新 Worker。
- 第二次相同方向失败：强制新 Worker，防止 context lock-in。
- 连续三次失败：拆分 task、做 diagnostic spike，禁止无限重试。
- Worker crash/预算耗尽不是 completed；状态为 worker_failed。

## 5. 架构漂移防护

每次 G2 验收检查：

- 是否引入规格外 service/database/framework；
- 是否绕过 domain/auth transaction boundary；
- 是否复制一套 schema/type；
- 是否把 Gitea/Next.js/Python 变成 canonical domain layer；
- 是否引入 private→public 隐式升级；
- 是否使测试或 audit 变弱。

## 6. Supervisor 自己改代码的边界

只允许：

- merge conflict resolution；
- 极小 integration glue；
- `rddev` 自身恢复；
- 明确的小型 CI/config fix。

超过约一个普通 Task 的工作量，必须创建 Task 并交 Worker。

## 7. 追踪链

所有合并必须能回溯：

`Requirement → Task → Worker Session → RESULT → Supervisor Verification → Commit → PR → Merge → Tests`
