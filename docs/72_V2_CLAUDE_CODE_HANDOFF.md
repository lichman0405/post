# POST V2：Claude Code 开发交接与持续执行协议

日期：2026-10-26；版本：1.0。对应产品计划：[70](70_WEB_OPERABILITY_V2_PLAN.md)，已确认决定：[71](71_V2_PRODUCT_DISCOVERY.md)。启动提示：[V2_CLAUDE_CODE_PROMPT.md](../V2_CLAUDE_CODE_PROMPT.md)。

## 1. 执行方式

使用一个长期 Supervisor 与按任务启动的独立完整 Claude Code Worker；Review Worker 也使用独立进程。它们不是 subagent。Supervisor 管理产品上下文、任务、契约、验收及 GitHub 控制；Worker 只产生限定 worktree 内的未提交 diff 与 RESULT。

先用一个 Worker 验证完整交付链，再按冲突面扩大到 2–3 个并行 Worker；硬上限沿用仓库的 4，Reviewer 共用资源。串行阶段不等于改为一个 Agent 同时编码、独立验收和控制 Git。

Supervisor 默认直接编排现有 `rddev task/worker/review/gate/git/pr` 子命令。`rddev drive` 可选，**缺少或未运行 drive 不构成停止条件**。依据是 Owner 明确指示与 ADR-029；无需再次请求是否允许绕过 drive。

禁止把多天开发简化为一次聊天调用或一条“继续直到完成”的提示。必须验证进程持续性、会话恢复和磁盘状态接管；终端断开、上下文压缩、模型请求失败后能够继续未完成工作。

## 2. 启动前检查

1. 确认角色为 Supervisor，读取根 `AGENTS.md`、`CLAUDE.md`、docs/70–73、ADR-029 和正式 DAG／状态／测试／进度账本。Worker 仅接收最小任务包，不灌入完整根 CLAUDE.md。
2. 核对 origin 为 `lichman0405/post`，integration branch 为 main；读取 GitHub 当前 visibility 与权限，保留全部已有本地改动。当前文档基准 c037256 不代表交接时远端仍停在那里。
3. 检查已有 Worker、worktree、分支、锁与待处理任务，先接管，避免两个调度者同时推进同一任务。查明本地规划文件是否已进入 Git 基线，不能 reset/clean 丢弃它们。
4. 在 Ubuntu 24.04 amd64，或 WSL2 的 Linux 文件系统运行。Windows 原生目录 `/mnt/c/...` 不作为 canonical 开发／验收环境。迁移工作区时先确认源与目标及未提交文件，不能用复制覆盖抹掉新改动。
5. 检查固定 Go/Node/pnpm/Python、Docker、真数据库／Git／Blob／Redis、浏览器和必要 worker；依据已保存 runbook 启动。诊断与基本检查通过后进入业务工作，不重建另一套技术栈。
6. 核对已有模型端点、凭据与额度可用。凭据不写入仓库、任务包、日志或 RESULT；不得打印完整环境变量。新的账号、付款、生产凭据或权限授权才是需要人工的边界。
7. 对本次未提交文档建立可审查的基线，再注册正式 V2 任务；不要把旧 V1 的 150 个 merged 当作 V2 已完成。

## 3. Kimi 接入预检

目标模型为用户指定的 `kimi-k3.1-pro-highspeed`。官方模型列表可确认该名称，但本仓库尚未跑过本轮完整模型交付测试。

