# Research Event、订阅与通知

## 1. Event Bus

所有重要 domain state change 发出结构化 ResearchEvent，通过 transaction outbox 保证与 DB commit 一致。Consumer 必须幂等。

## 2. V1 事件

事件的**规范清单**是 `specs/events/event-types.yaml`（机器可读，连 envelope 必填字段一起定义）；
下面只是它的分组导读，**新增或改名一律改 YAML，不改这里**。

- Project / Branch / State：`project.created`、`project.visibility_changed`、`project.main_frozen`、`branch.created`、`branch.aborted`、`state.committed`
- Research PR：`pull_request.opened`、`pull_request.reviewed`、`pull_request.merged`
- 科学对象：`scientific_object.version_created`、`scientific_object.aborted`、`scientific_object.reopened`
- Evidence / Release / Asset：`evidence_assertion.created`、`release.published`、`research_asset.version_published`
- Knowledge：`knowledge.version_published`、`knowledge.external_evidence_added`
- Dependency：`dependency.status_changed`、`dependency.impact_detected`
- Contribution / Credit / Rights / Policy：`contribution.accepted`、`credit.dispute_opened`、`credit.dispute_resolved`、`rights.visibility_changed`、`policy.version_published`
- Webhook：`webhook.delivery_failed`

（本节早先的散文写法与词汇表整体漂移——`pr.opened`、`main.frozen`、`asset.version.published`、
`knowledge.published`、`external_evidence.added`、`object.aborted/reopened` 都是**同族的另一种拼法**，
不是独立事件；`claim.assessment.changed`、`finding.contested` 则是 claim/finding 的**状态取值**，
从来不是事件名。以 YAML 为准。）

## 3. Subscription

用户可 follow Project/Asset/Knowledge/Person/Organization，并选择 event categories。默认避免通知每个低级 commit。

## 4. 输出接口

- Web Inbox
- Email immediate/digest abstraction
- RSS/Atom（公开对象/项目）
- Webhook（签名、重试、delivery log）

## 5. Dependency Watch

当上游 dependency abort/supersede/new version/rights restriction 时，分析受影响下游，并创建 alert。系统只标记 review required，不自动改科学结论。
