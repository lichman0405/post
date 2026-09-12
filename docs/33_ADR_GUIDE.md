# ADR 规则

重大且难逆转的架构选择必须写 `docs/adr/ADR-NNN-*.md`：Context、Decision、Alternatives、Consequences、Migration/Exit Strategy、Status。

需要 ADR 的典型情况：替换 DB/Git backend/auth/queue、改变 canonical source、修改 immutable/visibility/security invariant、引入微服务、改变 public API compatibility。

普通组件库选择、小重构写 `tasks/decisions.md` 即可。
