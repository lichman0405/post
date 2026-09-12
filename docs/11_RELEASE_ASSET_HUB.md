# Release 与 Research Asset Hub

## 1. Release

Release = immutable accepted RSG snapshot。Release manifest 固定 objects/versions、relations、blob hashes、git refs、policy/schema versions、review/approval record、rights snapshot。

Release 不是“报告”；Agent 可基于 Release 生成可读 summary/export，但 summary 不改变 release identity。

## 2. Research Asset

V1 类型：Dataset、Protocol、Material Collection、Benchmark。

Project 中对象通过 `Publish as Asset` 成为 network reusable object。每个 Asset 有 persistent ID；每个 Asset Version immutable。

## 3. 发布门槛

至少校验：source accepted state/release、version fixed、provenance、creators/contributors、rights/license、visibility、dependency pin、hash、required metadata、schema validation。

## 4. Metadata revision

Scientific Version 不可变；描述、keywords、cover、contact、documentation 等 Asset Metadata 可独立 revision，保留 audit，不产生新的 scientific version。

## 5. Reference / Dependency / Fork

- Reference/Use：引用固定 Asset Version，不修改。
- Dependency：项目复现/运行所需，参与 impact analysis。
- Fork/Derive：创建新的 Asset/Object identity，保留 lineage。

## 6. Asset Governance

分离 Rights Holder、Custodian、Maintainer、Creator、Contributor、Originating Project。Ownership transfer 是 append-only governance event；Creator/history 永久保留。

## 7. Abort/Supersede

Published Asset Version 不删除。若出现问题，追加 abort/supersede state/event并指向replacement；过去引用仍解析到原 version，同时显示 later status warning。
