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

## 3.1 自主执行授权与停止条件（owner 授权，2026-09-12）

**默认自主推进，不再逐 PR 请求人工批准。**

对 L0/L1 级别的常规实现任务与产品代码 PR，只要**同时**满足下列全部条件，Supervisor 可自行
review、commit、创建 PR 并 merge：

| # | 条件 |
|---|---|
| 1 | Worker 已返回 `completed`（或等效交付），且 Supervisor 已完成独立 G2 验收 |
| 2 | 该任务要求的 G1/G2/G3/G4 检查全部通过 |
| 3 | CI 全绿 |
| 4 | 没有未解决的 review 意见 |
| 5 | diff 未超出 task `allowed_scope`，无未声明改动 |
| 6 | 未改动既定产品语义、安全边界、权限模型、科研语义或核心架构原则 |

**必须停止并请求人工决定的情形（穷举，不在此列的一律自行决策并继续）：**

1. **L3 决策**：产品、科研语义、安全、权限、隐私、法律或公开性。
2. **重大 L2 决策**：会改变既定核心架构原则（与已接受 ADR 冲突且不可逆）。
3. **需要新的外部凭证、付费服务或账号授权**。
4. **`SPEC_BLOCKED`**：无法通过现有规格合理解决。

**不得等待批准的类别**（应自行决策、记录并继续）：普通代码实现、bug fix、测试、重构、
依赖范围内的技术实现选择、L1 基线校准、格式/命名/小型无语义重构。

**边界与不可让渡项：**

- 自主授权**不降低 Gate 标准**。禁止为让 Gate 变绿而删除/skip/弱化测试、放宽 assertion、
  放大 timeout 掩盖 race（§6 与 `docs/67` 仍然完全有效）。
- 自主 merge 仅适用于**本仓库** `lichman0405/post` 的 `main`；不适用于任何产品 runtime、
  外部系统或第三方账号。
- 每一次自主 merge 仍必须留下可追溯链：
  `Requirement → Task → Worker Session → RESULT → Supervisor Verification → Commit → PR → Merge → Tests`。
- 一旦任一条件不满足，授权立即失效，回到"停止并请求人工决定"。
- 该授权可由 owner 随时全部或部分撤回；撤回以写入本文件与 `tasks/decisions.md` 为准。

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
