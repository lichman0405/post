# ADR-022：四层测试 Gate

**Status:** Accepted

G1 Worker Local、G2 Supervisor Acceptance、G3 Integration/E2E、G4 Merge。Worker 自报 completed 不是完成；Supervisor 必须独立验收。禁止通过 skip/弱化测试推进状态。
