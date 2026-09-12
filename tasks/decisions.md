# 实现期决策记录

仅记录无需 ADR 的小型工程决策。格式：日期 / Task / 决策 / 原因 / 可逆性。

---

## L3-20260912-1 — Repository visibility（已由 owner 裁定）

- **决策**：`lichman0405/post` 保持 **PRIVATE**，这是 owner 的有意选择。
- **背景**：`PROJECT_REPOSITORY.md` 与 `specs/orchestrator/source-repository.yaml` 记录的是
  *package generation 当时* 的 visibility（`public`）。Supervisor 在首次 push 前重新查询，
  实测为 `PRIVATE`，按 `docs/69_GITHUB_SOURCE_REPOSITORY.md` §5 与 `BOOTSTRAP_PROMPT.md` 第 2 条
  进入 `SPEC_BLOCKED` 并向 owner 请求确认。
- **owner 裁定（2026-09-12）**：PRIVATE 是有意的。
- **后果**：解除 `SPEC_BLOCKED`，Supervisor 可正常执行 push/PR/merge。仓库内容（规格、源码、
  ADR、fixture）按**非公开**信息处理；即使将来改为 public，也不得依赖"当前是公开的"这一假设。
- **可逆性**：完全可逆（外部治理状态，由 owner 控制）。若将来 visibility 再变，必须重新确认。

## L3-20260912-4 — 自主推进授权（owner 裁定，**取代** L3-20260912-2）★ 当前生效

- **决策**：owner 撤销"逐 PR 人工批准"的要求，改为**默认自主推进**。
  对 L0/L1 级别的常规实现任务与产品代码 PR，只要同时满足以下六项，Supervisor 可自行
  review、commit、创建 PR 并 merge，无需逐个人工批准：
  1. Worker 已 `completed`（或等效交付）且 Supervisor 完成独立 G2；
  2. 该任务要求的 G1/G2/G3/G4 全部通过；
  3. CI 全绿；
  4. 无未解决 review 意见；
  5. diff 未超出 `allowed_scope`；
  6. 未改动既定产品语义、安全边界、权限模型、科研语义或核心架构原则。
- **必须停止并请求人工决定的情形（穷举）**：
  1. L3：产品、科研语义、安全、权限、隐私、法律或公开性决策；
  2. 重大 L2：会改变既定核心架构原则（与已接受 ADR 冲突且不可逆）；
  3. 需要新的外部凭证、付费服务或账号授权；
  4. 无法通过现有规格合理解决的 `SPEC_BLOCKED`。
- **不再等待批准的类别**：普通实现、bug fix、测试、重构、依赖范围内的技术选型、
  L1 基线校准、格式/命名/小型无语义重构。
- **明确边界**：授权不降低 Gate 标准；仅为 `lichman0405/post` 的 `main` 有效；
  每次自主 merge 仍须留下完整可追溯链；任一条件不满足即回到"停止并请求人工决定"；
  owner 可随时部分或全部撤回。
- **持久化位置**：`CLAUDE.md` §5.1、`docs/62_SUPERVISOR_GOVERNANCE.md` §3.1、
  `docs/69_GITHUB_SOURCE_REPOSITORY.md` §3.1、本文件。
- **背景**：原分级策略（L3-20260912-2）要求产品代码 PR 逐个人工 approve。owner 于 PR #3
  批准合并时同步改为上述自主授权，以支持长时间无人值守推进 131 个任务。
- **可逆性**：可逆，owner 可随时收紧。

## L3-20260912-2 — Merge 授权分级（owner 裁定）— **已被 L3-20260912-4 取代**

