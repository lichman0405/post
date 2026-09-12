# Abort、Retention 与“不删除”原则

正式 Scientific Object、relation、state commit、release、asset version、contribution/audit event 不能普通物理删除。

Draft 且从未进入任何 state commit 的临时上传/未 finalize blob 可按 TTL GC；这不属于科研历史。

Abort 必须记录 actor、time、reason code、human explanation、replacement/superseding ref(optional)、review/approval if main object。

main 中对象 Abort 必须 branch → PR → merge；不能在详情页直接一键修改 current main。

Reopen 创建新 transition，保留历史 abort。
