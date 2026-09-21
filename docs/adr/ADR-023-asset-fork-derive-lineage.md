# ADR-023：Asset Fork/Derive 是一次动作，谱系落在 asset_lineage
Status: Accepted

`docs/11 §5`「Fork/Derive：创建新的 Asset/Object identity，保留 lineage」的落地方式：
**是一次动作，不是一次声明。** 动作创建新的 asset version（即新的 identity），并在同一事务里
往 `asset_lineage`（`infra/migrations/00010_releases_assets.sql:49`）写**一条**边，
`relation_type` 取该表 CHECK 里的 `forked_from` 或 `derived_from`。

## 为什么不写进发布清单

- `docs/11 §5` 要的是**新 identity**——清单字段挂在既有 version 上，创建不了 identity。
- `research_asset_versions.origin_refs` 装不下这件事：它的词表是**封闭的四类**
  （`project` / `release` / `state` / `object_version`，见 `internal/assets/origin.go`），
  回答的是「这个版本**发布自**什么」，而且**发布后不可变**——记录不了此后才发生的派生。
- `asset_lineage` 就是为这件事建的表，三种关系已在它的 CHECK 里；`00086_external_fork.sql`
  的头部逐字说明项目级 fork 由 `project_forks` 承担，**资产级那一半留给 `asset_lineage`**
  （"The asset-level half stays asset_lineage's (00010) — the two are different subjects"）。

## 两套谱系名字保持不同

`asset_lineage.relation_type` 用 `derived_from`；领域关系目录（`docs/44`、
`internal/rsg/relationcatalog/catalog.go:106`）用 `derived_asset_from`。**两个主体、两张表，
不合并、不互改**：前者是资产版本之间的谱系边，后者是可推演的领域关系类型。

## `usage.derivatives` 的三个取值：三种行为，一个都不许坍缩

（2026-09-21 裁定。原文是「尚未决定」，见 `tasks/decisions.md`；判定依据与派生过程记在这里，
以便有人复核或推翻。**本 ADR 仍然只定存储与接口形状**——下面这条是**判定**，不是新的存储约束。）

`internal/rights/usage.go` 的包注释逐字写着 `"unspecified" is a real answer, not a missing one`，
并**同时**否定两种坍缩（`collapsing the two would invent a permission nobody granted`）：

| 发布方的声明 | 派生请求的结果 |
| --- | --- |
| `restricted` | **拒绝**（发布方声明了禁止衍生） |
| `allowed` | **放行**（发布方声明了允许） |
| `unspecified` | **既不拒绝也不放行**：要求请求方在请求里带一条**显式确认**，确认内容与 actor 一并写进**同一事务**的 audit 行；没有确认就拒绝 |
| `rights_json` 读不出来（缺失 / 解析失败 / `internal/rights.Parse` 拒未知字段） | **拒绝** |

**为什么是「显式确认」而不是替发布方选一边**：仓库自己禁止两种坍缩——当 `allowed` 用是
发明一条没人给的许可，当 `restricted` 用是发明一条没人声明的禁令。既然两个默认值都被禁止，
剩下的唯一不撒谎的行为就是**不静默决定**：把「这次派生是有人确认过的」这件事记下来，
让判断留给人。这与 `docs/12 §4`「字段不替代法律合同」、`internal/rights/doc.go`
「the platform records the declaration and does not judge it」是同一条线。

**这条裁定是 fail-closed 的**：它在任何一档都不新增权限，`unspecified` 在带确认之前等同于拒绝。
一个可能过严的默认值是可纠正的（补一条确认即可），一个过宽的默认值不可纠正（数据已经流出去了）——
所以不确定时统一往严的那一侧取。

**由谁承担**：这是按 `CLAUDE.md §5.1` 的授权做出的 Supervisor 裁定（L3 邻域），
依据全部来自仓库里已经写下的约束，不是新发明的产品语义。若 owner 认为 `unspecified`
应当另有默认，**改这一个格子即可**，不需要动存储或接口形状。