- **决策**：owner 采用**分级授权**。
  - **状态类 PR 自主 merge**：`chore(supervisor)` / `docs` / `tasks` 状态类改动（编排状态、
    decisions/progress 记录、CI/脚手架 bookkeeping），在 required CI 全绿且 Supervisor 完成
    G2 验收后，由 Supervisor 自主 squash-merge。
  - **产品代码需人工 review**：涉及产品实现、OpenAPI/schema 契约、migration、权限/安全语义的
    PR，Supervisor 开好 PR 后必须停下等 owner approve，得到显式批准后才 merge。
  - **边界判定**：以 PR 的实际 diff 内容为准，而非 PR 标题或 task phase。若一个 PR 同时包含
    状态类与产品类改动，按产品类处理（需 review）。
- **背景**：仓库无法启用 branch protection（见 F-20260912-1），因此"无 review 即 merge"必须由
  owner 显式授权。
- **后果**：Supervisor 不再对每个 PR 都停下；但对产品代码 PR 必须停下。
- **可逆性**：完全可逆，owner 可随时改为全人工或全自主。

## L3-20260912-3 — 保留 CodeRabbit（owner 裁定）

- **决策**：保留 CodeRabbit 第三方 AI code review 服务，接受其读取 PR 完整 diff。
- **背景**：仓库为 PRIVATE 且包含完整产品规格；CodeRabbit 已安装，会读取每个 PR 的内容
  （PR #1 已发生）。Supervisor 已向 owner 明示该第三方数据流，owner 选择保留。
- **后果**：CodeRabbit 作为 **advisory** review 信号，不是 required check；Supervisor 不等待
  其完成即按上述授权策略处理 PR。
- **可逆性**：可逆（owner 可在 GitHub 端卸载该 App）。

## L1-20260912-1 — Canonical merge strategy

- **决策**：合并进 `main` 的唯一 canonical 策略是 **squash merge**。
- **约定**：branch `task/T####-<slug>`；commit `T####: <imperative summary>`；
  PR title `[T####] <task title>`（依 `specs/orchestrator/source-repository.yaml`）。
- **原因**：`docs/69` §6 要求项目只选一种 canonical merge 策略。squash 使 `main` 保持线性、
  每个 task 恰好一个 commit，`Requirement → Task → Worker → RESULT → Verification → Commit → PR → Merge`
  的追溯链最清晰；task branch 上的 Worker 迭代过程不需要进入 `main` 历史。
- **可逆性**：可逆但成本较高（改写历史）。默认不再更改。

## L1-20260912-2 — Supervisor bookkeeping 的提交路径

- **决策**：Supervisor 独占写入的编排状态文件（`tasks/task_status.json`、`tasks/progress.md`、
  `tasks/decisions.md`、`tasks/tests.json`、`tasks/results/**`）由 Supervisor 直接提交到 `main`；
  任何**任务交付物**（产品代码、脚本、文档、CI 配置等）必须走
  task branch → PR → merge。
- **原因**：这些文件只有 Supervisor 会写，且随每次 Worker 状态变化而更新；把它们放进短生命周期
  task branch 会产生持续 merge churn，但并不构成任务交付物。`docs/69` §6 禁止的是 **Worker** 直接
  修改 `main`，不禁止 Supervisor 维护自己的编排状态。
- **可逆性**：可逆（后续可改为独立 `orchestration` 分支或自动提交）。

## L1-20260912-3 — Bootstrap 阶段的 Worker 调度方式

- **决策**：在 `rddev` 的 `worker spawn/collect`（T0009–T0012）落地之前，Supervisor 通过
  `.rddev/dispatch/spawn.sh` 手工调度独立 Claude Code Worker（`claude -p`，独立 worktree）。
  该脚本是 `rddev` 的临时先导实现，T0010 完成后应被替换并删除。
- **隔离三层**（与 `docs/61` §6 / `docs/69` §4 对应）：
  1. spawn 时剥离 `GH_TOKEN`/`GITHUB_TOKEN`/`GITHUB_PERSONAL_ACCESS_TOKEN`/`SSH_AUTH_SOCK`/AWS/Docker 等凭据变量，
     并把 `GH_CONFIG_DIR` 指向空目录；
  2. Claude Code `dontAsk` + permission deny 规则 + `worker-guard.sh` PreToolUse hook；
  3. collect 阶段校验 `HEAD == baseline_sha`、refs 未变化、diff 落在 `allowed_scope` 内。
