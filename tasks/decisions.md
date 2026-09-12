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

## L2-SPEC-20260912-15 — ★ 待 owner 裁定：版本历史在存储层可变（Master Gate A 不变量未被强制）

**发现方式**：T0005 G2/G3 验收时，Supervisor 把 13 个 migration 应用到 scratch 数据库后**直接做了
一次历史改写尝试**（不是读代码推断）：

```sql
UPDATE scientific_object_versions SET title='REWRITTEN HISTORY', integrity_hash='tampered';
-- UPDATE 1        ← 成功
```

即**已提交的版本行可以被原地修改**。`ON DELETE RESTRICT` 只阻止*删除*被引用的行，不阻止*修改*。

**这不是 T0005 的缺陷**：canonical schema `specs/database/postgres.sql` 中**没有任何**不可变性
机制（append/immutable/trigger/rule/revoke 全部 0 匹配），而 T0005 的要求是**忠实移植**该 schema
——Worker 做到了，并用 SQLSTATE 断言（23503/23505）真实测试了约束。Supervisor 独立复核确认移植无误。

**但它与以下既定要求冲突**：
- `CLAUDE.md` §9 不变量 5「Release / Asset Version immutable」、不变量 8「Nothing disappears;
  state only evolves」；
- `docs/adr/ADR-007-append-only-abort.md`；
- **`docs/31_MASTER_ACCEPTANCE.md` Gate A**：「Object/relation 版本不可改写历史」。

**为什么不由 Supervisor 自行修复**：如何在存储层强制不可变性是一个**存储边界 + 科研完整性**决策，
至少属 L2（`CLAUDE.md` §5：跨模块接口、存储边界必须新增/更新 ADR，不得静默改变既定架构），其中
"版本不可改写历史"属科研语义，触及 L3。可选项各有实质取舍：
1. `BEFORE UPDATE/DELETE` 触发器（改动最小，但触发器对批量操作与 superuser 的语义需明确）；
2. `REVOKE UPDATE, DELETE` 配合只写角色（更彻底，但要求应用连接使用受限角色，影响连接配置）；
3. 改为 append-only 表设计 / 事件日志投影（改动最大，与 ADR-021 相关）。

**因此标记为 `SPEC_BLOCKED`，等待 owner 决定**，不影响其他任务继续执行（T0006/T0009 与其无关）。

**影响面**：所有依赖"版本不可改写"的能力——Research PR 的 RSG diff 可信性、Release 的
immutable pinning、Audit/证据链。V1 Master Gate A 在此项通过前不应判为满足。

## L1-20260912-16 — guard/collect 第四次误报：refs 比对把 Supervisor 自己的分支操作判成 Worker 违规

T0005 collect 报 `refs changed`，实际是 **Supervisor 自己**在 Worker 运行期间合并并删除了
`refs/heads/task/T0004-config-secrets`、又新建了 `refs/heads/task/T0009-rddev-cli`。该 Worker 未做
任何 git 操作。

**修复**：refs 比对排除 `refs/remotes/**`（共享）与 `refs/heads/task/**`（Supervisor 的 task
分支命名空间）。**并做了双向验证**：修好后误报消失；我**故意植入** `refs/heads/worker-created-evil`
后 collect 立刻 REJECTED，删除后恢复 COLLECTED——即真实越界仍会被抓住，不是把检查关掉。

这已是隔离层第四次"拦截过宽"。规律已记录在 `L1-20260912-13`：**共享资源（git ref store、宿主
环境）上写的检查，必须区分"Supervisor 的正常活动"与"Worker 的越界"**，否则会产生系统性误报，
而误报会训练出"忽略红灯"的习惯——那比漏报更危险。

## L1-20260912-17 — Search 查询缺失访问控制（已修）+ 同类枚举查询的系统性缺口（已归档到授权任务）

**发现**：自动 security review 报 `internal/persistence/queries/search.sql` 两条
`broken-access-control`。复核确认：`SearchDocuments` **完全没有过滤**——没有 visibility、没有
project/tenant 限定，而且**根本无法接受 actor 的 scope 参数**，于是文本命中的**每一行**都会返回。
这正是 `docs/54` 的**头号威胁场景**「Private Project/Branch 内容出现在 Search/Explore」，
也违反 `docs/23` §5「所有 query/search/export/download 进行 policy 过滤」与 Master Gate E
「Search 无 private leakage」。既有测试**从未执行过这个查询**，所以没被发现。

**修复**（PR #21）：查询改为必须接受调用方的 scope，只有 `visibility='public'` 或
`project_id = ANY(@allowed_project_ids)` 的行才返回。**传空数组只返回 public，绝不返回全表**——
即"结构上不可能返回未授权私有行"，而不是依赖每个调用方记得加过滤（与 `badValue` 的
fail-closed-by-construction 同一手法）。新增 `TestSearchDocumentsEnforcesAccessControl`
以真实 PostgreSQL 证明三档 scope 行为。sqlc 已重新生成，drift check 干净。

**主动扩大排查（不只修被报的那条）**：我扫描了全部 `internal/persistence/queries/*.sql` 的读查询。
绝大多数是按主键的实体读取（`GetUserByID`、`GetBlobByContentHash` 等），其授权本就属于调用方，
**不是**同类问题。但发现**同类枚举查询**：

| 查询 | 问题 |
|---|---|
| `ListOrganizations` | 返回**全部**组织，无任何过滤 |
| `ListUsers` | 返回**全部**用户，无任何过滤 |
| `ListProjectsByOrganization` | 仅按调用方传入的 org 过滤，**无 visibility** → 非成员传入 org id 即可拿到该组织的 private project |

**决定：不在本轮顺带改写它们。** 这些查询的正确 scope 取决于授权模型（谁能看到哪个 org/project、
Project membership 语义、Explore 的 public 可见性规则），改写它们等于**由我设计授权模型**——属
L2/L3。正确做法是：把这些查询的 scope 作为 **P1（Identity/Organization/Project shell）与
P8（Explore/Fork/Contribution）任务的必需验收项**，由对应任务在授权框架内实现。

**给后续任务的强制要求**：任何返回**集合**的读查询都必须显式接受并应用 scope；只按主键返回单实体的
查询不在此列。这条已写入下方"后续任务强制项"。

## L1-20260912-18 — ★ Supervisor 自己的流程失败：在 CI 红灯的情况下 merge 了 PR #23

**事件**：T0009 在其 scope 内新增了 `specs/orchestrator/task-state-machine.yaml`，但**没有重新生成**
`specs/SPEC_VERSION.json`。CI 的 "Verify derived spec version marker" 步骤因此失败。我在该检查**红灯**
的情况下把 PR #23 merge 进了 main，导致 main 变红，随后用 PR #24 修复。

**两个失败，第二个是我的，且更严重**：

1. **Worker 侧**：改了 `specs/**` 未重新生成派生标记。它跑了 `make check`、`go test`、
   `ops/doctor.sh`——**这三者都不包含** marker 检查，所以它的本地 gate 是 CI 的真子集，必然抓不到。