- [Moonshot：Claude Code 接入](https://platform.kimi.ai/docs/guide/claude-code-kimi) 说明其 Anthropic-compatible Messages 端点及配置；[模型列表](https://platform.kimi.ai/docs/models) 列出目标模型。
- [Claude Code：程序化调用](https://code.claude.com/docs/en/headless) 描述非交互、结构化输出与会话恢复；[网关说明](https://code.claude.com/docs/en/llm-gateway) 明确 Anthropic 不为经网关使用非 Claude 模型提供支持。不要把兼容接入等同于全部 CLI 特性已有保证。
- 资料核对日期：2026-10-26。CLI 参数以已安装版本和实际帮助为准，不盲抄旧文档参数。源码 `worker_guard.go` 已负责生成 Worker 参数；发现兼容问题时做受测的最小适配，不能删除隔离或 RESULT 校验。

模型名称的启动环境示例（不含凭据）：

```bash
export ANTHROPIC_MODEL=kimi-k3.1-pro-highspeed
export RDDEV_WORKER_MODEL=kimi-k3.1-pro-highspeed
```

端点与认证使用用户已配置且与密钥所属平台匹配的值。当前 Worker 使用 `--setting-sources project`，不会依赖用户级 settings 的 env；必须在受控启动环境显式继承必要模型配置。需要时核对客户端支持的默认模型档位映射，防止压缩／辅助场景切到未授权模型。

本仓库的 `rddev worker spawn` 和 `review spawn` 均支持 `--model`，并读取 `RDDEV_WORKER_MODEL`；`drive` 没有 `--model` 参数。Reviewer、返工与新 Worker 均须核对实际模型，不能只设置主会话。

WEB-28 必须证明：

1. Read/Edit/Bash 工具调用及返回完整，长命令可等待，普通拒绝有可处理结果。
2. 原生结构化输出能满足真实 RESULT schema；伪造或缺字段的 completed 会被 collect 拒绝。
3. Worker 无 Git 控制凭据，HEAD/ref/scope 隔离有效；开发权限不因第三方模型而放宽。
4. 独立 Reviewer 只读、能交付合规 verdict，审查对象 fingerprint 正确。
5. 受控样例经过 G1/G2/G3/G4，以及 Commit/Push/PR/CI/Merge；需要 GitHub 现有权限才能完成该样例。
6. 人为制造一个普通失败，Supervisor 能调查、返工并继续；终止／恢复受控会话后可从磁盘接管。

hello、模型名称显示、token 输出速度都不能替代以上测试。当前没有通过这些测试的证据，后续 Agent 必须真实运行。

## 4. 先把工作包转入正式 DAG

`tasks/v2/backlog.json` 是经确认的交接工作包；它不是 rddev 可直接消费的第二份任务状态。

1. 先核对是否有在飞任务及规格指纹安全窗口；必要时等待／收集旧任务，不在活跃 Worker 中途改其契约。
2. 为 WEB-00–30 分配未占用的正式 T 编号，并按需要拆成较小切片；保存 WEB→T→Issue→PR→验收场景映射。
3. 每个任务必须有实际存在或明确要交付的精确测试命令、验收项、依赖、狭窄 allowed_scope、相关规范、decision_level、baseline 和由 Supervisor 分配的迁移号。
4. 规划中的 scope hints 只能用于调查，不能复制成 Worker 全仓库权限。未批准的 docs/specs/tasks/CI 写入不交给业务 Worker；生成物按既有派生规则覆盖并重新生成。
5. 先固化 V2 明确改变的契约：网页科研编辑、草稿、上传、公开默认、自审、模板、数据分析与部署。旧规范中仅针对 V1 的不交付声明保留历史，不能用来拒绝已批准 V2。
6. 当前 TaskSpec 使用 `v1_required`。不得把 V2 简单计入 V1 历史分母；创建受测的 V2 必需范围与完成投影，使所有 WEB 必需工作及其子任务被最终 Gate 覆盖。不能依赖未被执行器消费的字段声称完成。
7. 同步 `tasks/tasks.json`、task_status、tests、progress、需求追踪、必要 ADR、OpenAPI 与生成物；执行既有验证器后开始派工。
8. WEB-01 交付可红可绿的测试框架与初始可运行案例；未来场景按功能切片逐一接入 blocking CI。它不要求尚未实现的整套 U01–U14 在框架首笔就通过，也不允许用 skip 来伪造全量覆盖。全部场景在 WEB-25 前必须真实接入并全通过。

## 5. 每个实现切片的 GitHub 流程

必须执行并记录下面各项，由 Supervisor 操作，Worker 不持有 Git 控制权限：

1. **Issue**：查询已有相关议题，复用或建立范围明确的任务议题，写目标、验收、依赖和关联 T/WEB 编号；旧议题关闭遵循仓库政策，避免重复或擅自关闭。
2. **新分支／Worktree**：由现有 rddev 生成受控任务分支与独立 worktree，登记 baseline 和 refs。当前生成器使用 `task/T...` 命名，沿用其校验规则；手工 integration 分支按仓库约定，不能混用导致 gate 找错 ref。
3. **Worker**：交付限定 diff、真实测试输出与 RESULT，退出后 collect 校验。
4. **独立验收**：Supervisor 检查契约、diff、scope、证据与真实 UI；Reviewer 按规则独立审查。产品自审规则不替代研发独立审查。
5. **Commit**：通过相应 Gate 后提交可审查的小切片，消息关联任务；不把无关本地文件或凭据混入。
6. **Push**：推送任务分支并核对远端 SHA；Supervisor 做过 rebase／冲突合成时尤其要核对，不能让 PR 合并旧的远端树。
7. **PR**：创建／更新 PR，说明具体用户行为、改变、风险与验证，关联 Issue 和任务；多行说明用文件或结构化参数传递。
8. **CI**：等待真正的 required checks，不把 pending、缺失、未触发或旧 revision 的成功当作当前通过。
9. **Merge**：确认最新 head 的 G4、CI、Review、无未解决意见后合并；合并方式遵循仓库策略，不直接 push main 绕过 PR。
10. **同步**：更新本地主线、任务／测试账本、Issue 进度、证据和映射，然后派下一项。

可用的命令形态（TASK 为注册后的正式编号，参数须先核对当前帮助）：

```text
rddev task ready TASK
rddev worker spawn TASK --model kimi-k3.1-pro-highspeed
rddev worker collect TASK
rddev review spawn TASK --model kimi-k3.1-pro-highspeed
rddev review collect TASK
rddev gate run G2 TASK
rddev task accept TASK
rddev git commit TASK
rddev git push TASK
rddev pr open TASK
rddev pr merge TASK
```

这不是可不检查状态就整段执行的 shell 脚本。Supervisor 根据真实状态选择下一条命令，检查返回值、日志与 Gate；Reviewer 尚未退出时不能立即 collect。

## 6. 持续判断、恢复与防空转

每轮执行：读取正式账本／进程 → 收集已退出 Worker → 审查验收 → 处理待修复事项 → 推进 Git/PR/CI → 同步主线 → 派可执行且无冲突的任务 → 保存下一步。

必须把当前任务、最后成功动作、等待条件、重试理由、证据路径与下一步写到受控磁盘状态。`drive` 不运行时，Supervisor 不得误把 `rddev status` 中“driver dead”当作整个开发失败；同时要检查实际 Supervisor 运行与任务是否推进。需要合适的 Linux 会话／进程托管和重启机制；具体实现先做最小恢复验证，不以重写全套编排器为前置。

- 首次明确失败可原 Worker rework；再次同向失败或语义误解换新 Worker；连续三次同类失败拆任务／做诊断，保留证据，不无限重复相同调用。
- collect 拒绝、Review request_changes、CI 红、冲突、上下文或进程退出是工程判断点；既定范围内由 Supervisor 修复，不逐笔等人批准。
- 修改编排器后按现有规则重建 `make rddev`，核对 stale-binary，恢复在飞状态；不能设置 stale bypass 把旧二进制当新规则使用。
- 队列为空时同时检查未注册的 WEB、blocked/worker_failed/rejected/verification/accepted、未合并 PR、缺失的测试与未完成 U 场景；只有“无 ready task”不能宣告完成。
- Gate/数据库/迁移/共享核心包和主线合成按冲突面串行；只有互不阻塞的任务并行，不能给每个任务无条件开 3 份副本。
- 消费、存储、配额和日志需要可观测；采用已有授权额度，不自行充值、购买新服务或切换模型。第三方计费不一定能由 CLI 客户端估价精确代表，应记录实际可取得的用量。

## 7. 什么时候需要人工

已在 docs/70/71 确定的产品选择直接实施，不重复访谈。普通实现、库选择、命名、拆任务、测试、修复由 Supervisor 处理；L2 依规则写 ADR。

只有新的产品／安全／权限／隐私／科研语义，改变核心架构原则，新的外部凭据／付费／授权，或现有规范无法解决的 SPEC_BLOCKED，才停止受影响路径、写证据和最短待决事项。其他独立任务继续。不能为了无人值守放宽安全或删除测试。

软件验收可以使用明确 synthetic 的真实服务／浏览器故事；真实科研结论的接受与公开仍由平台里的有权人决定。工程 Agent 不代替真实研究者作科研承诺。

## 8. 最终交接条件

- 正式 V2 必需任务与 WEB-00–30 全映射、全部合并，Issue/Commit/PR/CI 可追溯。
- U01–U14 按正常网页操作全部通过，没有 API／SQL／Git CLI 代做产品业务步骤。
- 真服务、权限负向、数值分析正确性、模板历史、部署升级与空环境恢复通过；交付构建／迁移可复现。
- 部署、团队操作、备份恢复、运维和已知限制文档齐全；不存在把 ComingSoon／硬编码数据／无解释 disabled 当作已交付功能。
- 最终报告区分“工程可部署试用”与“真实团队尚未执行的使用反馈”；明确实际运行过的命令和证据，不抄历史成功。

仅当这些条件全部成立才停止并报告工程交付完成。无人值守运行遇到必须人工解决的外部阻塞时，应记录等待状态，不伪造有保障的完工日期。