- **证据**：`SMOKE` isolation smoke test（见 `tasks/results/T0011/isolation-smoke/`）12/12 通过——
  commit/push/checkout/tag/remote/worktree/gh/docker/sudo 全部被阻断，HEAD 保持 `fecf24d`。
- **已知缺口**：Claude Code 无参数可关闭 worktree 内 `CLAUDE.md` 自动发现，Worker 目前靠
  system prompt 显式声明优先级来忽略 Supervisor 的 `CLAUDE.md`。T0010 需给出结构性方案。

### 隔离层加固（2026-09-12，T0000 运行期间发现并修复）

观察第一个真实 Worker 的行为后，发现并修复了三个真实缺口：

1. **`Read` 工具绕过**（真实漏洞，已修复）。原本 `worker-guard.sh` 只挂 `Bash` matcher，
   Worker 可以直接用 `Read`/`Grep`/`Glob` 读取 `~/.config/gh/hosts.yml`、`~/.ssh/*`、
   `~/.claude/*` 等凭据存储 —— 即绕过整个 Credential 剥离层。修复：hook matcher 扩展为
   `Bash|Read|Grep|Glob|NotebookRead`，并在 `permissions.deny` 中补 `Read(...)` 规则。
   已验证 13 个用例（含"读自己的 worktree 允许 / 读别人的 worktree 阻断"）。
2. **命令 allow-list 过窄**。原先逐条枚举 `Bash(cmd:*)`，导致 `for`/`&&`/管道等复合命令
   被整体拒绝，Worker 大量时间浪费在试错。修复：`allow` 改为整体允许 `Bash`，把细粒度判定
   完全交给 hook（deny 规则仍保留为第二层）。这与 `specs/orchestrator/worker-permissions.yaml`
   的 `explicitly_denied_shell_intents` 设计一致。
3. **`docker` 规则过粗**。原规则阻断任何含 `docker` 的命令，连 `docker --version` 都拒绝，
   与 T0000（环境 preflight 必须检查 docker/compose 版本）直接冲突。修复：只允许版本查询
   （`docker --version`、`docker compose version`），其余仍需 socket 的操作继续阻断。
   同时新增**写入收敛**规则：shell 重定向/`tee` 的目标路径必须在自己的 worktree 或 `/tmp`
   之内，防止 Worker 通过 shell 写到仓库之外（Claude Code 原生文件工具本身已收敛，shell 没有）。

加固后 hook 回归测试 25/25 通过（`shellcheck -w` 无告警）。T0011 应把上述用例固化为
自动化 e2e 测试，而不是依赖人工回归。

- **仍在的缺口（T0011 必须解决）**：hook 对 `cp`/`mv`/`ln`/`install` 的绝对路径目标尚未收敛；
  Worker 仍可读自己 worktree 之外的任意非敏感路径（如其它项目目录）。

## L1-20260912-4 — `.rddev/` 为纯 runtime 状态

- **决策**：`.rddev/` 整体加入 `.gitignore`（原先只忽略 `runtime/`、`logs/`）。worker worktree、
  日志、registry、dispatch 产物都不进入主仓库历史。
- **持久记录位置**：`tasks/results/<TASK_ID>/RESULT.json` + `tasks/task_status.json` 中的
  `worker_run_id`/时间戳。
- **原因**：`docs/65` 明确 `.rddev/` 是 runtime state 目录；`docs/64` §6 要求 worktree 与主仓库同级
  SSD 但不要求入库。避免 worktree 污染 `git status` 与误提交。
- **可逆性**：可逆。

## L1-20260912-5 — Worker 运行时模型与并发

