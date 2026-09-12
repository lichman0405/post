# Provenance Graph 与 Evidence Graph

## 1. 不得混淆

Provenance 回答“从哪里来、谁做了什么”。Evidence 回答“为什么这个证据与某个科学命题相关”。两者可以共享节点但关系语义必须分开。

## 2. Core provenance relations

`uses`、`produces`、`derived_from`、`performed_on`、`follows_protocol`、`parameterized_by`、`part_of`、`generated_by`、`references`、`supersedes`。

## 3. Evidence Assertion

字段至少包括：target knowledge object/version、evidence source/version、relation type、evidence type、scope、directness、inference nature、author/evaluator、review state、reasoning note、external/internal、visibility、created transition。

Evidence source 可以是 Experiment、Calculation、Dataset、External Reference 的具体 evidence excerpt/snapshot、另一个 Claim 等。

## 4. Evidence relation types

- supports
- contradicts
- consistent_with
- inconsistent_with
- reproduces
- fails_to_reproduce
- validates
- challenges
- contextualizes

V1 不自动赋数值权重。

## 5. Causal claims

Causal/Mechanistic Claim 必须声明 causal basis，例如 controlled intervention、temporal ordering、confounders considered、dose-response、mechanistic characterization、computational intervention。Agent 可警告“目前 evidence 只有 association”，不能自动降级/升级而不经确认。

## 6. Literature evidence

不能 `DOI -> supports Claim`。必须定位文献中的具体 evidence unit：figure/table/results assertion/supplementary dataset/method。保留 source pointer、snapshot/excerpt metadata、human confirmation。

## 7. Network external evidence

Published Knowledge Object 页面区分 Origin Evidence、Reviewed External Evidence、Unreviewed External Evidence。原发布者不能删除外部反证；只能对其本项目 assessment 负责。

## 8. Network Evidence State

平台可按透明规则产生描述性标签（limited evidence、mixed evidence、actively contested、independently reproduced），不得产生隐式 Truth Score。
