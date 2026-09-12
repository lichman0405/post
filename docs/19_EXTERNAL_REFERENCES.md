# External Reference 规范

## 1. 来源类型

Publication/DOI、Patent、External Database Record、Standard、Vendor Datasheet、Web Resource、Other。

## 2. Live identity + pinned snapshot

保存 canonical external id/url 作为 live identity；同时保存被项目实际引用时的 accessed_at、upstream version、metadata snapshot、hash、允许情况下的 cached file ref。

## 3. Citation vs Dependency

`cites`：背景/知识引用；上游变化只提示。

`depends_on`：实际输入/方法/复现依赖；上游变更触发 impact analysis。

## 4. Evidence extraction

External Reference 不是自动 Evidence。需要 Evidence Assertion 指向具体 location/excerpt/figure/table/dataset/method，记录 human confirmation 和 relation。

## 5. Retraction/update

定期或手动 refresh live metadata；若上游出现 retraction/correction/version change，产生 event 并分析 active dependencies/citations。V1 可先支持手动 refresh + DOI metadata adapter，自动 monitor 后续迭代。
