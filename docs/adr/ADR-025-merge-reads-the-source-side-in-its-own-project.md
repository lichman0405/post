# ADR-025：合并的源侧按「它自己所属的项目」解析；跨项目源分支是合法输入，不是例外
Status: Accepted

外部 fork 这条路径在**数据库层**和**权限层**都已经写死了，而且写得很清楚：

- `infra/migrations/00086_external_fork.sql` 的 `pull_request_fork_gate` 允许一个 PR 的
  `source_branch_id` 属于**另一个项目**，条件只有一个：那个项目是 PR 所属项目的一个 fork、
  且 fork 人是这个 PR 的作者。头部注释逐字写着
  **"pull_requests has always carried source_branch_id and target_branch_id with no constraint
  tying them to the PR's project — the same-project rule lived only in the store — so an external
  PR is representable."**
- `specs/policies/permissions-matrix.csv:5-7` 给 `authenticated_nonmember` 的三格是
  `create_branch=external_fork_only`、`write_scientific_state=own_fork_only`、`open_pr=allow_from_fork`；
  `docs/04 §2` 的原话是「Public Project 的非成员用户不是 Contributor role，但可 Fork 并从自己的空间发起外部 PR」。
- `docs/31_MASTER_ACCEPTANCE.md:17` 把这件事列成主验收的一条：
  「Public Project 外部用户可 fork/contribute」。

**这条 ADR 定的是一条读的规则：既然源分支可以属于另一个项目，那么合并路径在解析源侧时，就必须问「这个分支属于哪个项目」，而不是假定它是 PR 的那个项目。**

## 问题：merge 把两侧钉在同一个项目上

`internal/application/merge/service.go` 的合并入口：

- `:439` `source, err := s.branch(ctx, in.ProjectID, pr.SourceBranchID)`
- `:441` 同形解析 `target`
- `:458-465` `diffs.Inputs{ProjectID: in.ProjectID, BaseStateID, SourceStateID: pr.ProposedStateID, TargetStateID}` 与
  `s.plans.Plan(ctx, in.ProjectID, ...)`

也就是说，源分支、被提案的状态、以及"读源状态的版本内容"这三件事全部被钉在 `in.ProjectID`
（PR 所属的上游项目）上。对 fork 来的 PR，第一步就落空。

而在它**之前**还有一条门（同样来自 00086）挡着相反的用法：PR 的源分支属于第三方项目、
且那不是作者本人的 fork 时，插入就被拒——所以"跨项目"这一维在库里只有**一个**合法取值，
不是任意的。**这不是把门放松，是把已经允许的那一种真正接通。**

## Decision

1. **源侧按「源分支自己所属的项目」解析**：源分支、提案状态、以及源状态的版本内容都从 fork 项目读；
   **目标侧仍按 PR 所属项目**（`target_branch_id`、目标头、落地写入都在上游项目）。
2. **这一维的合法取值不扩大**：能走到合并的跨项目 PR 仍然只有"源项目是 PR 作者本人的 fork"这一种；
   插入门（`pull_request_fork_gate`）保持原样，合并路径侧加一条对称判定，**fail closed**——
   解析不到合法的源项目就拒绝，不退回"按 PR 项目再查一遍"。
3. **门的强度一处不动**：`merge_ready` 状态机、main freeze、`pull_request_semantic_gate`、
   版本头锁的重试，全部照旧。
4. **内容搬运不放宽 rights**：`materialize` 仍是"逐字拷贝源版本内容"，`VisibilityPolicyID` 与 abort 记录
   照旧随内容走——**源在另一个项目不是放宽可见性或权利的理由**（docs/12 §3）。
5. **表示的诚实性**：`semantic_merges` 一行的 `project_id` 仍是**上游**项目，
   `source_state_id` 可能属于 fork 项目，`target_state_id`/`base_state_id` 属于上游项目。
   这层表示如果被证明不够，是一条**新的**架构问题，另行裁决，不在本 ADR 的授权范围内顺手改。

## Alternatives

- **把 fork 的提案复制进上游项目再合**（"内部化"）：会造出第二个 source of truth，
  一次 fork 提案要在两个项目里各有一份状态；`pull_request_fork_gate`（00086）存在的理由正是**不许**这样绕。
  否决。
- **让 fork 的 PR 走一条"外部专用"的合并实现**：`docs/09 §3` 说 main 只经 Research PR merge 前进
  （`00069:53-56` 逐字："main (and every other target branch) advances only this way"）。
  给外部提案开第二条合并路径，就是让"前进"有两个实现，provenance/审计都要跟着分叉。否决。
- **把这条能力砍出 V1**：`docs/31:17` 把外部贡献列进主验收，砍掉等于改产品范围——那是 L3，不是架构决定。

## Consequences

- `merge` 服务里每一处"按项目取值"的地方都要重新回答一次"这个值属于哪一侧"。
  这是本 ADR 的主要代价：改动面比一行修复大，但每一处都是**同一条规则**的应用。
- 合并后的状态、事件、Git saga 仍在上游项目落地；fork 侧只被**读**，
  以及 `closeSourceBranch` 对源分支的 `active → merged` 迁移（按分支自己的 id 做，与项目无关）。
- 外部贡献的审计面扩大：一次合并同时牵涉两个项目的事实，在 `semantic_merges`
  的三个状态 id 上已经是可读的，不需要新表。

## Migration / Exit Strategy

无需迁移：所需的两个事实（fork 谱系、合并行）都已经在库里。
退出策略：若将来允许"从 fork 直接推上游"（即取消 fork 要求），那条规则改的是
`pull_request_fork_gate` 与权限矩阵，本 ADR 的"源侧按自己的项目解析"仍然成立。

## 落地任务

T0817（P8）——"外部 fork 提案的合并路径：merge 必须能在源分支自己所在的项目里读它"。
它是 T0814 的 AC8 拆出来的一半；另一半（真实 HTTP 路径上开出外部 PR）留在 T0814。
