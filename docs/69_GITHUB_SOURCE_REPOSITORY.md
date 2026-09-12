# 69 — GitHub Source Repository & Supervisor Control Plane

## 1. 目标

POST 的产品开发使用一个 canonical GitHub repository：`lichman0405/post`。本文件定义源码 GitHub control plane，防止它与产品运行时 Gitea infrastructure 混淆。

## 2. Canonical repository

```text
owner/name: lichman0405/post
default branch: main
https remote: https://github.com/lichman0405/post.git
ssh remote: git@github.com:lichman0405/post.git
```

`rddev doctor` 最终必须能够校验：

- 当前目录位于 Git repository；
- `origin` 的 normalized owner/name 为 `lichman0405/post`；
- default integration branch 为 `main`；
- Supervisor environment 可访问 GitHub；
- Worker environment 没有 GitHub write credential；
- Supervisor 与 Worker credential boundary 可通过自动测试证明。

其中 origin 归一化、integration branch 与 visibility 三项已由
`scripts/source_repo_preflight.py` 实现为确定性、可审计的 preflight（§2.1），
CI 通过 `scripts/tests/spec-validation-smoke-test.sh` 在真实 checkout 上执行
真实 `git remote -v` 捕获并验证其解析正确性。

## 2.1 source_repo_preflight 与注入契约

`scripts/source_repo_preflight.py` 是 canonical source repository 的确定性 preflight：

- 输出：human 行 + `--json`（schema：`specs/orchestrator/source-repo-preflight.schema.json`）；
- 退出码：`0` bless（允许 push）/ `1` refused（SPEC_BLOCKED 或 process 原因，禁止 push）/
  `2` usage / `3` spec 不可读无法继续；
- 默认行为是**安全、可测试、无需凭据**的：不触碰网络、不运行 `gh`；visibility 观测报告
  `unknown / cannot verify`，verdict 拒绝 bless。真实探测必须显式 opt-in：
  `--check-visibility`（执行 `gh api repos/{owner}/{name} --jq .visibility` **与**
  `--jq .default_branch`，需要 Supervisor 的 gh credential），或 `--observed-visibility <value>`
  直接给出操作者实测值（优先级最高）。与 T0000 的 `ops/doctor.sh --check-docker-daemon` 同一模式。
- **凭据安全**：remote URL 可能内嵌 token（CI checkout 常见形式
  `https://x-access-token:<PAT>@github.com/owner/name.git`）。任何进入 `measured`/`detail`
  的 URL 都必须先经 `speclib.redact_url()` 脱敏，token 绝不进入 stdout、`--json` 文档或 CI 日志。

注入契约（test-only）：`--fixture DIR` 用 fixture 文件覆盖测量输入，决策逻辑在**不触碰真实
主机与网络**的情况下可测（fixture 模式下不执行任何真实 git/gh 命令；未提供的输入按 unknown
报告，绝不回退真实测量）：

| fixture 文件 | 内容 |
|---|---|
| `git-remote-v.txt` | 原始 `git remote -v` 输出（`#` 开头的行忽略） |
| `git-remote-get-url.txt` | 原始 `git remote get-url origin` 输出 |
| `origin-head.txt` | 原始 `git symbolic-ref refs/remotes/origin/HEAD` 输出（如 `refs/remotes/origin/main`） |
| `default-branch.txt` | 远端权威 default branch（如 `main`） |
| `visibility.txt` | 观测 visibility：`public` / `private` / `internal` / `unknown` |
| `gh-visibility.txt` | `gh api ... --jq .visibility` 原始输出（`--check-visibility` 时使用） |
| `gh-default-branch.txt` | `gh api ... --jq .default_branch` 原始输出（`--check-visibility` 时使用） |

校验项（固定顺序，每个结果含 id/status/measured/expected/detail/source）：