- **决策**：Worker 继承 Supervisor 环境的 Claude Code CLI 配置（模型 `deepseek-v4-pro`，
  effort `max`，经 `ANTHROPIC_BASE_URL` 指向的 Anthropic-compatible gateway）。每个 Worker 的
  `claude --version`、模型、budget、退出码记录在 `.rddev/workers/<TASK_ID>/registry.json`。
  默认并行 Worker = 3，硬上限 4（`specs/orchestrator/rddev-cli.yaml`）。
- **原因**：当前环境未配置其它 provider；与 Supervisor 保持一致可减少结果差异。`docs/64` §7 要求
  记录而非强制特定模型。
- **可逆性**：可逆，后续可为 Worker 单独指定模型。

## L1-20260912-6 — T0000 预检基线确认（Worker 提出，Supervisor 裁定）

T0000 Worker 没有静默选择，而是把三处基线校准作为待确认项写进 RESULT。Supervisor 裁定：

1. **Docker Compose 基线 = "v2 插件 lineage，major ≥ 2，版本记录"**。`docs/64` §4 的 "Compose v2"
   指 Go 版 compose plugin 这条产品线（相对于已废弃的 python `docker-compose` v1），不是字面
   `major == 2`。本机实测 `Docker Compose version v5.5.1`，按 major ≥ 2 接受并记录实际版本；
   v1 仍判为不合法。可逆：若将来要求精确 pin，改 `docs/64` §4.1 与 `ops/doctor-checks.md` 即可。
2. **shellcheck 不 pin**（presence + 记录版本）。`docs/64` 从未 pin 过 shellcheck，noble 仓库
   提供 0.9.x。
3. **`python3` 解析到 anaconda 3.12.7** 满足 `>= 3.12` 基线。服务侧 canonical Python 仍是
   `uv` 管理的虚拟环境（T0002），因此系统解释器来源属开发机细节，不构成契约变更。

## L1-20260912-7 — Worker 长思考阶段的调度容忍度

- **观察**：T0000 的 Worker 在写第一个文件之前，先用掉约 30 分钟与两个超长 thinking block
  （约 22k + 20k tokens），期间 tool call 长时间停在 24 次不变；随后在约 20 分钟内完成全部交付
  （92 次 tool call、8 个文件、1543 行）。
- **结论**：这是该模型在 `effort=max` 下"先做完整设计再动手"的行为特征，**不是卡死**。
  判断 Worker 是否卡死应看 stream log 是否仍在增长（实测约 70 tok/s），而不是看 tool call 计数。
- **决策**：不因单次长思考就 kill Worker；仅在 log 完全静止时才判定 hang。
  T0010 的 worker 健康检查应按"log 是否增长"实现，而不是按"多久没有 tool call"。
- **代价提示**：该模式对长链路任务吞吐不利。若 P0 后半段吞吐成为瓶颈，可考虑对规格明确的
  实现类任务下调 Worker `--effort`，但需先用数据证明不影响 G2 通过率。

## L1-20260912-8 — T0002 Monorepo 脚手架的前置决策

派发 T0002 前由 Supervisor 先行确定，避免 Worker 在无规格处自行发明：

1. **Go module path = `github.com/lichman0405/post`**。与 canonical source repository 同名，
   但**不得**与产品 runtime 的 Gitea provider 混淆（`docs/69` §7 的身份域分离仍然有效：
   这是源码工程的模块路径，不是产品领域对象）。
2. **pnpm workspace 边界**：只覆盖 `apps/web`、`packages/ui`、`packages/api-contracts` 与生成的
   TS client；不控制 Go/Python（`docs/65` 规则）。
3. **不使用 Turborepo**；跨语言命令统一由根 `Makefile` 承担。
4. **`packageManager` 精确 pin 到宿主 pnpm 版本**，使 T0000 的 `T-PNPM-PIN` 从 `skipped` 转为
   `passed`；这也顺带验证 T0000 的"deferred check"设计确实会在 T0002 后生效。
