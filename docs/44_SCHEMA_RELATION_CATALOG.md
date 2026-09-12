# Schema 与 Relation Catalog

## Core Relation（V1）

Provenance/structure：uses、produces、derived_from、performed_on、follows_protocol、parameterized_by、part_of、contains、generated_by、supersedes、references、depends_on。

Knowledge：addresses_question、tests_hypothesis、supports、contradicts、consistent_with、inconsistent_with、reproduces、fails_to_reproduce、validates、challenges、contextualizes、competes_with、refines。

Lineage/network：forked_from、derived_asset_from、published_from、originates_from、used_by（projection）。

## Extension
Domain/Project relation 可注册 namespaced type，例如 `materials:synthesized_from`。必须声明 semantics、source/target types、是否参与 provenance/dependency/impact。

Custom relation 若无声明，不进入强推理。
