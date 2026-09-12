# ADR-007：正式科研历史 append-only，使用 Abort/Reopen
Status: Accepted

不实现普通 delete/retract 作为科研语义。错误、失败、放弃通过 abort event 记录；后续可 reopen，但不能抹掉 abort 历史。