| check | 判定 | 阻塞 blessing |
|---|---|---|
| `SPEC-PARSE` | spec YAML 可解析 | 不可解析 → 退出 3，无法继续 |
| `SPEC-CANONICAL` | provider=github、full_name=owner/name、default_branch=main、clone URL 自洽 | 失败 → `spec_not_canonical` |
| `SPEC-EXPECTATION` | `visibility_expectation`（value/confirmed_by/confirmed_at）已记录且有效 | 失败 → `expectation_unrecorded` / `expectation_invalid` |
| `REPO-CANONICAL` | `origin` 归一化后 == spec 记录的 owner/name | 不一致 → `remote_not_canonical`；无法解析 → `remote_unparseable`；无 origin → `remote_missing`；无法确定 → `remote_unverifiable` |
| `BRANCH-DEFAULT` | integration branch == spec default_branch。来源优先 `refs/remotes/origin/HEAD`，本地记录缺失时回退到远端权威 `.default_branch`（`--check-visibility`） | 不一致 → `branch_mismatch`（含本地与远端记录互相矛盾的 state drift）；**无法确定 → `branch_unverifiable`，阻塞**（fail-closed：无法验证 integration branch 的 gate 不得 bless） |
| `VISIBILITY` | 实时观测 == owner 确认预期（三态模型，§5） | 不一致 → `visibility_mismatch`；无法确定 → `visibility_unverifiable` |
| `SPEC-VERSION` | checked-in 规格版本标记 == 推导 digest（`scripts/spec_version.py`） | 过期 → `version_stale`；缺失 → `version_missing` |

verdict 的每个 reason 带 `level`：`SPEC_BLOCKED`（governance 信号）或 `process`（簿记错误）。

## 3. Supervisor workflow

建议生命周期：

```text
Task ready
  ↓
Supervisor optionally creates GitHub Issue
  ↓
Supervisor creates task branch + worktree
  ↓
rddev starts independent Claude Code Worker
  ↓
Worker modifies files + RESULT.json, no commit
  ↓
G1 Worker Local Gate
  ↓
Supervisor G2 acceptance
  ↓
Supervisor commit + push
  ↓
PR + G3/G4
  ↓
Supervisor merge
  ↓
Task status -> merged
```

任务 ID 必须进入 branch/commit/PR traceability。推荐：

```text
branch: task/T0042-short-slug
commit: T0042: <imperative summary>
PR: [T0042] <task title>
```

如果一个 GitHub Issue 对应多个 machine tasks，则 PR body 中列出全部 Task IDs；不得强行一任务一 Issue。

## 3.1 Review 与 merge 授权（owner 授权，2026-09-12）

对 L0/L1 级别的常规实现任务与产品代码 PR，Supervisor 无需逐 PR 请求人工批准，可自行
review 并 merge，前提是 `docs/62` §3.1 的六项条件**同时**成立：

1. Worker 已 `completed` 且 Supervisor 完成独立 G2；
2. G1/G2/G3/G4 全部通过；
3. CI 全绿；
4. 无未解决 review 意见；
5. diff 未超出 `allowed_scope`；
6. 未改动既定产品语义、安全边界、权限模型、科研语义或核心架构原则。

必须停止并请求人工决定：**L3** 决策；改变既定核心架构原则的**重大 L2** 决策；需要新的
外部凭证/付费服务/账号授权；无法用现有规格解决的 `SPEC_BLOCKED`。

**不得**因等待批准而停滞的类别：普通实现、bug fix、测试、重构、依赖范围内的技术选型。

现状约束（如实记录，不得伪造）：本仓库在私有 + 当前 plan 下**无法启用 GitHub branch
protection**（见 `tasks/decisions.md` F-20260912-1）。因此 "required review" 与 "禁 force push"
**不是**平台强制的，只由 Supervisor 纪律 + credential 独占保证。不得在任何文档或 PR 中声称
已启用 branch protection。CI（`.github/workflows/spec-validation.yml`）是真实的 required gate，
必须全绿。

