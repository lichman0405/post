# ADR-021：Append-only Domain Event Log，而非纯 Event Sourcing

**Status:** Accepted

正常 relational current state + append-only versions/domain events。状态变更与 outbox 同 transaction。读取 current state 不通过全量 event replay；event log 用于 audit、activity、notifications、indexing、impact、future federation。
