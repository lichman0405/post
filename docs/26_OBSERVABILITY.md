# 可观测性与审计

## 1. 三类记录

- Application logs：工程运行。
- Domain audit log：谁对何科研状态做了什么。
- Research events：可订阅的业务事件。

三者不能混成一个日志表。

## 2. Correlation

每个 request/command/job/event 携带 request_id/correlation_id/actor_id/project_id（如适用）。跨 API→queue→Git/S3 保持 trace。

## 3. Metrics

HTTP latency/error、DB pool/query、queue depth/failures、Git ingestion lag、blob upload/failure、outbox lag、webhook success、search latency、LLM planner failures、RSG reconciliation drift、permission denied rates。

## 4. Domain metrics

只用于产品运营，不作为科研质量分数：projects created、PR merge time、assets published/reused、external contributions、reproductions、search→project conversion。

## 5. Audit

高风险：visibility、rights、policy、member roles、main freeze、merge、release、publish、abort/reopen、ownership transfer、credit dispute。Audit append-only，并支持管理员检索。

## 6. Alerting

P1：data visibility leak suspicion、RSG/Git drift、database unavailable、blob integrity mismatch。P2：event/outbox backlog、search unavailable、webhook failures surge。