第三方 app（CodeRabbit）对本仓库 PR 提供 **advisory** review，不是 required check；owner 已知悉
其会读取私有仓库 PR 内容并选择保留（`tasks/decisions.md` L3-20260912-3）。

## 4. Worker security boundary

Worker 启动时：

- unset `GH_TOKEN`, `GITHUB_TOKEN` 及同类变量；
- 不挂载 Supervisor SSH private keys；
- `gh auth status` 不得显示 write-capable authenticated account；
- Claude permissions deny Git control commands；
- `rddev collect` 校验 HEAD 未移动、refs 未改变；
- Worker 仅提交文件系统 diff 和符合 schema 的 `RESULT.json`。

仅靠提示词声明“不要 commit”不构成权限隔离。

## 5. GitHub repository visibility

Visibility 属于 repository governance，不是产品 runtime visibility。Visibility 有三个不同的语义实体，**不得混淆**：

1. **generation-time 记录**（历史事实，永不改写）：`specs/orchestrator/source-repository.yaml`
   的 `visibility_at_package_generation`（`public`）记录规格包生成当时的 visibility。
2. **owner 确认的当前预期**（治理事实）：同文件 `visibility_expectation` 记录 owner 书面确认的
   预期值。当前（2026-09-12 起）：`private`，见 `tasks/decisions.md` L3-20260912-1 —— Supervisor
   实测 remote 为 `PRIVATE`，与生成时记录不一致，按本文件 §5 进入 L3 governance 阻塞，owner 书面
   确认 PRIVATE 是有意选择。
3. **实时观测**（只能由 Supervisor/operator 环境确定）：push 前重新查询 remote 状态
   （`gh api repos/lichman0405/post --jq .visibility` 或等效手段）。

每次 push 前（至少首次 push 前）Supervisor 必须运行
`python3 scripts/source_repo_preflight.py --check-visibility`（或 `--observed-visibility <value>`）。
**以下任一情形 preflight 输出 SPEC_BLOCKED verdict 并拒绝 bless（退出码 1），禁止 push：**

- 实时观测与 `visibility_expectation` 不一致（`visibility_mismatch`）；
- `visibility_expectation` 未记录或无效（`expectation_unrecorded` / `expectation_invalid`）——
  必须先取得 owner 书面确认并写入规格，再重新运行；
- visibility 无法确定（`visibility_unverifiable`）——preflight 报告
  `unknown / cannot verify`，**不得猜测，不得静默通过**。

**禁止把 "private 永远正确" 编码进实现**：preflight 只比较"记录的预期"与"实时观测"。若 owner
未来改变预期，更新 `visibility_expectation`（并在 `tasks/decisions.md` 记录裁定）即可，无需改代码。

若 repository 为 Public，则提交到 GitHub 的所有规格、源代码、ADR、测试 fixture 都应视为公开信息。
Secret、真实客户数据、内部 credentials、私有数据集绝不能进入 repository。

如果 remote visibility 与项目 owner 的预期不一致，属于 L3 governance 阻塞：停止 push 并请求人工确认，
不得自行改变 repository visibility。

## 6. Branch policy target

P0 CI 就绪后，Supervisor 应配置/提示人工配置以下目标规则（GitHub plan/API 权限受限时不得伪造成功）：

- `main` 不作为 Worker 直接修改目标；
- merge 前 required CI Gate 全绿；
- 禁止 force push 到 `main`；
- 删除已 merge task branch 可自动化；
- merge strategy 优先 squash 或 merge commit，项目只选一种 canonical 策略并写 ADR/L1 decision；
- release tag 只能由 Supervisor release workflow 创建。

## 7. GitHub 与 Gitea mapping 禁止

禁止直接把源码 GitHub 的：User / Issue / PR / Release 模型复用为 POST runtime 的 Project / Research PR / Release 模型。

源码 GitHub 是 POST 的工程协作工具；GiteaProvider 是 POST 产品实现的一部分。二者的身份域、权限域、生命周期均独立。