5. **`packages/schemas` 与 `specs/schemas` 的关系必须可校验**：若复制，必须带 drift check，
   禁止制造第二份会静默分叉的真相源。

以上均为 L1（实现级、可逆），记录于此；若 Worker 认为必须偏离，须在 RESULT 中显式提出。

## L1-20260912-9 — Bootstrap Worker harness 缺陷修复（由 T0001 Worker 发现）

T0001 Worker 在 RESULT 中**报告而非绕过**了两个 harness 缺陷。两者都是我（Supervisor）在
T0000 之后加固隔离时引入的，属实测发现，必须记录：

1. **Worker 无法按契约写 `RESULT.json`**。`docs/63` §6 要求 Worker 把结果写到 worktree 之外的
   `.rddev/workers/<TASK_ID>/RESULT.json`，但我在 `permissions.deny` 里加了
   `Read(//…/.rddev/workers/**)` —— Claude Code 把 Read-deny 同时当作**写入阻断**，于是契约路径
   被自己的加固封死。T0001 Worker 只能写到 worktree 根目录并在 notes 中说明，由 Supervisor 在
   collect 阶段搬回契约位置。
   **修复**：移除该 deny 规则；改为按 Worker 授权自己的 result 目录（
   `POST_WORKER_WORKERS_DIR`），并在 hook 中阻断访问**其它** Worker 的 result 目录。
2. **`Bash(git remote:*)` deny 与 Worker 契约矛盾**。契约（`docs/63` §3 与 task prompt）承诺
   Worker 可用只读 git（含 `git remote -v` / `git remote get-url`），但 settings deny 把
   `git remote` 整体封死，导致 T0001 无法直接自证 remote 归一化。
   **修复**：从 deny 列表移除 `git remote`/`git branch`；改由 hook 精确区分只读与变更子命令
   （`remote -v|get-url` 放行，`remote add|set-url|prune|...` 阻断；`branch -a|-v|--list|...`
   放行，裸分支名与 `-D/-m/--contains <val>` 阻断）。

**第三次修复（我在 collect 阶段自查发现）**：

3. **spawn.sh 的 registry 写入在多行值上崩溃**。`REFS_BEFORE` 含换行，被插值进 Python 字面量后
   产生 `SyntaxError`，`> registry.json` 重定向留下一个**空文件**，任务看似"无 registry"。
   **修复**：改为把值作为 argv 传给 Python（绝不再插值进脚本文本），并对 `refs/remotes/**`
   统一排除。T0001 的 registry 由 Supervisor 依 stream log、git 状态与文件时间戳**重建**，
   并在文件中标记 `reconstructed: true` 与原因。

**经验**：加固隔离的每一次改动都必须回归测试，且**测试必须覆盖契约本身**（"Worker 到底能不能
完成契约要求的动作"），而不只是覆盖"危险动作是否被阻断"。2 和 3 都是"阻断过头"型缺陷。

**待办**：T0011 必须把上述 20 条 guard 用例与两个契约路径用例固化为自动化 e2e。

## L1-20260912-10 — 修复 T0001 preflight 的两个安全缺陷（Supervisor 直接修复）

**触发**：自动 commit security review 对 `scripts/source_repo_preflight.py` 报出
`sensitive-data-to-observability` 与 `fail-open-state-drift`。Supervisor 独立复核后**确认两者均为真
实缺陷**，且 T0001 自带的测试都没有覆盖到。

1. **凭据泄漏（真实）**：remote URL 可能内嵌 token（CI checkout 常见形式
   `https://x-access-token:<PAT>@github.com/owner/name.git`）。
   `normalize_remote_url()` 只在**解析时**剥离 userinfo，但原始 URL 被原样写进
   `REPO-CANONICAL` 的 `measured`/`detail` —— token 会进入 stdout、`--json` 文档与 CI 日志。
   修复：新增 `speclib.redact_url()`（保留 user、密文替换为 `***`），在所有可能输出 origin URL
   的位置调用，**包括 URL 无法解析的失败路径**（最易漏的地方）。
