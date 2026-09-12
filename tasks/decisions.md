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

## L3-20260912-2 — Merge 授权分级（owner 裁定）

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
