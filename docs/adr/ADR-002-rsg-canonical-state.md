# ADR-002：RSG 是科研状态模型，Graph DB 不是 canonical source
Status: Accepted

## Decision
Canonical state 必须可由 versioned objects/relations/manifests/blob refs/Git refs 重建。Postgres 保存规范化历史和 projection；未来 Graph DB 仅作可重建索引。

## Why
支持 export/self-host/federation，避免把平台锁死在特定图数据库。