2. **fail-open（真实）**：`BRANCH-DEFAULT` 在 `origin/HEAD` 不可确定时 `gating=False`，而
   `VISIBILITY` 对 unknown 是 fail-closed —— 同一个 gate 内自相矛盾，且与其 docstring 的
   "never silently passes" 承诺不符，可出现"integration branch 从未被验证却 bless push"。
   修复：`BRANCH-DEFAULT` 改为 gating（unknown → `branch_unverifiable` → refused）；
   由于 `origin/HEAD` 在 CI/fresh clone 中通常缺失，operator opt-in 现在同时用
   `gh api ... --jq .default_branch` 取远端权威值，使**真实 operator 路径从"只能失败"变为"能验证"**；
   本地与远端记录矛盾按 state drift 显式失败。

**决策：由 Supervisor 直接修复，而非派 Worker。** 理由：(a) 安全缺陷应尽快落地而非排队等
Worker 往返；(b) 在飞行中的 T0002 的 `allowed_scope` 含 `scripts/**`，并行 Worker 会写冲突。
这是一次对"业务实现交 Worker"原则的**有意识偏离**，已如实记录而非掩饰。修复范围小且带回归
测试（新增 8 个用例），并顺带发现 smoke test 的 schema 一致性检查确实拦住了新增 reason code
未入 enum —— 契约校验按设计工作。

**结果**：PR #8（`862b78b`）。unit/smoke/validate_specs/doctor 全绿，marker 已重生成，
shellcheck 干净，真实 `--check-visibility` 路径现可验证 BRANCH-DEFAULT 并 bless。

**教训**：安全 review 报的"fail-open"和"数据外泄"类问题，即使出自我方已验收的代码，也必须
独立复核后当真——这两个都不是 Worker 的疏漏，而是"验收标准本身没覆盖"的缺口。后续 task 的
acceptance criteria 应显式包含"输出中不得出现凭据"与"无法验证时必须 fail-closed"。

## L1-20260912-11 — 自动安全 review 对 JSON Schema 的两条发现：判定为"已在别处受控，缺可追溯提示"

**触发**：自动 commit security review 对 T0002 复制进 `packages/schemas/` 的 schema 报出
`authorization-metadata-client-controlled`（`core-scientific-object.schema.json`）与
`ssrf-validation-differential`（`external_reference.schema.json`）。

**先查了"是不是规格缺口"** —— 结论是**两条都已在规格中受控**，只是控制点不在 JSON Schema 层：

1. **SSRF**：`docs/23` §7 明确要求 "SSRF guard for External Reference fetch、URL allow/deny
   strategy"；`docs/54` 把 "External Reference fetch SSRF 内网" 列为威胁场景 #5，并要求
   **每个场景必须有对应 automated negative test**。控制点在 **fetch 层**，不在数据形状层。
2. **客户端可控的授权元数据**：`docs/23` §3 规定授权 "默认 deny。每个 command 明确 actor +
   org + project + object + action + policy context"；`docs/12` §3 "不扩大可见性原则" 规定任何
   private→public 必须由有权限的人显式确认并产生 audit/event。即 `created_by` /
   `visibility_policy_id` 本就**不是客户端可决定的**，控制点在**应用层**。

**判定**：这不是规格语义缺失，而是"schema 没告诉实现者控制点在哪"。而 schema 作为**数据形状**
描述，本来就不该、也不能表达 fetch 层与授权层的控制——若把 scheme allowlist 之类的**策略**写进
schema，那才是真正的 L3 安全决策，Supervisor 不得自行决定。

**采取的动作（仅注解，非策略）**：为全部 16 个 schema 补充 `description`：
- 顶层说明该 schema 是 **entity-shape、不是 request DTO**，不得把客户端 body 直接绑定到它；
  列出 server-authoritative 字段；