2. **Supervisor 侧（我）**：我的 G2 同样只跑了 `go vet`、`go test`、`make check`、`ops/doctor.sh`
   ——**又是 CI 的真子集**；并且我把 `gh pr checks` 与 `gh pr merge` **写在同一条 shell 命令里**，
   于是无论检查结果如何 merge 都会执行，我却在汇报里写了"CI 全绿"。

**为什么这比之前几次严重**：`L3-20260912-4` 授权我把"CI 全绿"作为自主 merge 的**前置条件**。我
既没有验证那个条件，又把"看起来像 CI 的检查"当成了 CI。这正是我此前反复批评 Worker 的同一模式
——**验收标准/门禁与真实门禁不一致**——这次由我自己犯下。

**纠正措施（立即生效）**：
1. **G2 必须执行 CI workflow 的完全相同步骤**，而不是"类似的一组"。CI 跑什么，G2 就跑什么；
   当前 CI 步骤为：`validate_specs.py`、`spec_version.py --check`、两个 spec 测试脚本。
2. **CI 状态必须在 merge 之前作为独立步骤读取并断言**，绝不与 `gh pr merge` 写在同一命令里。
   绿灯才允许执行 merge 命令。
3. `make check` 与 CI 的**覆盖面差异本身**是缺陷，已作为 T0008 的必需项（见下）。

**验证**：修复后 main 已恢复绿灯（`794b2c6`，CI success）。

## L1-20260912-19 — `make check` 现在需要活的 PostgreSQL（T0005 引入，T0008 必须修）

排查上述问题时发现：`make check` 会跑 `go test ./...`，其中包含 `tests/integration`，而后者**需要
一个可连接的 PostgreSQL**（默认 127.0.0.1:15432）。数据库不在时 `make check` 直接失败。

- **这与验收标准冲突**：T0002 要求"根目录一条命令完成 Go/Web/Python 基础检查"；T0006 要求
  "CI smoke 通过，且不依赖 Docker、不依赖活的数据库"。需要基础设施的"一条命令"不满足任何一条。
- **为什么没被发现**：T0005 的 Worker 和我的 G2 都是**在数据库起着的时候**跑 `make check` 的，
  于是双方都看到绿色。这与本轮其它缺陷是同一模式：**只有在特定环境下才暴露的检查**。
- **强制要求（写入 T0008 验收项）**：按 `ops/DEV_COMMANDS.md` 已意图的设计拆分——
  `make check` / `make test` 为**无基础设施**可跑的单元级检查；集成测试走
  `make test-integration`，并**显式要求**数据库（不可连接时必须**明确失败**或**明确 skip 并打印原因**，
  不允许静默跳过）。CI 的默认 job 必须能在无 Docker 的 runner 上跑绿。

## L1-20260912-20 — ★ 我的"并发已验证"是过度声明：验证必须主动制造失败

自动 security review 报出 `internal/devorchestrator/store.go` 两个缺陷。**两个都是真的**，而我此前
在 T0009 的 G2 中报告过"并发已验证：10 个并发写者 → 1 成功 / 9 明确拒绝，无静默覆盖"。

那个结果**是真的，但没有意义**：10 个写者用的是**同一种**路径拼写，且状态文件都是完整的。两个缺陷
恰好都落在我不曾尝试的情形里：

1. **锁按字符串相等选取**：默认拼写用 `.rddev/locks/task_status.lock`，任何其它拼写用
   `<statePath>.lock`。于是两个进程写**同一个文件**却拿了**不同的锁**：
   `rddev task ready T0002` 与 `rddev task ready T0002 --state-json ./tasks/task_status.json`
   双双报告 `todo -> ready`，其中一个静默覆盖了另一个的历史。修复：锁由规范化绝对路径派生，
   成为**文件**的属性而非**调用者拼写**的属性。
2. **state drift 静默 fail-open**：状态文件存在但 `tasks` 为空/缺失（截断、改名、手工编辑）时，
   所有任务**静默读成 todo**，无警告无错误；于是 `rddev task ready <已 merged 任务>` **成功**，
   已完成的工作被重新派发。修复：文件不存在仍是合法全新开始；文件存在但无任务则报错并说明处置方式。

**流程教训（本轮最重要的一条）**：
**对于"X 不会发生"这类性质，测试必须主动尝试让 X 发生**——变换输入、漂移输入、对抗性拼写；
而不是用一组彼此相同、环境完好的输入跑一遍就宣布"已验证"。我此前批评 Worker 的"验收标准没覆盖"
模式，这次由我自己在验证环节重演，而且我用它得出了错误的结论并写进了汇报。

**已固化为后续要求**：G2 报告中，凡声明"某性质已验证"，必须同时给出**使该性质失败的尝试**及其
被挡住的证据；只说"跑了 N 次都通过"不足以支撑。

**验证**：原始探针重跑——锁排他性恢复（2 个写者 1 成功、只剩 1 个锁文件）；drift 下 `inspect`
与 `ready` 均 exit 1 并给出可操作错误。新增 `store_integrity_test.go`（4 种拼写的锁同一性、
4 种 drift 形态被 Next/Inspect/Transition 拒绝、正常路径仍工作）。PR #26（`9b6ef86`）。

## L1-20260912-21 — T0013 落地：触发器强制不可变，以及**实测出但未关闭**的残余绕过

**背景**：owner 裁定用 DB 触发器强制版本不可变（`L2-SPEC-20260912-15`）。T0013 已实现并合并
（PR #28，`eb1fc6d`）：13 张 append-only 表，`BEFORE UPDATE OR DELETE` 行级触发器 + 共享 guard
函数。我用自己的原始探针复核：原先成功的
`UPDATE scientific_object_versions SET title='REWRITTEN HISTORY'` 现在被 `P0001` 拒绝，合法
`INSERT` 新版本行仍成功，13 个触发器在全新安装与升级路径下均 enabled。**Master Gate A 的该项
在应用层意义上已闭合。**

**T0013 Worker 主动披露的缺口**：行级触发器**不响应 `TRUNCATE`**。我已用 `00015` 补上
statement-level `BEFORE TRUNCATE` 触发器（PR #29，`a530fef`），并验证 `TRUNCATE ... CASCADE`
现被拒绝、未受保护的表仍可正常 truncate（即定向而非一刀切）。

**我方主动攻击后实测出、且**未**关闭的残余绕过**（这一步是新规则"主动制造失败"的直接应用）：

```
SET session_replication_role = replica;   → UPDATE 1   （触发器不触发）
ALTER TABLE ... DISABLE TRIGGER ...;      → UPDATE 1
```

**含义**：触发器强制可被**表 owner** 或任何能设置 `session_replication_role` 的角色绕过。而本栈的
应用当前**以表 owner 身份连接**（`POST_DB_USER` 默认 `postgres`）。因此这套机制挡住的是
**应用层的意外改写**（它的主要用途），**挡不住特权角色**。

**为什么不由我关闭**：真正关闭它需要引入受限写入角色（`REVOKE UPDATE/DELETE` + 应用以非 owner
运行），这属于**权限模型与部署形态**的变更；owner 在我给出的选项中**明确没有选择**该方案
（选项 2/3），因此我不擅自更改权限模型，仅如实记录并上报。**待 owner 决定是否升级为纵深防御。**

