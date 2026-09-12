# V1 Scientific Object 详细规格

所有对象继承 CoreScientificObject。完整机器 Schema 见 `specs/schemas/`。

## CoreScientificObject

必备：global_id、type、version、project_id、schema_ref、title、lifecycle_state、created_by、created_at、updated_by transition、visibility_policy_ref、metadata completeness、tags、relation refs。

Draft 可缺部分 domain field；进入 PR/main/release 时按 validation gate 逐步增强。

## ResearchQuestion

字段：statement、purpose/context、parent_question、scope、status(open/partially_answered/unresolved/superseded/aborted)、related hypotheses/findings/issues。不能使用 Issue close 来代表问题已科学解决。

## Hypothesis

字段：statement、question_ref、scope、proposer、hypothesis_type、current assessment、competing/refines/derived relations。Hypothesis 不“转化覆盖”为 Claim；后续 Claim 与其建立 tested_by/supports 等历史关系。

## Material

字段：name、formula(optional)、structure refs、identifiers、material family、aliases、source/reference、properties summary。结构 blob 大小独立处理。

## Sample

字段：material_ref、batch/sample id、preparation/protocol refs、parent sample、physical form、custody/location（V1 可 optional）、status。

## Experiment

字段：objective、operator、sample/input refs、protocol version、instrument refs、conditions、start/end、raw output refs、processed output refs、result summary、provenance completeness。实验失败仍是有效 Experiment。

## Calculation

字段：objective、software、method、input material/structure refs、protocol/parameter set、environment summary、run refs、outputs、result summary。V1 不执行计算，但允许记录外部 run id/provider metadata。

## Dataset

字段：purpose、data type、row/item summary、schema/profile、blob refs、derived_from、quality notes、access level。Project Dataset 可 later publish as Research Asset。

## Protocol

字段：purpose、domain、steps/parameters、inputs/outputs、equipment/software requirements、version notes。Protocol 修改必须新 version；不同条件可保留多个版本而非强行覆盖。

## Claim

规则：一个可独立判断的命题。字段：statement、claim type(descriptive/associational/predictive/causal/mechanistic/quantitative/comparative)、subject、predicate/property、value(optional)、scope/qualifiers、origin、assessment、evidence assertion refs。

如果一句话可被 reviewer 部分同意部分反对，应拆 Claim。

## Finding

字段：human-readable statement、finding type、contained claim versions、status(preliminary/accepted/contested/unresolved/superseded/aborted)、summary scope。Finding 是人工确认的正式对象；Agent dynamic summary 不是 Finding。

## ExternalReference

字段：source type(publication/patent/database/standard/vendor/web/other)、external identifier、canonical URL/identifier、accessed_at、upstream version、snapshot metadata/hash、rights on cached copy、reference status。