- `created_by`（14 个）与 `visibility_policy_id`（12 个）注明 server-authoritative 及依据；
- `external_reference.canonical_url` 注明解引用时必须走 `docs/23` §7 的 SSRF guard，
  schema 本身**永不足以**作为校验，且抓取内容按 `docs/23` §8 视为不可信数据。

**证据（这是本次判断的关键）**：把新旧 schema 的所有 `description` 键剥离后逐文件比对，
**非 description 内容零差异** —— 即纯注解改动，未触碰任何 validation 关键字、`required`、
`enum`、`additionalProperties`。因此**没有做出任何安全策略决策**。

**未做（留给 owner / 后续）**：
- 若要在 schema 层**强制** URL scheme allowlist 或字段不可变性，那属于 L3 安全策略，需 owner 决定。
- `docs/54` 要求威胁场景 #5 有 automated negative test 并映射到 traceability 文档。该负向测试
  属于**实现 External Reference fetch 的那个任务**（P5）的验收项，必须在其 task package 的
  acceptance criteria 中显式写入，否则会重演"标准没写清楚"的老问题。

## L1-20260912-12 — T0003 init 脚本的三个安全缺陷（Supervisor 直接修复）

**触发**：自动 commit security review 报出 `credential-exposure`、`sensitive-to-observability`
（`infra/docker/gitea/init-gitea.sh`）与 `sql-injection`（`infra/docker/postgres/initdb.d/01-init.sh`）。
**复核确认三条全部为真**，且前两条在我的 G3 输出里**明文可见**——我读过那段输出却没有质疑。

1. **Gitea token 被打印到 stdout**：`gitea admin user create --access-token` 会把 token 明文
   输出，于是 `make infra-init` 会把一个真实凭据写进终端 scrollback、`tee` 捕获与 CI 日志。
   违反 `docs/23` §10（日志 redact）。**修复**：init 不再自动签发 token；改为显式 opt-in
   （`GITEA_SVC_MINT_TOKEN=1`），并由 README 指引直接重定向进 secret store；签发时显式指定
   窄 `--scopes`，不再用 Gitea 默认的 `all`（`docs/23` §8 服务 token 最小权限）。
2. **SQL 注入**：Gitea 角色名/密码/库名被从环境变量直接拼进 SQL 文本，被覆盖的值可越出字符串
   字面量。**修复**：改用 psql 变量 + `:"ident"` / `:'literal'`，由 psql 负责转义。
3. **（修复过程中发现，review 未报）环境变量覆盖从未生效**：`docker compose exec` 不转发宿主
   env，因此所有文档化的覆盖变量（`GITEA_ADMIN_USER`、`MINIO_INIT_BUCKET`、
   `GITEA_SVC_MINT_TOKEN` …）一直被**静默忽略**并退回默认值，而 README 与脚本都声称可覆盖。
   **修复**：`init.sh` 显式用 `-e` 转发白名单。以 `MINIO_INIT_BUCKET` 建桶验证——只有转发到位
   才会成功。
   
   第 3 条最值得警惕：它是**正确性故障却不报错**，安静地成功，产出的是默认值。

**决策**：仍由 Supervisor 直接修复（安全缺陷 + 与在跑的 T0004/T0005 无写冲突），并在 PR 中
如实标注为 Supervisor 作者身份，不伪装成 Worker 交付。

**验证**：全新 volume → 五个服务全 healthy；`make infra-init` exit 0 且输出中 40-hex 形态 token
**零个**；显式签发路径仍可用；重复 init 全部 no-op；`down -v` 无残留；shellcheck 干净。
PR #14（`29625a5`）。

**流程教训（比缺陷本身更重要）**：这是同一模式第三次出现——**验收标准没覆盖**该检查项。
我在 G2 里核对了 acceptance criteria，却没有核对"这个输出里是否含凭据"。自本条起，凡涉及
配置/密钥/初始化脚本的任务，task package 的 acceptance criteria **必须**显式包含：
"输出/日志/快照中不得出现凭据（以 canary 值扫描验证）"与"无法验证时必须 fail-closed"。

