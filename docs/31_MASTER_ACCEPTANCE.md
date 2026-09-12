# V1 Master Acceptance / 最终验收总表

V1 完成必须全部满足。

## Gate A — Domain Integrity
- [ ] RSG 可导出/重建并有稳定 hash。
- [ ] Object/relation 版本不可改写历史。
- [ ] frozen main 无 direct semantic/Git write。
- [ ] Abort/Reopen 保留完整历史。
- [ ] Provenance 与 Evidence 关系分离。
- [ ] Claim 可表示支持、反驳、consistent、reproduce 等。
- [ ] Citation/Dependency 分离并影响分析只对 dependency 强传播。

## Gate B — Collaboration
- [ ] Branch/PR/RSG diff/Scientific Review/Integrity Review/merge 完整。
- [ ] Semantic conflict 不被 Agent 自动科学裁决。
- [ ] Public Project 外部用户可 fork/contribute。
- [ ] Private branch 不因 merge/publish误公开完整历史。

## Gate C — Release/Asset
- [ ] Release immutable、policy/schema/version/hash pinned。
- [ ] Dataset/Protocol/Material Collection/Benchmark 可发布。
- [ ] Asset PID/version/lineage/rights/reference/dependency/fork 工作。
- [ ] private→public 有影响预览和显式确认。

## Gate D — Network
- [ ] Public pages 未登录可访问。
- [ ] Explore/Search 可发现 public project/asset/knowledge/person/org。
- [ ] Published knowledge 可收到 external evidence，区分 origin/reviewed/unreviewed。
- [ ] Contribution 落到个人 Profile，组织 affiliation 不吞掉个人历史。

## Gate E — Search
- [ ] Seed query 生成 Evidence-backed Answer。
- [ ] Answer 引用确定版本实体；不得 hallucinate 不存在 source。
- [ ] Search 无 private leakage。
- [ ] Answer → Draft Research Context → new Project 流程可用。

## Gate F — Web UX
- [ ] Project 首页是 Summary/Research，不是文件树。
- [ ] Files 只读，无 edit/upload/delete。
- [ ] GitHub/Primer 中性高密度设计，无渐变紫/玻璃拟态。
- [ ] PR 默认 Research State Diff，raw Git diff 二级。
- [ ] Research Map 可访问并可 drill-down。

## Gate G — Agent/API/Git
- [ ] MCP semantic tools 完整且权限正确。
- [ ] Agent 无 merge/publish visibility 扩大权限。
- [ ] Git branch push ingestion；main direct push 拒绝。
- [ ] Git/RSG drift reconciliation 可检测。

## Gate H — Security/Quality
- [ ] Critical/High security issues = 0。
- [ ] permission negative E2E 全过。
- [ ] backup/restore tested。
- [ ] local one-command dev。
- [ ] CI blocking suites 全绿。
- [ ] accessibility AA core pages。

## Gate I — Canonical Workflow
完整 MOF workflow 从 Research Question 到外部项目贡献 Evidence 与 Profile credit，全程真实 DB/Git/blob/browser，非 mock 演示。

## Development System Gate（新增）

在任何 V1 产品完成声明前，开发系统本身必须满足：

- Ubuntu 24.04 canonical preflight 可复现；
- `rddev` 可恢复 task/worker/worktree 状态；
- 至少两个独立 Claude Code Worker 可并行完成互不冲突的示例任务；
- Worker 无 Git remote credentials，commit/push/branch mutation 尝试被拒绝或验收强制拒绝；
- Worker 越界 diff 被拒绝；
- Supervisor 可独立执行 G2/G3/G4 并只在全绿后 commit/PR/merge；
- Supervisor 会话中断后可从仓库状态恢复，不依赖旧聊天上下文。
