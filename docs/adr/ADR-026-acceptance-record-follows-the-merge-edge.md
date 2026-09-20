# ADR-026：验收记录沿合并边可读——main 上的状态是被合并出来的，它的评审要能被读出来
Status: Accepted

`docs/11_RELEASE_ASSET_HUB.md:5` 逐字要求 Release manifest 固定
**"review/approval record"**；`internal/rsg/validation/validator.go:162` 把同一件事写成了门的判据：
**"scientific and integrity review must be recorded and approved (docs/11 §1)"**。

**这条 ADR 定的是一条读的规则：状态的验收记录必须沿"合并边"可读。** 一个状态进入 main 的方式
决定了它的评审记在哪一行上——今天记录的读法只认其中一种，而产品路径走的是另一种。

## 问题：记录沿 `parent_state_id` 走，而 main 是合并出来的

读在 `internal/persistence/queries/releases_assets.sql:88-107`（`ListReleaseReviews`）：

```sql
WITH RECURSIVE lineage(id) AS (
  SELECT project_states.id FROM project_states WHERE project_states.id = @state_id
  UNION
  SELECT ps.parent_state_id FROM project_states ps JOIN lineage l ON ps.id = l.id
  WHERE ps.parent_state_id IS NOT NULL
)
... FROM reviews r JOIN pull_requests pr ON pr.id = r.pull_request_id
WHERE pr.target_branch_id = @main_branch_id
  AND pr.proposed_state_id IN (SELECT id FROM lineage)
```

而 main 今天**只能**经 Research PR merge 前进：
`infra/migrations/00069_semantic_merge.sql:53-56` 逐字 ——
**"The Research PR this merge executes. NOT NULL on purpose: main (and every other target branch)
advances only this way (docs/09 §3/§4)"**。合并提交在 `internal/application/merge/service.go:570-590`
用 `BaseStateID: &targetHead` 落笔：**新状态的 parent 是合并前的目标头，不是提案**。

两条合起来就是结论：合并产生的状态 M 的血缘是 `{M, 合并前的 main 头, …}`，
**提案状态 P 永远不在里面**。所以对"经合并进入 main"的每一个状态，
`ListReleaseReviews` 恒为空——而 main 上的状态**全部**是这么来的。

## 影响：不是记录难看，是发布被自己的门拒掉

同一份记录还喂 release gate：`internal/application/releases/service.go:309-318` 把它变成
`rsgvalidation.ReleaseFacts.ReviewApproved`，再进 `validator.go:162` 的判定。
于是在真实产品路径上（提案 → 评审批准 → 合并 → 发布），**发布拿不到自己已经把关过的评审**，
门的答案是"没有记录"。

为什么测试没抓到：T0605 的契约（`tests/integration/release_store_test.go:22-32`）与 T0606 的 E2E
（`tests/integration/release_e2e_test.go:447-451`）钉的都是**合并之前**的形状——
PR 的 `proposed_state_id` 恰好就是 main 的那个头（`f.seedPR(t, ctx, 1, f.genesis, head1)`）。
"合并产生新状态"这条真实路径**一条测试都没有**，所以绿的是被覆盖的那一半。

## Decision

1. **一个状态的验收记录 = 血缘内状态所对应 PR 的评审 ∪ 血缘内状态作为 `semantic_merges.result_state_id`
   的 merge 行所属 PR 的评审。** 也就是让 `semantic_merges.result_state_id → pull_request_id` 这条边可读，
   而不是绕道去猜提案。
2. **既有那一支不许删**：`proposed_state_id IN lineage` 是 T0605 写下的契约，撤掉它会让另一类状态失去记录。
   本 ADR 是**加**一条边，不是替换一条。
3. **顺序与分组不许漂**：仍按 PR number、再评审时间、再行 id 排序；同一 PR 只有一组；
   分组走 `internal/persistence/release_store.go:63-74` 的同一个 helper——那段注释逐字写着它存在的理由：
   release gate 与 knowledge publish（`internal/persistence/knowledge_publish_store.go:143`）是**两个读者**，
   它们不许对同一份记录有分歧。
4. **门的判定强度一处不动**：不改 `validator.go:162` 的 required kinds，不因为"这个状态是合并来的"免检。
   修的是**记录**，不是判据。

## Alternatives

- **改 merge，让提案状态也进 main 的血缘**（把合并提交挂在提案上）：会改掉 frozen main 的推进语义，
  动的是 `docs/09 §3` 的结构性规则，而收益只是让一个查询少写一个分支。否决。
- **在 release 侧放宽判定**（例如"合并过的状态免检评审"）：用放宽 Gate 掩盖缺陷，`CLAUDE.md` §6 逐字禁止。否决。
- **把评审行复制一份挂到合并结果状态上**：等于让"同一条评审"有两个家，两个家会漂；合并行已经存着
  `result_state_id`，读得出来就不该复制。否决。

## Consequences

- 记录的读多一条来源，`reviews` 段会**变长**：一个状态现在会带上把它带进 main 的那些 PR 的评审。
  这是修正，不是扩张——这些评审本来就是这次发布的评审依据。
- 合并链上**每一次**合并的评审都会出现在后续状态的记录里（血缘内每个 merge 边都可读）；
  这是有意的：一次发布固定的是**整条被接受的历史**，不是最后那一步。
- 无需迁移：`semantic_merges`（00069）已经存了 `result_state_id` 与 `pull_request_id`。

## Migration / Exit Strategy

无需数据迁移。退出策略：若将来 release 的验收记录改由别的源（例如独立的 acceptance ledger）产生，
本 ADR 的规则换成那条即可，读的接口（`ReleaseReviewPort.ListReleaseReviews`）不变。

## 落地任务

T0611（P6）——"Release 的验收记录必须覆盖「经合并进入 main」的状态"。
它是 T0608 第 6 段（合并之后经 Releases 表单切片）走不通的上游原因；T0608 在它合并后解封。
