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

## 尚未决定

**发布方对 `usage.derivatives` 声明 `unspecified` 时，派生是否放行**——见
`tasks/decisions.md` 2026-09-19 的 T0708 一节。本 ADR 只定存储与接口形状，
它对这三种取值（`allowed` / `restricted` / `unspecified`）的判定不作规定。
