# 49 — Claude Code 无人值守交接

当前 V2 的直接启动入口为根 [V2_CLAUDE_CODE_PROMPT.md](../V2_CLAUDE_CODE_PROMPT.md)，具体执行协议为 [docs/72](72_V2_CLAUDE_CODE_HANDOFF.md)。产品范围与所有已确认决定先按 docs/70/71 读取，再恢复正式任务状态。

## 目标

尽量减少日常人工参与，但不允许 Agent 自主改变产品意图。

## 开发组织

一个长期 Supervisor Claude Code + 多个由 `rddev` 拉起的独立 Claude Code Worker。Worker 不是 subagent；任务完成即回收。默认并行 3，最大 4。

2026-10-26 Owner 更新：取消 `rddev drive` 的强制使用条件。Supervisor 可直接编排 `rddev` 子命令完成任务与 GitHub 流程，`drive` 仅为可选机械执行器。任务隔离、四层 Gate、Git 权限与可恢复记录保持有效，见 ADR-029。

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
