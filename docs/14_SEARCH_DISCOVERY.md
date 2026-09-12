# Search、Explore 与 Evidence-backed Answer

## 1. V1 搜索范围

只搜索平台 network 内 public/accessible content + 已正式纳入项目的 External Reference snapshots。不做全互联网 scholar crawler。

## 2. Search Planner

自然语言问题先解析为：intent、target object、property、condition/scope、evidence preference、network scope、visibility、ranking constraints。

然后执行：structured filters + FTS + semantic candidate retrieval + graph traversal + scientific ranking。

## 3. Ranking

主要考虑 query/scope match、evidence profile、review state、independent reproduction、contradictory evidence、version/freshness。不得主要按 popularity/star/organization prestige。

## 4. Answer Page

必须包括：直接回答、comparison、conditions、origin assessment、network evidence state、conflicting/limited evidence、source objects、Research Map、underlying results、Start Research Project。

Agent summary 明确标注为 View，不创造新 Claim。

## 5. Search → Project

“Start Research Project”创建 Draft Research Context：Research Question、referenced knowledge、dependencies、candidate materials、known uncertainties、agent-suggested hypotheses。用户确认后才形成 initial state。

## 6. Explore

面向开放网络：Open Projects、Assets、Knowledge Objects、Open Contributions、People、Organizations。普通外部贡献者从“可以做什么”进入，而不是申请项目成员。