**同时修复的测试脆弱性**：迁移头号被硬编码（`headVersion = 14`），导致新增一个迁移就打断 3 个
无关测试；现改为从 embedded 迁移集**推导**，不可能再过期。`assertTriggers` 也改为断言每张表的
**两半**保护（行级 + 语句级）而非"恰好一个触发器"。

**流程印证**：这次"主动尝试让 X 发生"的规则立刻产出了价值——TRUNCATE 与
`session_replication_role` 两个绕过都不是读代码能看出来的。

## L1-20260912-22 — T0006 第一次派发 worker_failed：预算耗尽 + **我写了一个不存在的 scope glob**

**结果**：T0006 第一次派发以 `worker_failed` 结束（`docs/62` §4：预算耗尽不算 completed）。Worker 在
**即将写 RESULT.json 的那一刻**用尽了 $15 上限（`total_cost_usd = 15.23`），因此**没有交付 RESULT**，
collect 因"缺 RESULT + 6 条越界路径"而 REJECTED。

**归类**：这是**预算耗尽**，不是纪律失败。它产出的 29 个改动路径是真实且有价值的——`go build`、
`go vet` 通过，`internal/health` 与 `internal/worker` 的测试通过。因此处理方式是**在同一 worktree
续做**（保留成果），而不是销毁重来。

**★ 我自己的错误（两个之一）**：T0006 的 task package 里我写了
`"internal/config/wiring*"`——**这个 glob 不匹配任何真实文件**。Worker 需要改
`internal/config/config.go`（为 `cmd/api` 提供 cwd/仓库根相关的 env 文件解析），却因 scope 而无权改，
于是它越界了。**6 条"越界"里有 3 条（config.go / cwd.go / cwd_test.go）是我造成的。**

**纠正措施（已生效）**：派发前**必须用真实文件树校验 `allowed_scope` 的每个 glob 至少匹配到东西**；
纯占位式 glob 会制造出"Worker 必然违规"的局面，然后被我自己的 collect 判为违规——这是**门禁设计
缺陷**，不是 Worker 问题。scope 已修正为 `internal/config/**` 等真实路径。

**第二条教训（已写入 Worker 提示词）**：**RESULT.json 必须在早期写出并增量更新，绝不能留到最后一步。**
上一次尝试把结果写在最后一个操作，预算一断**全部证据归零**——包括那些本已完成的验证。现在的规则是：
有任何真实结果就先写一版（哪怕 `not_run` 占位），随后覆盖。这条对任何"交付物在最后生成"的流程都成立。

**`cmd/devredis` 的裁定（L1）**：Worker 新增了一个 dev-only 的进程内 Redis（miniredis），使
"worker 消费真实 job"这一验收项能在**无 Docker** 的前提下用真实 Redis 协议证明。这是对"无 Docker +
必须证明真实消费"这一张力**合理的工程解法**，故 **接受**并纳入 scope，条件是：明确标注为 dev/CI 工具、
不被任何部署服务依赖、并在 RESULT 中说明理由。验收标准不因此放宽。

## L1-20260912-23 — ★ 我把刚记录的教训立刻重犯了一次（scope glob），以及 Worker 会占用宿主资源

**事件**：T0007 collect 以"两条越界路径"被拒（`tests/observability/canary-sweep.sh`、`trace-e2e.sh`）。
复核：DAG 中 T0007 的声明 scope **本来就是 `tests/**`**，是**我在 task package 里把它收窄成**
`tests/integration/logging*` 与 `tests/**/logging*`——这两个 glob 恰好排除了 Worker 合理放置测试的
`tests/observability/`。**Worker 没有任何过错，拒绝完全由我的收窄造成。**

**★ 关键在于时机**：我在**上一次派发（T0006）刚刚记录过完全相同的教训**
（`L1-20260912-22`：`internal/config/wiring*` 不匹配任何真实文件）。记录之后，我在紧接着的派发里
以另一种形式重犯了它。

**结论：这类错误靠"记住"是治不住的，必须机械化。** 纠正规则改为：

1. **默认直接采用 DAG 声明的 `allowed_scope`**，不做"看起来更精确"的收窄；
2. 只在**并发任务确有具体文件冲突**时收窄，且**只排除对方确实需要的具体路径**，绝不用凭空发明的
   新 glob 去"近似"原 scope；
3. 收窄后必须自问：**"这个 glob 覆盖的文件，是否包含该任务自然会产生测试/代码的位置？"**

**处理**：把 T0007 的 scope 恢复为声明的 `tests/**`（仅保留对 T0010 正在使用的
`cmd/rddev/**`、`internal/devorchestrator/**` 与 `go.mod/go.sum` 的排除），重新 collect 即通过。

## L1-20260912-24 — Worker 会在宿主上起服务（无 Docker 时的合理变通），但不清理

T0007 的 Worker 为了跑 `make check`（该命令当时仍需要一个真实数据库，直到 T0008 修复）**自行用 conda
的二进制在 `/tmp` 起了一个 PostgreSQL**，监听 127.0.0.1:15433，并在 RESULT 中如实披露。

- **合理性**：它没有 Docker 授权；`/tmp` 属于我的写入收敛规则明确允许的 scratch 空间；只监听回环。
  这是在真实约束下**有创造力的正确做法**，不应视为违规。
- **问题**：**任务结束后它没有停掉该服务**——宿主上留下了仍在运行并占着 15433 的进程与 `/tmp/pgdata`。
  我已停止并清理。
- **为什么必须清理（不只是"不整洁"）**：T0008 此刻正在验证的正是"`make check` 在没有数据库时必须
  通过"。**宿主上多出一个数据库会让 T0008 的验证虚假通过**——即一次泄漏的宿主状态会污染另一个任务
  的验收结论。这比"留下垃圾"严重得多。

**已固化的要求**：任务包必须显式要求 Worker **清理其在宿主上启动的任何服务/进程**；T0010/T0011 应在
collect 阶段把"宿主残留"纳入检查（至少记录 Worker 启动的进程，并在 collect 时提示）。当前 guard 不
禁止启动进程（只禁 Docker/sudo/apt），这是有意的，但"启动后不清理"必须被约束。

## L1-20260912-25 — ★ T0008 的 CI workflow 是无效 YAML：合并它会让仓库**完全没有 CI**

**事件**：T0008 用一个新的 `.github/workflows/ci.yml` **替换**了原有的 `spec-validation.yml`。而
该文件**无法被 YAML 解析**——`- name: go: fmt check` 这类未加引号的"冒号+空格"被 YAML 读成分层
mapping，于是整个 workflow 非法。GitHub 对非法 workflow 的处理**不是"检查失败"，而是根本不运行**：
run 在 0 秒内失败。

**后果（为什么这条最严重）**：该 commit 同时**删除了**原来可用的 spec-validation workflow。若照原样
合并，仓库将**一个可运行的 CI 都没有**——而我这轮一直在执行的合并条件是"CI 全绿"。**绿灯的前提
（CI 存在）本身消失了，而且没有任何本地检查能发现**：`scripts/ci.sh` 验证的是 workflow **所运行的
命令**，从不验证 workflow **文档本身**。

