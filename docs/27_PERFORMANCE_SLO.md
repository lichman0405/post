# 性能与 SLO

V1 是工程目标，不承诺公网 SLA，但应满足以下开发验收。

## 页面
- 普通 Project Overview p95 API < 500ms（不含大图/LLM）。
- 常规列表 p95 < 400ms。
- Object detail p95 < 500ms。
- Research Map initial aggregated query p95 < 1s，复杂展开可异步。

## 搜索
- Candidate retrieval p95 < 2s（seed/中小规模）。
- Evidence-backed Answer 若调用 LLM，首个 loading state < 200ms；完整答案目标 < 10s，超时提供 structured results fallback。

## 写入
- 常规 semantic command p95 < 800ms（不含 blob upload）。
- PR validate 可异步，用户获得 job id/progress。

## 容量基线
单 Project 至少支持：10k Scientific Objects、100k relations、10k state transitions、1k branches/PR history（active branch 可限制）。Network seed 至少 100 Projects/100k searchable entities 仍可完成 V1 benchmark。

## Blob
支持 multipart upload；单 blob V1 目标至少 10GB（具体 cloud limit adapter 化）。Web preview 只对安全/合理大小生成。
