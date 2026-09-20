# ADR-027：一个项目的 RSG 按「它自己的状态谱系」枚举，不按容器所属项目
Status: Accepted

## 背景

T0817（外部 fork 提案的合并路径）把「源侧按源分支自己所在的项目读」接通之后，
独立评审在 approve 的同时报出一条 major 发现。我按它给的引用逐环复核，每一环都成立：

- **落地的版本行挂在源（fork）谱系的容器上**：`materialize` 调用
  `s.objects.CreateVersionInTx(ctx, tx, c.TargetID, …)`（`internal/application/merge/service.go:743`），
  而 `c.TargetID` 就是计划里的 `c.ObjectID`（`internal/rsg/merge/merge.go:342`）。
- **写入层不校验容器属于哪个项目**：`CreateVersionInTx`
  （`internal/persistence/scientific_object_tx.go:82`）对拿到的容器做计数器 CAS（`:87-96`）再插版本行。
- **读法因此分叉**：`ListObjectVersionsAsOf` 的谓词是 `WHERE so.project_id = @project_id`
  （`internal/persistence/queries/rsg_query.sql`），`ListRelationVersionsAsOf` 同形——
  **上游项目按项目域读看不见自己 main 上刚合进来的内容**（`rsg.Service.Query`、
  `ResearchOutline`、search、knowledge publish 都走它），而按 state 读的 manifest
  （`internal/persistence/queries/manifest.sql:19-27`）看得见。

同一个状态，两条读法给出两个答案。而 `docs/31_MASTER_ACCEPTANCE.md:17`
（「Public Project 外部用户可 fork/contribute」）要求这条路是通的。

## Decision

1. **容器保持在源（fork）身份上，不在目标项目新建身份。** 这不是偏好，是 ADR-025 的
   Alternatives 已经逐字否决过反面：把 fork 的提案「内部化」到上游项目
   「会造出第二个 source of truth，一次 fork 提案要在两个项目里各有一份状态；
   `pull_request_fork_gate`（00086）存在的理由正是**不许**这样绕。否决。」
   落地版本带着**源身份**进上游状态，正是单一真相源。

2. **一个项目的 RSG 枚举 = 它自己状态谱系所携带的版本，与容器的 `project_id` 无关。**
   承载者是状态（`scientific_object_versions.state_id`），项目是边界与授权面。
   `scientific_objects.project_id` 不能充当「本项目的 RSG 里有什么」的判据——
   它只在容器恰好属于本项目时才与这个问题等价。

3. **两个方向都要成立、都要有断言。** ①上游项目在自己的谱系里看得见落地内容（对象与关系）；
   ②fork 项目**不**因为上游状态而把落地版本渲染成自己对象的 latest（它的谱系里没有那个 state）。
   只做①不做②，等于把「看得见」当成「都看得见」。

4. **授权面不因此放宽。** 枚举范围是「我的谱系携带的版本」，不是「对方项目的所有版本」：
   B 没有合进 A 的版本，A 的读者一条也看不到；版本的 `visibility_policy_id` 随内容走
   （既有规则，T0817 已断言）。

5. **写入侧维持现状**：容器 = 源身份，计数器 CAS 落在该容器上（于是对象的版本序列跨项目仍是**一条**）。
   若实现中发现计数器语义另有问题，**报上来**，不顺手改。

## Consequences

- 读路径出现跨项目 join：按谱系枚举版本，并带上版本所在的对象/关系行。
  `internal/persistence/queries/rsg_query.sql` 的两条查询与其生成物（`internal/persistence/sqlc/**`）
  要改，索引要重新看（`infra/migrations/00036_rsg_query_indexes.sql` 是既有的那批）。
- 「我在这个状态上有什么」与「我的项目里有什么」从此是两个不同的问题：
  前者是 RSG，后者是项目资产清单。界面上若同时展示，必须各自说清。
- 既有断言里凡把 `so.project_id = @project_id` 当作「本项目的 RSG」的，都建立在被本 ADR 取代的
  假设上；改动它们必须逐条说明理由（见 T0818 的任务书），不许为了让新读法变绿而删。

## Alternatives

- **合并时在目标项目新建容器（新 identity）**：就是 ADR-025 已否决的「内部化」，
  且与 ADR-023 的语义打架——「Fork/Derive 创建新 identity」是一次**动作**产生的谱系边
  （`asset_lineage`），不是一次 merge 的副产物；`scientific_objects` 上也没有任何 per-object
  谱系列可以据以「映射回目标侧容器」（`infra/migrations/00005_scientific_objects.sql`：
  `id / project_id / object_type / created_by / created_at`）。否决。
- **把分叉写成「既定表示」**：让同一状态的两条读法永久不一致——上游 outline 看不见自己
  刚刚接受的内容。那等于把主验收里那条「外部用户可 fork/contribute」做成不可见。否决。
- **把 outline 改成按 manifest 读**：看起来最小，实则作废整个项目域读层，
  且 `ListObjectVersionsAsOf` 的调用方（search、knowledge publish）语义仍是项目域。否决。