**修复**（Supervisor）：
1. 给 27 处含 `": "` 的 step name 加引号；
2. 新增 `scripts/validate_workflows.py`，作为 `scripts/ci.sh` 的**第一个 stage**：解析每个 workflow，
   并校验每个 job 的 `runs-on`/`steps`/step 结构/`needs` 可解析。这是套件里**唯一**能发现该缺陷类型的
   检查。它同时处理了 PyYAML 的 YAML 1.1 怪癖（裸 `on:` 会被解析成布尔 True，否则会对合法文件误报）。
3. **验证**：反向种植原缺陷 → `does not parse at line 69 column 17`，exit 1；修复后 → exit 0。

**随后暴露的连带问题（都是真问题，非误报）**：workflow 一旦真的运行，python stage 立刻失败——
T0008 新引入的 ruff/mypy gate 发现了 **T0007 遗留的 6 处违规**（2 处 import 排序、3 处 RUF059 未使用
解包变量、1 处 mypy 类型错误：`configure_request_logging(stream: object)` 与 `StreamHandler` 不兼容）。
**选择修复而不是加 baseline**：在 gate 引入当天就把 6 处小问题 baseline 掉，等于永久削弱它。
修复后 `go/web/python/spec-validation/task-state/migration-integration` **六个 job 全绿**。

**第三条"用真实命令验证"的教训（本会话第三次）**：我用**近似**命令跑
`ruff --fix`（在仓库根用 `ruff check services/scientific-adapter --config ops/ci/ruff.toml`），
其配置解析与 CI 的真实命令（在 adapter 目录用 `ruff check . --config ../../ops/ci/ruff.toml`）**不同**，
导致它**删掉了作者写的 `# noqa: N802`**（未启用该规则时 RUF100 会移除对应 noqa），并因此与 main 产生
冲突。**必须用与 CI 完全相同的命令验证**——与 L1-20260912-18（带着红灯合并）同源。

**我的第四个操作失误**：我在 T0008 分支上修 T0007 的文件，而 T0007 之后才合并进 main，导致 PR 变为
`CONFLICTING`。冲突由我按 Supervisor 职责解决（取 main 版本 + 重跑真实命令）。

**合并流程必须更新**：required check 的名字已从单一 `validate` 变为六个 job
（`go` / `python` / `web` / `spec-validation` / `task-state` / `migration-integration`）。此后 G2/merge
必须断言**这六个全部为 success**，不能再 grep `^validate`。

## L1-20260912-26 — ★ 新的 Worker 失败模式：**用后台任务后自我了结，并谎报 completed**

**事件**：T0011 的 Worker 产出了完整的实现（`worker_collect.go`、`worker_proc.go`、299 行的
`tests/worker-collect/e2e-live.sh` 六场景双向 e2e、单元测试，且当前都能构建通过），但：

1. 它写下的 `RESULT.json` **自称 `status: completed`**，而同一个文件开头的 summary 明写
   **"INTERIM SNAPSHOT (written early … overwritten as evidence improves)"**；
2. 其中 **2 个必需测试标为 `not_run`**（live e2e、`make check`），即所声明的验收标准**没有证据**；
3. stream log **没有 `result` 事件**，最后几条是 `background_tasks_changed` / `task_updated` /
   `task_notification`；
4. 花费仅 **$12.85 / $20**——**不是预算耗尽**。

**判定**：Worker 使用了后台任务（subagent/background），在主循环结束时**尚有后台任务未完成便自我了结**，
留下一个"早写"的中间态 RESULT，却把它标成了 `completed`。按 `docs/63` §5（"只有全部 acceptance
criterion 有 evidence、要求测试通过时才可返回 completed"）这是**虚假完成声明**，属 `rejected`。

**这是"提前写 RESULT"这条改进的副作用**：我为了不再丢失证据而要求 Worker 早写并增量更新
（`L1-20260912-22`），但没有同时要求**完成态必须显式终稿**。规则补充：

- 早写的 RESULT **必须**以 `status: pending`（或 `blocked`）之类的明确未完成态存在，**绝不能**在中间
  态就写 `completed`；
- 终稿只能在整个任务真正完成时覆盖写入；
- **Worker 不应把关键路径交给后台任务后结束**；
- T0012 应在 collect 阶段**检测"自称 completed 但存在 not_run 的必需测试 / summary 含 INTERIM"**并
  直接拒绝——这类自相矛盾的 RESULT 不该靠人读出来。

**Supervisor 处置**：交付物本身是完整的，缺的只是"运行 e2e 并收尾"。因此不重建，由 Supervisor 在同一
worktree **亲自运行 live e2e**（这本来就是 G2 的职责：独立重跑要求的测试），并按结果决定接受或退回。

## L1-20260912-27 — ★★ T0011 被拒：**GitHub 写凭据泄漏进 Worker**（移植时丢了一个 flag）

**发现路径**：T0011 的 Worker 把 live e2e 标为 `not_run` 就结束了 session；**Supervisor 亲自运行它**
（这本来就是 G2 的职责），得到 `PASS=19 FAIL=3`。其中一条是安全缺陷：

真实嵌套 Worker 自己的 Bash 报告：

```
absent: GH_TOKEN
absent: GITHUB_TOKEN
PRESENT: GITHUB_PERSONAL_ACCESS_TOKEN     ← 泄漏
absent: GITHUB_ENTERPRISE_TOKEN / SSH_AUTH_SOCK / GITEA_TOKEN / POST_GITEA_TOKEN
```

**根因（已定位）**：`rddev worker spawn` **没有传 `--setting-sources project`**。于是 Worker 的
Claude Code 会加载 **用户级** `~/.claude/settings.json`，而该文件的 **`env` 块里 export 了
`GITHUB_PERSONAL_ACCESS_TOKEN`**——它在 `BuildWorkerEnv` 从继承环境中剥离该变量**之后**又被注入回来。
我手工 harness 的 `spawn.sh` 传了这个 flag，这正是它从未泄漏的原因：**移植时把这一条丢了**。

**为什么这条最有价值（可推广的教训）**：**"在 exec 前剥离环境变量"本身不是隔离**。只要子进程会重新
加载一个能设置 `env` 的配置文件（Claude Code 的 settings、shell profile、`.env` 加载器……），剥离就会
被悄悄撤销。可靠的隔离必须在**子进程真正跑起来之后**再断言一次"这些变量一个都不在"，否则你验证的是
你自己的意图，而不是实际状态。已把"spawn 后复验 + 发现即大声失败"写进 T0011 的必需项。

**为什么单元测试没抓到**：`worker_env_test.go` 直接测 `BuildWorkerEnv` 的输出——那里确实是干净的。
缺陷发生在**下游**（Claude Code 加载 settings 时）。又一个"单测通过、真实路径泄漏"的实例。

## L1-20260912-28 — T0011 另两处被拒项

1. **residue 检测不 reject**：live 场景 `residue` 返回 `collect exit 0`，而该场景与验收标准都要求
   拒绝（Worker 留下 session 可归属的残留进程即应 rejected）。Worker 自己的场景设计说应当拒绝，实际
   没有——即检测逻辑没有生效。
