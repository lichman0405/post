# V2 开发交接清单

产品决定已确认，访谈 4/4 已结束。这里提供可验证的规划输入，**不是已执行的任务或产品测试证据**。

- `backlog.json`：31 个 WEB 工作包、优先级、依赖、需求、验收、调查路径与粗估工作量。
- `web-acceptance.json`：U01–U14 网页旅程，共 81 个步骤及反 API 代行规则。
- `estimate.json`：规划假设与工期范围；未在本仓库对 Kimi 实测。
- `validate_plan.py`：校验清单、DAG、覆盖与文档引用；只验证文档一致性，不运行产品 Gate。
- `mapping.json`：已注册的 WEB→T→Issue→PR 追溯映射。31 个包对应 T1301–T1331；
  WEB-00 另有 Supervisor 盘点切片 T1332。Issue/PR 字段在派工时填写。
- `kimi-precheck.json`：WEB-28 的模型/工具/恢复预检证据（初始为空，未运行不记 passed）。

入口为根 `V2_CLAUDE_CODE_PROMPT.md`，完整计划见 docs/70，执行协议见 docs/72。

## 正式注册规则

1. Supervisor 为 WEB 工作包建立未冲突的正式 T 编号，可细分为多个任务，保存完整映射。
2. 运行真相源保持 `tasks/tasks.json`、`task_status.json`、`tests.json` 和受控 runtime；不得创建另一个互相漂移的 V2 运行状态账本。
3. `implementation_scope_hints` 只供调查，须核对实际路径并收窄；精确允许范围、基准 SHA、迁移号、测试命令、相关规范由 Supervisor 在派工前固化。
4. 每个正式任务有 Issue／分支／Commit／PR／Merge 与证据链。V2 必需范围必须有受测的独立投影，不能改写历史 V1 分母或依赖执行器未消费的字段。
5. 未实现的最终场景保持 `not_run`；按功能切片逐步接入 blocking CI，所有 U 场景在最终 WEB-25 必须通过。WEB-01 的 harness 完成不等于未来全部功能已实现。
6. 任何本次范围内的新接口／字段等技术细化先按既定语义完成契约；只有新的 L3／重大架构／凭据等才停受影响路径。

## 校验

```bash
python3 tasks/v2/validate_plan.py
```

Windows 的文档检查可用 `python`；产品开发／Gate 使用 Ubuntu 或 WSL2 的 Linux 文件系统。这里的校验成功不能证明实际 Kimi、产品网页、部署或 GitHub 交付链通过。
