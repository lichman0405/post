# ADR-013：Domain Event 使用 Transactional Outbox
Status: Accepted

所有 Research Event 与 DB state 同事务写 outbox，由 worker 异步投递。Consumer 幂等；不以“先发 Kafka/Redis 再写 DB”作为一致性机制。
