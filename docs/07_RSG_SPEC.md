# Research State Graph (RSG) 规范

## 1. 定义

RSG 是 Project 在某一 state version 下的完整科研状态图。canonical state 可由开放 Manifest + Blob/Git references 重建；Graph DB 只能作为索引，不得成为唯一真相源。

RSG = Versioned Scientific Objects + Versioned Relations + Policy/Schema references + Blob references。

## 2. 节点

节点由 `global_id + object_type + object_version` 唯一定位。Project 内 local slug 只是可读别名，不是身份。

每个对象版本至少含：id、version、project、branch/state、schema id/version、title、status、created_by、created_at、metadata、visibility policy ref、integrity hash。

## 3. 边

Relation 必须是 typed、directed、version-pinned。边有自己的 global id、relation type、source version、target version、scope/metadata、creator、state transition。

禁止所有关系退化为 `related_to`。`related_to` 只能用于无法标准化的弱关系，且不得参与强 provenance/dependency 推导。

## 4. Canonical serialization

每个 accepted state/release 必须能导出 manifest：project metadata、objects、relations、schema refs、policy refs、blob refs、git refs、hash tree。具体 JSON Schema 见 `specs/schemas/rsg-manifest.schema.json`。

## 5. State transition

任何写操作产生 Domain Event/State Transition：actor、via agent/client、base state、branch、operation set、result state、timestamp、correlation id、audit context。

## 6. Immutability

对象旧版本不可原地更新；update 生成新 version。Release/Published Asset Version 的 referenced versions 永久固定。

## 7. Materialized views

为性能可建立：current branch object table、search index、graph edge index、network evidence aggregation、contribution projection。所有 projection 必须可从 canonical event/state 数据重建。

## 8. Merge

RSG Merge 使用三方语义 diff：base、source branch、target。先判断 object/relation field-level non-overlap，再检测 domain conflict。科学冲突不得自动选 winner。