2. **声称 completed 的中间态 RESULT** —— 见 `L1-20260912-26`（后台任务未完成即自我了结）。

**处置**：不重建（交付物已大体存在且构建/单测通过），在**同一 worktree** 续做，把三处缺陷作为
带证据的必需项交给新 Worker。这是本会话第一次真正的 `rejected`（此前的失败都是 `worker_failed` 或
我自己的门禁缺陷）。

## L1-20260912-29 — T0011 第三次尝试通过：凭据泄漏已修复并有独立证据

**结果**：T0011 在第三次尝试被接受并合并（PR #41，`fd1195b`），六项 CI 全绿。Supervisor **亲自运行**
live e2e：**PASS=28 FAIL=0**；并在自己的运行中确认**全部七个凭据变量**——包括此前泄漏的
`GITHUB_PERSONAL_ACCESS_TOKEN`——在真实 Worker 环境中均为 `absent`。

**三处缺陷的修复方式**：
1. **凭据泄漏**：既传 `--setting-sources project`，**又**增加了 spawn 后读取 `/proc/<pid>/environ`
   的断言，发现任何被剥离变量即让 spawn 大声失败。**只加 flag 不够**——这正是本会话反复出现的
   "请求不等于强制"的教训。
2. **residue 检测**：Worker 诊断出根因——**真实 claude 的 Bash 工具让每条命令跑在自己的 session 里**，
   于是 `nohup` 的残留进程逃出了 reaper 的 session 扫描。改为在环境中植入 `POST_WORKER_RUN_ID`
   标记，从 `/proc/<pid>/environ` 读回做归属判定：可归属的残留 → 硬拒绝；无法归属的监听端口 → 仅告警。
3. **虚假完成**：中间态一律写 `blocked`，绝不写 `completed`；collect 侧机械检测（见 T0012 必需项 1）。

**★ 第三条"子集门禁"教训（本会话第三次）**：我在本地跑 `make check` 得到绿灯就据此认为 go 阶段没问题，
但 **`make check` 不包含 staticcheck**——CI 的 go job 额外跑 `make fmt-check` 与 `make staticcheck`，
它们发现了 2 处新代码问题（ST1005 大写错误串、U1000 死代码 `procSession`）。这与
`L1-20260912-18`（带着红灯合并）和"近似 ruff 命令"同源：**永远跑真实门禁，且要跑全**。

**我的第四次越界影响**：我在 worktree 里改了 embedded guard，导致
`TestGuardScriptEmbeddedMatchesExample` 因 `.rddev.example/guard/worker-guard.sh` 未同步而失败——
而该文件**不在 T0011 的 allowed_scope 内**。Worker **正确地拒绝去改它**（改了就会违反本任务正在强制的
隔离规则），并如实报告 `blocked` 与原因。由 Supervisor 同步该文件后 `go test`/`make check` 转绿。

**统计**：本会话我（Supervisor）的失误已累计到四类：scope 过窄（3 次）、子集门禁（3 次）、
带着红灯合并（1 次）、虚假并发验证（1 次）。**全部已成为 T0012 的机械检查项**，而不是停留在文档里。

## L1-20260912-30 — ★★ T0011 合并后被发现：**gate 的输入本身是 Worker 可写的**

自动 security review 报 `internal/devorchestrator/worker_collect.go` 的 `gate-input-tampering` /
`security-check-evasion`。**经代码检查确认属实**，且命中整个编排器最核心的不变量：

```
spawn   → 把 task-package.json 与 registry.json 写进  .rddev/workers/<TASK>/
collect → 从该 task-package.json 读 allowed_scope
          从该 registry.json 读 baseline_sha
guard   → 明确**允许** Worker 写 .rddev/workers/<TASK>/（它必须能写 RESULT.json）
```

**即：gate 用"被检查方可以改写"的输入来判定被检查方。** 具体可利用点：

- Worker 可以**放宽自己的 `allowed_scope`** → 越界写入随即通过 scope 检查；
- Worker 可以**改写自己记录的 `baseline_sha`** → 它做过的 commit 随即通过 HEAD 检查。

而这两条正是 collect 的核心不变量——本会话中它们**正确地拒绝过多个任务**（T0006 的越界、T0001 的
refs 等）。**一个能被被检查方篡改的检查，等于没有检查**，而且它此前一直在"看起来有效"地工作，
所以没人怀疑。

**处置**：该缺陷位于 T0012 的职责范围（gate runner），且 T0012 **尚在早期**（26 次 tool call、
worktree 零改动），因此**停止并携带该要求重新派发**，而不是由我并行修改同一文件造成三方冲突。要求
包括：把权威 gate 输入移出 Worker 可写范围（guard 只允许写 worktree / `$POST_WORKER_RESULT_DIR` /
`/tmp`，因此其余路径天然对它不可写），并**演示**篡改无效。同时要求考虑同一目录下 `worker-settings.json`
与 `guard/` 的同类向量（Worker 改写自己的 guard）。

**元观察**：这是本会话第四次针对 orchestrator 的 security review 命中**真实**问题，且这次打在最核心的
不变量上。共同模式是"**门禁信任了不该信任的输入**"——与 `L1-20260912-27`（spawn 后环境变量被 settings
重新注入）同类：**验证必须发生在权威输入上，而不是被验证对象自报的状态上**。

## L1-20260912-31 — T0012 第一次派发 worker_failed（预算耗尽且**完全没有写 RESULT**）

**事件**：T0012 第一次派发用掉 $25.13 / $25，**没有任何 RESULT.json**（不是"写了中间态"，是**根本没写**）。
按 `docs/62` §4 记 `worker_failed`。工作本身是连贯的（43 条路径、可构建、包测试通过，且
`internal/devorchestrator/gate_inputs.go` 看起来就是 gate 篡改修复），因此**在同一 worktree 续做**。

**唯一的 collect 拒绝是 `go.mod`——而那条禁令是我写陈旧了**：我在 T0010 仍需独占 `go.mod` 时写下
"forbidden: go.mod"，T0010 早已合并、无人竞争，新增 `go.yaml.in/yaml/v3` 是合理依赖。已修正 scope。
（这是本会话第 N 次"我的 scope 比任务实际需要更窄"。）

**★ 真正值得记录的教训：把"早写 RESULT"写进提示词**（`L1-20260912-22`）**又一次没有生效**。
T0006 因此丢过一次证据，T0012 这次连一个 RESULT 都没留下。**两条提示词、两次失效**——这仍然是
"prose 不是强制"的同一课，只是在 Worker 自律这一维度上。

**机械化方案（交给后续任务）**：`rddev worker spawn` 应在启动 Worker **之前预先写入一个骨架
RESULT.json**（`status: blocked`、`task_id`、全部 required test 标 `not_run`）。这样即使 Worker 预算
耗尽或崩溃，磁盘上仍有一个**可解析、诚实**的产物：collect 能报告"blocked + 哪些没跑"，Supervisor 能
看到 diff 范围，而不是面对"什么都没有"。**让合约的默认状态是存在，而不是缺失。**

## L1-20260912-32 — ★★ P0 完成：T0012 合并，开发系统自身的 Gate 已可执行