## L1-20260912-13 — guard `rm` 规则第三次过度拦截（Worker 报告，已修）

T0004 Worker 报告：`rm -f <自己 worktree 内的绝对路径>` 被"recursive delete outside the task
worktree"拦截，而普通 `rm <file>` 可以。复核确认是**同一类过度拦截缺陷的第三次出现**（前两次见
L1-20260912-9）：规则对任何绝对路径都拦截，既没有判断该路径是否就在 Worker 自己的 worktree 内，
又把相对路径里的 `/` 误当成绝对路径（`rm -rf internal/config/tmp` 也会被拦），并且完全漏掉
`~` / `$HOME` / `${HOME}`。

**修复**：改为解析真实目标——只有位于自身 worktree / 自身 result 目录 / `/tmp` 之外的目标才拦，
且只认"路径位置"上的绝对路径（行首或空白之后）；`~`/`$HOME` 一律视为 worktree 之外。
回归 17/17 通过（合法清理放行；`/etc`、`/home`、他人工件、`~`、`$HOME` 全部拦截）。

**规律**：隔离层三次修正**全部是"拦得太宽"而非"拦得太松"**。因此本项目的规则是：**任何对
guard 的改动都必须附带回归套件**，且套件必须同时覆盖"合法操作放行"与"危险操作拦截"两侧，
不能只测拦截。

## L1-20260912-14 — T0004 的跨任务遗留项，强制纳入 T0006 验收

T0004 的 Worker 正确地**没有越界**，而是把两处跨任务问题写成 follow-up。Supervisor 判定这两项
**不是可选改进，而是必须修复的缺口**，因此把它们写成 T0006 的**必需验收项**，而不是让它沉在
follow-up 列表里：

1. **`apps/web/app/page.tsx` 未接入 `lib/config.ts`**：T0004 交付了经过验证的
   `loadWebConfig()`，但运行时的 web app 仍使用静默的 `?? "http://127.0.0.1:8080"` 回退。
   即**"fail-closed" 在运行时并未真正生效**——校验模块存在且被测试覆盖，但没有任何东西调用它。
   T0006 必须完成接线并删除静默回退。
2. **端口冲突**：scientific adapter 默认端口 9000 与 T0003 compose 中 MinIO S3 API 的 9000 冲突。
   涉及 `apps/web` 默认值、adapter 默认值与 compose 映射，需跨任务统一。T0006 必须解决。

**理由**：README/规格声称的行为与实际运行时行为不一致，属于"看起来通过、实际没有生效"的失效模式——
与本轮安全 review 反复暴露的模式同类（`L1-20260912-12` 第 3 条）。宁可把它变成阻塞性验收项。

---

## 环境发现（非决策，必须显式记录）

### F-20260912-1 — GitHub branch protection 在当前 plan 下不可用

`gh api repos/lichman0405/post/branches/main/protection` 返回
`403 Upgrade to GitHub Pro or make this repository public to enable this feature.`

因此 `docs/69` §6 的目标规则（required CI、禁 force push、required review）**无法由 GitHub 强制执行**。
按 `docs/69` §6 "GitHub plan/API 权限受限时不得伪造成功"，此处如实记录，不宣称已启用保护。
当前补偿措施：只有 Supervisor 持有 write credential；`main` 的写入通过 Supervisor 纪律 +
PR 流程保证。若 owner 升级 plan 或调整 visibility，应重新尝试启用 branch protection。

### F-20260912-2 — Supervisor 环境中存在 ambient GitHub write credential

Supervisor 环境变量中存在 `GITHUB_PERSONAL_ACCESS_TOKEN`。Worker spawn 时已被显式剥离
（见 L1-20260912-3）。任何绕过 `spawn.sh` 直接启动 Worker 的做法都会破坏这条隔离，禁止使用。
