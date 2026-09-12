# Research Event、订阅与通知

## 1. Event Bus

所有重要 domain state change 发出结构化 ResearchEvent，通过 transaction outbox 保证与 DB commit 一致。Consumer 必须幂等。

## 2. V1 事件

project.created、branch.created、pr.opened/reviewed/merged、main.frozen、release.published、asset.version.published、knowledge.published、external_evidence.added、claim.assessment.changed、finding.contested、object.aborted/reopened、dependency.status_changed、rights.visibility_changed、contribution.accepted。

## 3. Subscription

用户可 follow Project/Asset/Knowledge/Person/Organization，并选择 event categories。默认避免通知每个低级 commit。

## 4. 输出接口

- Web Inbox
- Email immediate/digest abstraction
- RSS/Atom（公开对象/项目）
- Webhook（签名、重试、delivery log）

## 5. Dependency Watch

当上游 dependency abort/supersede/new version/rights restriction 时，分析受影响下游，并创建 alert。系统只标记 review required，不自动改科学结论。
