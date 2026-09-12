# ADR-004：Web Files 视图严格只读
Status: Accepted

## Decision
Web Files 无编辑、上传、覆盖、删除。科研状态写入来自 Agent/API/结构化治理操作/Git compatibility ingestion。

## Reason
避免文件级 mutation 绕过 RSG、Schema、Provenance、Review。