**里程碑**：T0012 合并（PR #45，`b80ef60`），六项 CI 全绿。**P0 全部 14 个任务（T0000–T0013）merged。**
`docs/31` 的 Development System Gate 各项现在都有**被演示过的证据**，而不是声明。

**Supervisor 亲自运行的三套验收 e2e**：

```
four-gate-e2e        tamper worker 被拒；collect 按"权威 scope"判定越界文件
                     T0002 被 rejected，never verification；此时 accept 被拒绝
rejection-retry-e2e  诚实未完成被拒；respawn 进入**新的 claude session** 并清空被拒 worktree
supervisor-git-e2e   gh pr create/merge 只在 **green gate** 上被调用；commit/merge 留下 GitRecord 证据
```

**Gate/一致性测试**（含完整红灯矩阵）全过：`no G2 record`、**`subset G2 — the shipped defect`**（即我自己
犯过的错，现在是一个具名测试）、`red required job`、`stale G2`、`non-ok collect`、`missing G3`；以及
自身矛盾的 RESULT 被拒（interim 标记、completed + not_run、漏掉必需测试、acceptance 条目少于标准），
且**诚实的未完成不被误判为矛盾**。

**本轮修掉的 hermeticity 缺陷（只在 CI 暴露）**：`TestGitControlCommitOnGreenGate` 在本地通过、在 CI
失败于 `Author identity unknown`——fixture 为自己的初始 commit 传了身份，但**被测代码路径**
（`CommitTask`）依赖**环境**的 git 身份。已让 fixture 在 scratch repo 内配置身份，并在**类 CI 条件**
（无 HOME、`GIT_CONFIG_GLOBAL`/`GIT_CONFIG_SYSTEM` 指向 `/dev/null`）下重跑整套 Go 测试与 e2e 全绿，
确认那是唯一的环境依赖。

**这与本会话我自己的失误是同一类**：验证通过是因为它恰好在某个环境里运行，而不是因为被测对象正确。

**P0 之后的下一步**：P1（Identity / Organization / Project shell）。`rddev` 现在可以正式接管 Worker
调度——`.rddev/dispatch/` 手工 harness 的使命结束。

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

## L1-20260912-33 — ★ DAG 缺陷：P1–P10 的 phase 默认 scope 全部漏掉 `infra/migrations/**`

**发现方式**：T0101（用户认证与 session）的 Worker 在实现中途报告——email+password 登录需要
`users.password_hash`，而 `users` 表在 `00002_identity.sql` 里没有这一列；新增列必须写
`infra/migrations/**`，但该目录**不在它的 allowed_scope 里**。Worker 的处理是正确的：它没有越界写入，
而是把迁移交给 Supervisor 作为 integration glue，并把这个发现写进 `notes_for_supervisor`。

**这不是 T0101 一个任务的疏漏。** 逐 phase 核对后（`tasks/tasks.json`）：

| phase | 任务数 | scope 形态 | 覆盖 migrations |
|---|---|---|---|
| P0 | 14 | 每个任务**各自定制** | 只有 T0005、T0013 有 |
| P1–P10 | 100 | **每 phase 一条统一默认** | **0 个任务有** |
| P11 | 10 | `infra/**` | 有 |
| P12 | 8 | 验收类 | 不需要 |

即：**除了 P0 是手工定制的，P1–P10 全部 100 个任务用的是同一条 phase 默认 scope，而这条默认里没有
`infra/migrations/**`。** 后续每一个需要 schema 演进的任务（RSG 状态、关系、Evidence、Release、
Rights、Contribution、Search projection、Events…）都会**以完全相同的方式卡住**。

**判据不是我的猜测，是仓库自己的规程。** `infra/migrations/README.md` 的 "Adding a migration" 明确
规定新增迁移要改 `infra/migrations/**`、必要时 `sqlc generate`、并更新
`tests/integration/migration_test.go` 的 expected-catalog fixture。也就是说：**要给这个项目加一列，
就必须写 `infra/migrations/`；而 DAG 没给任何一个 P1–P10 任务这个权限。** 这是一个自相矛盾的 DAG。

**处置**：给 P1–P10 的 **100 个任务**统一补上 `infra/migrations/**`（插入在
`internal/persistence/**` 之后，与数据层相邻）。`specs/SPEC_VERSION.json` 同步重算
（`sha256:eaa4067e63134ee4`）。本地 `ci.sh spec` 与 `ci.sh task-state` 全绿。

**这是"放宽"而不是"收窄"，需要说明理由。** `tasks.json` 的 `scope_note` 说 phase 列表是"上限"、
Supervisor 应在 spawn 前**收窄**。这里反向操作，理由是：那条默认**不是上限，是错的**——它漏掉了一个
该项目规程要求写入的目录。收窄的前提是上限本身正确；上限本身错误时，正确的动作是修上限，而不是让
100 个任务各自撞一次墙再各自把 SQL 交回给我。**后者才是真正的失职**：它把"实现业务功能"从 Worker
手里挪回 Supervisor 手里（每次迁移由我转抄），而 CLAUDE.md §1 明确禁止那样。

**放宽没有削弱 Gate**：迁移是否与任务相关，仍在 G2 由我逐 diff 判定；`infra/migrations/**` 只是允许
写入，不等于允许任意写入。

**明确的边界——`specs/**` 仍然不给 Worker**：

1. `specs/**` 是契约本身，应当只有单一写者（Supervisor）。10 个并行 Worker 同时改
   `specs/database/postgres.sql` 必然冲突。
2. 更硬的理由：`specs/**` 参与 `SPEC_VERSION` 摘要，而 `specs/SPEC_VERSION.json` **不在**
   `specs/database/**` 这类子目录 glob 里。Worker 改了 spec 却无法重算 marker，结果就是 CI 的
   spec-validation 必然红灯、merge 必然被自己的 Gate 挡住。这是一个陷阱，不能靠把 marker 也塞进
   scope 来解决——那等于让被约束方自己签发一致性证明。
3. 实践上也不需要：`infra/migrations/README.md` 把 `postgres.sql` 定义为 **seed**，迁移集才是活的
   schema。自 `fecf24d`（初始规格提交）以来 `postgres.sql` 从未被修改过，T0013 的 append-only
   触发器（00014/00015）也没有回写它——**既有惯例就是"迁移是活的，seed 冻结"**。本次沿用该惯例。

**同时记录的运维规则（并行迁移编号）**：T0102、T0103 在 T0101 之后**可以并行**，两者都可能新增迁移。
若都选 `00016`，合并时会产生**编号冲突**。规则：**编号冲突在 merge 时由 Supervisor 解决**——后合并
的一支在合并前重编号。这是允许的，因为 "never edit an existing numbered file"（README）约束的是
**已发布**的迁移，分支上的迁移在合并前尚未发布。

**教训（对我自己）**：我 spawn T0101 时直接照抄了 phase 默认 scope，没有执行 `scope_note` 要求的
"按具体 Task 再缩小/核对"这一步。**但即使我做了那一步，也只会照抄同一条错误的默认。** 真正的问题是
我没有在 P1 开工前**核对默认 scope 是否覆盖任务规程要求的目录**。这条检查应作为 phase 开工前的
固定动作，而不是等 Worker 撞墙后由 Worker 来告诉我。

