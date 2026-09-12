# ADR-020：PostgreSQL 是科研语义 Canonical Store

**Status:** Accepted

RSG 是图模型，但 V1 不引入专用 Graph DB。PostgreSQL 保存 Scientific Object/version/relation/evidence/rights/contribution/current projection/domain events。Git 管文件真相，S3 管大 blob。Release 绑定三者 hashes/versions。
