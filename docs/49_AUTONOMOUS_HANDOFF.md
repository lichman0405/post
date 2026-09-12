# 49 — Claude Code 无人值守交接

## 目标

尽量减少日常人工参与，但不允许 Agent 自主改变产品意图。

## 开发组织

一个长期 Supervisor Claude Code + 多个由 `rddev` 拉起的独立 Claude Code Worker。Worker 不是 subagent；任务完成即回收。默认并行 3，最大 4。

## Supervisor 自主权

可自行拆任务、排序、增加 implementation/spike/test 子任务、修 bug、做 L0/L1 决策。L2 写 ADR；L3 `SPEC_BLOCKED`。

## Worker 自主权

只在 task scope 内选择实现细节。发现邻近问题只报告 follow_up，不扩 scope。

## Git

所有 Issue/Branch/Commit/PR/Merge/Release 由 Supervisor 控制。Worker 无 credentials 且 collect 时验证无 HEAD/ref mutation。

## 恢复

任何新 Supervisor 会话先读 task_status/progress/decisions/recent git log/worker registry，不依赖旧聊天记忆。

## 人工出现的条件

真实生产 secret/domain/account、法律协议、科研内容最终 Scientific Review/Publish、L3 产品/安全/权限变化。普通编码/测试不应等待人工。