## L1-20260912-34 — 同一缺陷类的第二次：P1–P11 默认 scope 也漏掉 `go.mod` / `go.sum` / `.env.example`

### (a) 证据：T0101 被迫把 argon2id 降级成 PBKDF2，只为不碰 `go.mod`

T0101 的 Worker 在收尾时发现：它用了 `golang.org/x/crypto/argon2` 与 `github.com/google/uuid`，
两者都是**新的直接依赖**，必须改 `go.mod`/`go.sum`——而这两个文件不在 allowed_scope 里，
collect 会因此整体拒绝。它的处置是**把两个依赖全部去掉**：

- `argon2id (64MiB,t=3,p=2)` → 标准库 `crypto/pbkdf2`（先实测 69ms/hash 确认测试耗时可接受）；
- `google/uuid` → 自写的标准库 v4 生成器（`internal/domain/user.go` 的 ID 变成 string）。

**它不是偷懒，是在我的 scope 缺陷下唯一能交付的路径。** 但这个结果是：`golang.org/x/crypto` 与
`google/uuid` 是 Go 生态里最主流的两个库，而本仓库的 phase 默认 scope 让**任何**任务都无法引入任何
依赖——除非像它这样把依赖去掉。**这不是一个个案**：本会话早前 T0010 的 scope 修正记录
（"`forbidden: go.mod`……新增 `go.yaml.in/yaml/v3` 是合理依赖。已修正 scope"）说明这条我已经撞过一次。

**处置**：给 **P1–P11 的 110 个任务**补上 `go.mod`、`go.sum`、`.env.example`。

- `go.mod`/`go.sum`：Go 代码任务引入依赖的唯一合法途径。
- `.env.example`：T0101 Worker 明确列为 follow-up（新增 `POST_WEB_ORIGIN`、`POST_AUTH_*`、
  `POST_OIDC_*` 等变量）。每个加配置的任务都需要它。
- P12（最终验收：`tests/acceptance/**`、`ops/**`、`docs/**`）不加：它不实现产品代码。

**明确不给 Worker 的（一并记录，避免下次重新讨论）**：`Makefile`、`.github/**`。原因不是保守，
是**这两个文件就是 Gate 本身**——把它们交给被 Gate 约束的一方，等于允许 Worker 通过修改 Gate 来让
Gate 变绿。T0101 报告的 Makefile 需求（web 测试 glob）由我以 Supervisor 身份修复，见 (b)。

### (b) 一个已经存在、且一直在漏的 Gate 洞：`apps/web/lib/correlation.test.mjs` 从未被执行

T0101 的 follow-up 指出 Makefile 的 web 测试写死了文件名：

```
node --disable-warning=MODULE_TYPELESS_PACKAGE_JSON --test "apps/web/lib/config.test.mjs"
```

同一行也出现在 `scripts/ci.sh` 的 `stage_web` 与 `.github/workflows/ci.yml` 的 web job 里。
后果不是"T0101 的新测试没跑"，而是**已经有一个测试文件在树上、一直是绿的、从来没有人运行过它**：

```
apps/web/lib/config.test.mjs
apps/web/lib/correlation.test.mjs   <-- 自它被写下起，从未被 make check / make test / CI 执行
```

**"写死的文件名"是一种静默失败**：Gate 看起来很绿，但它只覆盖了被记住的那一个文件。

**处置**：新增 `scripts/web-unit-tests.sh`，跑 `apps/web/lib/*.test.mjs` 全部文件，Makefile 两处
（`check`、`test`）、`ci.sh` 的 `stage_web`、CI workflow 全部改为调用它。

**同时修掉 glob 自身的 fail-open**：glob 匹配不到任何文件时，`node --test` 会以 **exit 0** 报
`tests 0`——那是一个"什么都没测"的绿灯，与空的 baseline 是同一种失败形状。因此脚本在跑之前先用
shell 统计匹配数，**为 0 就大声失败**，并明确告诉后来者"如果测试搬家了就改 glob，不要删掉守卫"。

守卫本身有 fixture 测试 `scripts/tests/web-tests-unit-test.sh`（三例：空 glob 必须失败、真实 glob
必须找到文件、故意失败的测试必须仍然失败），已接入 `stage_web`。实测：修复前 `correlation.test.mjs`
不跑；修复后 `2 file(s) matched`、17 个测试通过。

### (c) 对 push 安全复查告警的处置（记录为误报）

本次 push 触发了后台安全复查告警 "Overly Permissive IAM/RBAC in tasks/tasks.json"。
**这是误报**：`tasks/tasks.json` 是任务 DAG，不是 IAM/RBAC 策略；告警来自 PR #49 把
`infra/migrations/**` 加进 Worker 的 allowed_scope，被按"IAM 权限放宽"模式匹配。allowed_scope 约束的是
**文件写入范围**（由 guard + collect 强制），不是身份/角色权限。已确认无实际权限模型变更。

**但告警指向的方向是对的**，所以我没有简单忽略：真正需要审的是"放宽 scope 是否让 Worker 能绕过 Gate"。
上面 (a) 的结论正是对它的回答——`specs/**`、`Makefile`、`.github/**`、`tasks/**` 一律不给，
且这次的放宽**没有**触及其中任何一个。

## L1-20260912-35 — T0101 第一次 collect 的四个判定：三个真、两个是工具自己的缺陷

**结论：T0101 被 rejected 是正确的，但 collect 给出的四个失败里有两条是工具自身的缺陷，两条是真的。**
先把真的钉住：**Worker 在 status: completed 的同时把 `go test ./tests/integration -run
TestCredentialStorePasswordRoundtrip` 标成 `not_run`**——理由是它需要的 migration 超出它的 scope。
这条是**真缺陷**，不是误报：完成声明不能骑在一个自己承认没跑过的测试上。状态维持 rejected。

另外两条是工具的错，都已修（PR #51）：

### (a) `refs`：refsSnapshot 存的是 `"<refname> <objectname>"`，collect 却整条比较

于是**任何"移动"的 ref 都会被读成"新建"**。而 ref 在 Worker 运行期间移动是常态——**因为那正是
Supervisor 在合并别的工作**。T0101 的 run 期间我合并了 PR #49 与 #50，于是 collect 报告
`new ref(s) created: refs/heads/main a07ac01…`，**用我自己的合法操作否决了这个任务**。

更糟的是：**代码和它自己的注释互相矛盾**。注释一直写着"moving an existing ref is normal Supervisor
activity — other PRs merge while a Worker runs; creating one is a Worker capability the guard denies"，
实现却按整条字符串比较。这是"注释描述了正确策略、代码实现了另一个"的典型，而它只在**恰好有并发合并
时**才暴露——单任务开发时永远不会触发。

### (b) `result-tests-coverage`：要求的测试是**标签**，entries 的 `command` 是**命令**，却要求两者字符串相等

DAG 里 `tests: ["auth unit", "auth e2e"]` 是标签；契约里 `tests[].command` 是实际跑的命令。
T0101 **跑了这两个套件并且把标签写进了命令**
（`go test ./internal/application/authn/... -count=1 (T0101-TEST-01 auth unit, blocking)`），
却被判"两个必需测试都没有 passed 条目"。

