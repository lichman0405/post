# 数据模型与数据库不变量

## 1. 数据域

Identity、Organization、Project、RSG/Object、Git State、Review、Release、Asset、Rights、Contribution、Network Evidence、Search、Events/Audit。

机器 DDL：`specs/database/postgres.sql`。

## 2. ID

所有 network/domain entity 使用全局稳定 ID（建议 UUIDv7/ULID）。Slug/URL 可变，不作为引用身份。版本对象使用 `(entity_id, version_no)` 或 immutable version id。

## 3. 主要表

- users, profiles, organizations, organization_memberships
- programs, projects, project_memberships, policies, policy_versions
- branches, state_commits, project_states
- scientific_objects, scientific_object_versions
- relations, relation_versions
- blobs, blob_attachments
- issues, pull_requests, reviews, validation_results
- releases, release_items
- research_assets, research_asset_versions, asset_lineage, asset_dependencies
- knowledge_publications, external_evidence_links
- external_references, external_reference_snapshots
- contribution_events, credit_attributions, credit_disputes
- subscriptions, research_events, outbox_events, webhook_deliveries
- audit_log

## 4. Append-only 部分

`state_commits`, object_versions, relation_versions, release manifests, asset_versions, contribution_events, audit_log 不 update semantic content；修正通过新记录。

## 5. Current projection

允许维护 current object head、current relation head、branch current state、current assessment 等 projection，方便查询。projection 可 rebuild，不等同历史 source of truth。

## 6. 生命周期

不使用硬 delete cascade 删除科研历史。FK delete 行为默认 RESTRICT；账号注销/PII 删除采用 identity anonymization 与 legal policy，不能删除贡献事件本体。

## 7. Scope & qualifiers

Claim/Hypothesis scope 使用结构化 JSONB + normalized common fields（temperature, pressure 等可由 domain schema projection 建索引）。避免把所有 domain 字段塞进单一 EAV。

## 8. Schema refs

每个 object version 固定 schema id/version；旧 schema 数据始终有效。Schema migration 不重写历史，只能产生新 object version 或 compatibility projection。

## 9. Permissions

Authorization 不通过“查询后在前端隐藏”实现。所有 repository/service query 必须接受 principal/context，并在 DB/service 层过滤。Search index 也必须做 visibility-aware materialization 或 post-filter with guaranteed no leakage。

## 10. Hash/integrity

Object version canonical JSON、relation set、release manifest、blob content 使用 hash。Hash 算法版本记录，以便未来升级。
