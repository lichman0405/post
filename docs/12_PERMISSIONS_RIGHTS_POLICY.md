# 权限、Rights 与 Organization Policy

## 1. 权限层次

Access Role、Object Visibility、Data Access、Usage Rights、Scientific Responsibility、Organization Policy 必须分离。

## 2. Project visibility

V1 UI：Public / Private 两个 preset。底层使用 policy id/scope，不用单一 `is_private` 作为永久模型。

Public Project：accepted RSG 与公开研发历史可见；Blob/data access 可 Open/Restricted。

Private Project：project/RSG/private blobs 默认不可见；可显式 Publish Asset/Knowledge/Attestation。

**已发布 attestation 的可见面**：V1 的 attestation 页面**只按编号（`pid`）可达**，不提供列表、
搜索或导航入口。「只有知道编号才能打开」是**产品决定**，不是缺口（`L3-④` 裁定，`docs/02` §4）；
新增任何发现面都是一次可见性扩大，须走 §3。

## 3. 不扩大可见性原则

任何 private→public、restricted→open、named access→public 都要求有权限的人显式确认，并产生 audit/event。Agent/MCP 不得自动执行此类扩大操作。

## 4. Usage Rights

V1：标准 license + machine-readable declaration + custom agreement ref。机器字段：commercial use、derivatives、redistribution、model training、attribution、patent grant note。字段不替代法律合同。

## 5. Organization Policy

Organization policy 是最低治理要求。Project policy 可更严格，不能静默放宽。Policy 版本化；Release 绑定当时 policy version。

例：main protected、release min reviewers、raw data retention、public asset IP review、required schema profile。

## 6. Future collaboration

数据模型需预留 multi-org project、organization-private compartment、joint publication approvals、private evidence attestation，但 V1 UI 不完整实现。