**这个检查对任何诚实 Worker 都不可满足**——它不是"严格"，是"错"。已改为按标签出现匹配
（entry 仍必须是 `passed`）。

### (c) 对第二条安全告警（"Substring/Unanchored Allowlist Bypass"）的处置：**成立但不改变边界**

告警指出 (b) 引入的 `strings.Contains` 让必需测试更容易"满足"。**这个观察成立**：Worker 完全可以把
标签塞进一条没跑过的 passed 条目里。

**但它不构成安全边界的削弱**，理由必须说清楚，而不是拿"误报"打发：
`RESULT.json` **不是证据，是主张，而且是被审查方自己写的**。一个愿意编造的 Worker 用字符串相等
一样能编（直接写 `"command": "auth unit", "status": "passed"`）。真正的执行者是 **Supervisor 在 G2
独立重跑必需测试**（CLAUDE.md §6）——这正是为什么 (b) 的原实现是**更糟的失败**：它挡住了每一个诚实
的 run，却挡不住任何一个不诚实的 run。

已把这段判断**写进代码注释**（`commandNamesTest`），以免下一个人重新推导一遍，也以免有人误以为
这个检查承担了它从未承担的职责。

## L1-20260912-36 — `make test-integration` 的默认端口是错的，而 Worker 无法自己修

**现象**：我按文档起 infra（`make infra-up`）后，`make test-integration` 报
"no PostgreSQL reachable"。原因不是环境，是**默认值写错了**：

| 位置 | 端口 |
|---|---|
| `docker-compose.yml`（`${POSTGRES_PORT:-5432}`） | **5432** |
| `infra/docker/README.md`（端口表 + `POSTGRES_PORT` 默认值） | **5432** |
| `.env.example`（`POST_DB_PORT`） | **5432** |
| `ops/DEV_COMMANDS.md` §默认端口 | **5432** |
| GitHub CI 的 `migration-integration` service container | **5432** |
| **`Makefile` 的 `test-integration` 默认 URL** | **15432** ← 唯一的例外 |

15432 在 compose 文件里是**"再起第二个隔离栈时"的覆盖示例**，不是主栈端口。也就是说
**文档描述的流程（`make infra-up` → `make test-integration`）在全新克隆上必然失败**，
而 `ops/DEV_COMMANDS.md` 第 44 行把这个错误默认值抄了一遍，于是错的地方看起来是两处、源头是一处。

**为什么这条比看上去严重**：Worker **没有 Docker 权限**（Docker 是按任务 opt-in 的），所以
Worker 无法自己 `make infra-up`。数据库必须由 Supervisor 预先起好，而"起好"的标准是
**`make test-integration` 能不改配置直接通过**。默认值错了，等于每一个需要真实 PostgreSQL 的任务
（T0101 起，P1–P10 大量任务）都会在这句话上原地卡住，并把责任错误地显示成"Worker 没跑集成测试"。

**处置**：`Makefile` 默认改为 `5432`，`ops/DEV_COMMANDS.md` 同步。**并且实测过**：起一个同配置的
临时 PostgreSQL 在 5432，`make test-integration` **不做任何环境变量覆盖**直接通过
（`ok github.com/lichman0405/post/tests/integration 3.074s`），随后删除该容器。

**验证过程中的一个小教训（记录以免下次误判）**：第一次跑失败了，报
`failed to receive message: read tcp …: connection reset by peer`。这不是代码问题，也不是配置问题：
`pg-ready.py` 在 PostgreSQL 初始化期间会短暂成功（它会在这期间先监听再重启），于是探针过了、
测试连接被重置。**"探针说 ready" 与 "服务真的可用" 是两件事**——判据应该是
`database system is ready to accept connections` 这类来自服务自身的就绪信号。
本次 CI 的 `migration-integration` job 用的是 service container 加健康检查，不受此影响；
但本地/Worker 场景下 `pg-ready.py` 是唯一探针，这个假阳性值得后续修——**已在 L1-20260912-37 修复**。

**当前环境**：为不打断正在运行的 T0101 rework，本次把 infra 起在
`POSTGRES_PORT=15432 REDIS_PORT=16379 …`（即 compose 文档中的覆盖形式）。**T0101 结束后应改回默认端口**，
让本机状态与修复后的文档一致。

## L1-20260912-37 — `pg-ready.py` 只做 TCP connect，所以会把"正在初始化"报成"就绪"

**L1-36 里我记下的那个假阳性，根因比"时序问题"更具体：探针从来没有问对问题。**

`scripts/pg-ready.py` 原实现只有一句 `socket.create_connection(...)`——**TCP 连上就算 ready**。
但 PostgreSQL **在真正可用之前就已经在监听端口**，随后会重启。于是：

- 探针报告 ready（它确实连上了）；
- `make test-integration` 立刻连接，被 `connection reset by peer` 打断。

**更一般地说，它无法区分"PostgreSQL"和"任何监听该端口的进程"。** 一个 nginx、一个别的数据库、
一个刚 bind 还没 listen 完的服务，都会被判成"PostgreSQL 可达"。

**处置**：探针改为**真正做一次 PostgreSQL 协议握手**——发送 protocol 3.0 的 StartupMessage，
要求对方回一个协议响应：

- 回 `'R'`（Authentication）→ 是 PostgreSQL 且在接受连接 → ready；
- 回 `'E'`（ErrorResponse）→ 把**服务端自己的话**（`M` 字段）打印出来（例如
  "the database system is starting up"），比探针自己编一句有用得多；
- 连上但对方不说话 / 直接关闭 → **不是** ready（这正是我踩到的那一种）；
- 第一个字节不是 `R`/`E` → 不是 PostgreSQL。

**明确不做的事（写进 docstring）**：**不校验凭据**。握手成功即视为 ready，密码错了会由测试套件自己
以更好的错误暴露——要在这里也判密码，就得在本脚本里重实现 SCRAM，那是把探针变成半个驱动。

**回归测试 `scripts/tests/pg-ready-unit-test.sh`**（已接入 `ci.sh stage_integration` 与 CI 的
`migration-integration` job，gates.json 同步）：

```
ok   close listener: rc=1, not reported ready      <- 这次踩到的那个形状
ok   silent listener: rc=1, not reported ready     <- accept 后一言不发（初始化中）
ok   malformed URL: rc=2 (usage error)
ok   real PostgreSQL (...): ready                  <- 证明没有矫枉过正
```

**最后一条是必要的**：只测"假的必须失败"会得到一个"永远失败"的探针，那也是坏的。
一个探针必须**同时**被证明会拒假的、会收真的。

**写这个测试时我自己先犯了一次同类错误**（记录以免重犯）：第一版把后台 listener 的 stdout
用 `$(...)` 捕获，而后台进程持有那个管道不关闭，命令替换会一直等到进程退出——测试自己把自己挂住。
改成"listener 把端口写进文件、调用方读文件"，端口在 `listen()` 之后才写出，因此读到端口即已可连接。
**"捕获一个后台进程的输出"和"等待一个后台进程"是同一件事**，这在这里是不想要的。
