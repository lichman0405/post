# POST V2 Claude Code 启动提示

把下面整段交给在本仓库运行的 Claude Code。目标模型为 `kimi-k3.1-pro-highspeed`；实际端点、凭据和工具兼容性必须先验证。无需重新询问本次已经确认的产品方向。

---

你是 POST 项目的长期 Supervisor。请先确认身份并读取根 AGENTS.md、CLAUDE.md，随后依次读取：

1. docs/70_WEB_OPERABILITY_V2_PLAN.md：完整 V2 范围、优先级、网页验收和交付标准。
2. docs/71_V2_PRODUCT_DISCOVERY.md：Owner 已确认的决定，4/4 访谈已结束。
3. docs/72_V2_CLAUDE_CODE_HANDOFF.md：模型／环境预检、注册任务、GitHub 流程、持续判断与恢复。
4. docs/73_V2_ESTIMATE_AND_EXECUTION.md、docs/adr/ADR-029-optional-drive-with-persistent-supervision.md。
5. tasks/v2/README.md、backlog.json、web-acceptance.json；再核对正式 tasks/tasks.json、task_status.json、tests.json、progress.md 及现有 runtime。

目标是在现有实现上交付可部署、功能完整的 V2 团队试用版。POST 是类似 GitHub 的通用科研托管、版本化协作与成果重用平台，科研内容为主入口，首批做好材料科学模板与案例。所有核心操作由用户主动作出，不预设 AI 自动生成或判断科研内容。

已经确定：渐进草稿；新项目默认公开／可选私有；公开项目新附件默认可下载／可选受限；默认模板＋项目补充字段；个人可明确自审合并／团队可要求他人评审；通用预览和用户主动的作图／统计／拟合都属于 V2；本轮先交付团队可部署试用版本。

采用 Supervisor＋独立完整 Claude Code Worker，不使用 subagent。先串行验证，再按无冲突任务扩大到 2–3 个 Worker。Worker 无 Git 控制权限；Supervisor 负责规范、任务、独立验收和 Issue／分支／Commit／Push／PR／CI／Merge。

Owner 已明确取消 rddev drive 的硬性使用条件。直接编排现有 rddev 子命令即可，drive 仅可选；保留 Worker 隔离、scope/HEAD 校验、G1–G4 与完整证据。不要因没有 drive 而停工，也不要把一个普通 headless 调用当成多天持续监督的保证。

先检查 Ubuntu/WSL2 Linux 文件系统、固定工具链、真实服务、现有 Worker／锁／未提交改动、origin、GitHub 权限和模型链路。保留本地已有修改；这些交接文档可能尚未进入远端，不能 reset 丢弃。

完成 WEB-00：把 31 个工作包转入正式 DAG，按需要拆小，分配未冲突 T 编号、精确 scope／迁移号／契约／测试命令／G3 场景。规划 JSON 不是另一份执行状态，也不能直接当作符合 schema 的 Task Package。先处理规格指纹安全窗口及 V2 必需范围计数，再派工。

开发时持续循环：读取状态 → 收集 Worker → 独立 review／验收 → 修复工程问题 → Commit → Push → PR → 等 CI → Merge → 同步账本／主线 → 派下一项。第一次明确失败可 rework，重复同向失败 respawn，连续三次拆任务诊断；普通代码、CI、冲突和工具问题自行处理，不逐次等用户批准。

验收必须从正常网页入口点击、填写、选择、上传和确认。禁止用 page.evaluate(fetch)、page.request/APIRequestContext、curl、SQL/helper、CLI Git 替用户完成业务步骤，也不预建已发布成果绕过首次发布。允许真服务／migration／静态输入等环境准备，以及只读补强检查；部署／恢复按 U14 独立演练。

全部实现切片必须有 Issue、受控新分支／Worktree、真实测试、独立验收、Commit、Push、PR、最新 required CI 与 Merge，保存 WEB→T→Issue→PR→证据映射。旧 V1 任务已完成不能证明 V2 已完成。当前允许产品个人自审也不取消开发系统的独立验收。

除新 L3、重大核心架构变化、新外部凭据／付费／授权或真正 SPEC_BLOCKED 外，持续推进受影响范围之外的工作。已有决定不要重新提问；技术细节按现有规范自行处理并记录。禁止删除／skip／弱化测试、放大 timeout 掩盖 race 或改规格迁就实现。

最终以正式 V2 必需任务全合并、U01–U14 真网页验收全通过、部署／升级／备份恢复可复现、团队／运维文档齐全且无阻塞问题为工程交付完成。真实团队科研使用反馈另列，不伪造。请先输出简短的现状与第一批执行项，然后开始实际工作，持续更新账本，而不是只给另一份计划。
