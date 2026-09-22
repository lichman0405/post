# 空窗待办 —— 一次只做清单上写着的这些（Supervisor 自己看）

**空窗是什么**：`tasks/tasks.json` 与 `specs/**` 是规格指纹的输入（`scripts/spec_version.py`）。
只要有**还没合并的任务**在飞，改这两处，`specs/SPEC_VERSION.json` 就动，
那个在飞任务的 G2（在**当前** main 上合成验收树）立刻变红、main 也红。
所以这些改动**只能在"没有任何任务在飞"的时候做**，那就是空窗。

**这份文件是空窗工作的唯一清单。** 记在这里而不是散在 `tasks/decisions.md` 的叙事里，
理由很直白：**散文会被忘掉，清单不会**（我吃过这个亏——"compose-link 少改了派生件"
那次就是藏在叙事里没人再想起）。空窗一开，**只做这份清单上写着的**，不临时发挥。

**每条都带"怎么确认它还需要做"**：空窗可能隔很久才来，来的时候世界已经变了。

---

## A. ~~落 T0511 的任务书，然后解冻 T0812~~ —— **已完结（2026-09-23 核）**

`T0511` 与 `T0812` 都已 `merged`（`tasks/task_status.json`）。空窗清单上唯一一条"真待落地"落地了：
T0812 的 `blocked` 由 T0511 的合并解冻，两本都收口。

**唯一的遗留是记账噪音**：`python3 .rddev/tools/packages-vs-dag.py | grep T0511` 现在报
`DIFFERS fields: deliverables` —— 这是**已合并任务的过期草稿**（live 那份在返工里改过 `deliverables`），
不是待落地。判据同 `tasks/packages/README.md`：`NOT IN DAG` 才是活，`DIFFERS` 对**已合并**的任务只是留档。
**不再需要任何动作。**

---

## B. 让 CI 真的跑 `tests/e2e` —— **已立账为 T1212（2026-09-23），并当场收窄**

**立账了，但比这条原本的描述窄。** 今天重新在树上核过：`tests/e2e` 里**大多数**测试是进程内的
（`httptest` + `miniredis` + `memstore`，`tests/e2e/auth_e2e_test.go:46` 的 `newE2EEnv`），
在 CI 的 `go` job 里**是真跑的**；**只有两个**旅程要真 PostgreSQL ——
`tests/e2e/conflict_e2e_test.go:107`（冲突解决）与 `tests/e2e/discussion_promote_e2e_test.go:112`
（话题升格）—— 库不可达时**静默** `t.Skipf`，job 照报绿。
所以口子是真的，但只有**两条链路**，不是「整个 `tests/e2e` 没跑过」。

**工人那一半**（`tasks/packages/T1212.json`，`allowed_scope` 只给 `tests/e2e/**` 与
`internal/persistence/testdb/**`）：共享判据 `POST_REQUIRE_E2E_DB=1`（库不可达时 `t.Fatalf` 而非 `t.Skipf`，
未设时行为一字不变）＋一个证明守卫**能红**的测试。

**留在我手上的那一半**（工人不许碰 `.github/**`——两道作业步骤必须逐步一致，改一处要两处一起走）：
① 带库的 `migration-integration` job 加一步 `POST_REQUIRE_E2E_DB=1 go test ./tests/e2e -count=1`；
② 让没有库的 `go` job 把 `./tests/e2e` 从包列表里**摘掉**（今天它是被 `grep -v '/tests/integration'`
顺带带进去的）。`T1212` 合并后做，与 `specs/orchestrator/gates.json` 同笔改（`TestCIWorkflowMatchesGateSpec`）。

---

## C. 两处 baseline 的"只许变短"守卫（**新，2026-09-19 发现**）

**为什么**：`ops/ci/gofmt-baseline.txt` 与 `ops/ci/staticcheck-baseline.txt` 是两道门的豁免名单，
文件开头自己写着"New files are never added to this list"——**没有东西在检查这条**。
把新文件加进名单 = 门变绿，**静默**、无痕。**20 本任务的 scope 含 `ops/**`**，工人改它不算越界。
（判据：**静默的失败才值得加机械守卫**；大声会自己炸的那些不加。）

**怎么确认还需要做**：`grep -rn "baseline" scripts/ Makefile .github/workflows/ci.yml` ——
若仍只有"过滤报告行"的逻辑、没有"相对基准是否新增条目"的检查，就还没做。

**设计（已定）**：新脚本 `scripts/baseline-growth-check.sh`，语义：

- 取一个**基准版本**（`BASE_REV`，CI 里传 PR 的 base sha；本地默认 `origin/main`）；
- **新增的非注释/非空条目 → 红**（逐条打印出来）；**删除条目 / 改注释 / 重排 → 绿**（鼓励变短）；
- **基准取不到 → fail-closed 报错**，并在错误里点明"CI 侧需要 `fetch-depth: 0`"；
- 配 `scripts/tests/baseline-growth-unit-test.sh`（本仓库惯例：每道检查都要有证明它**能红**的测试）。

**为什么必须和 CI 一起改**：`actions/checkout@v4` 默认 `fetch-depth: 1`，基准 ref 不在本地，
塞进 `make fmt-check` 会让**每次 CI 变红**。所以它是一条**新 CI 步骤**（`fetch-depth: 0`），
而改 `.github/workflows/ci.yml` 就必须同步改 **`specs/orchestrator/gates.json`**（指纹输入）。

**优先级**：**排在 A 之后。** 空窗是稀缺资源，落任务书能解锁工人（吞吐），这条是防护（正确性）。

---

## D. 开一条新账：PR 页缺 Discussion 区块（**新，2026-09-19 记**）

**为什么**：`docs/42_PAGE_SPECS.md:13` 逐字写着 PR 页的区块里有 **Discussion**；
`apps/web/app/(main)/projects/[id]/pulls/[number]/page.tsx` 里**一次都没出现这个词**。
T0811 合并后**接口侧已经有了**（开话题、评论、升格），但界面上仍然**开不了、看不到**。
这不是 T0811 违约（它 7 条验收标准里没有 UI 那条，`deliverables: []`，同阶段的 T0805
也是同一形状——**API + 客户端库，不带页面**），是**页面规格与交付之间的缺口**，
而且**没有任何一本在册的任务管它**（我查过：全 DAG 里只有 T0811 提到 discussion）。

**怎么确认还需要做**：`grep -rn "Discussion" apps/web/app/\(main\)/projects/\[id\]/pulls/` → 仍为空。

**要做的事**：空窗里**立一本任务**（写进 `tasks/packages/` 再落），覆盖
"PR 页 Discussion 区块 + 知识对象页/项目页的话题入口"，接 T0811 已有的路由与客户端库。
**若认定 V1 不需要它**（即这一格不属于 V1 验收），那就**改 `docs/42:13` 把这一格删掉**——
两条路都行，**但不许两头都不做、让它悬着**。

---

## E. 记着但**不属于空窗**的事（写在这是为了不忘，别在空窗里顺手做）

- **T0602 一合并，立刻清两条判断点**：`./bin/rddev drive --clear-decision T0808` 与
  `--clear-decision T0809`。**驱动会跳过带判断点的任务**——不清，那两个 PR 永远不会被重试合并。
  **这不是空窗工作，是合并那一刻的动作。**
- **这些"合并后小修"现在不算空窗工作，但也不该立刻做**（2026-09-19 记）：
  T0602 已合（`0d33097`），它的三处小修**技术上随时可做**（都不在指纹输入里）。
  但 T0510 / T0811 / T0814 **三本在飞的任务，scope 全都含 `internal/persistence/**` 与
  `internal/application/**`**，而这正是这些修改要碰的文件。**我一改，就可能把某本在飞任务的补丁
  变成"合不进当前 main"**——刚为 T0811 付过一次这个代价（rebaseline + 重做 + 重评）。
  **判据是代价不对称**：等它们落地是零成本，撞车是一次 rebaseline 加一轮重做。
  **所以：等这三本收口后再批量做。**
- **T0602 合并后的三处小修**（评审 `approve` 时带的 `minor`/`nit`，我判为"记账后自己修"）：
  ① `internal/application/aborts/service.go:792` `requireShape` 只用长度卡 `replacement_ref`，
  而 00100 的 CHECK 要 `length(btrim(...)) BETWEEN 1 AND 512`——**全空格值**（`"   "`）能过命令、
  死在 CHECK，23514 没进映射表（`internal/persistence/scientific_object_store.go:180-183` 只有
  22P02/23503/23502），最终被答成 **503 `retryable:true`**。评审**在真库上复现过**：
  这是"把调用方的错报成可重试的服务错"，客户端会一直重试一个**永远不可能成功**的请求。
  修法：`requireShape` 里拒掉 `TrimSpace` 为空的值（**这就是 SQL 已经写明的规则，不是我新造语义**）。
  ② `internal/persistence/pullrequest_store.go:243` 与 `internal/persistence/scientific_object_store.go:345`
  两处**新写的注释**把 SQL 的空串字面量 `''` 打成了弯引号 `”`，读起来像被截断。
  ③ T0602 的 `RESULT.json` 里 `acceptance[3].evidence` 引的 sha256 是**另一个文件**的
  （`scientific_object_abort.go` 的，却写成"这个文件的"），事实本身为真（改写确实回滚了，
  评审独立复现过），**只是引错了对象**——`RESULT.json` 是工人的留档，**我不改它**，
  只把更正记进 `tasks/decisions.md`。
- **T0811 合并后的三处小修**（评审 `approve` 时带的 `minor`/`nit`）：
  ① `cmd/api/discussionhttp/handlers.go:451` 的 `DELETE .../threads/{threadId}/comments/{commentId}`
  **从不读** `threadId`，所以**路径与效果可以不一致**（删的评论其实在另一个话题里；不是越权——
  store 仍卡所有权与项目范围）。同文件的 `handlePromote` 就是反例（它传 ThreadID 让命令拒配对），
  **照那个先例补上**即可。
  ② `infra/migrations/00104_discussions.sql:127` 的 CHECK 注释说 ref「cannot name nothing」——
  **我和评审各自独立用 psql 量过**：`'issue:'` 能过，它钉的是"kind 前缀与 ref 一致"。
  产品路径够不到空值，**保护完整 → 记账后合并**，合并后把话说准。
  ③ `internal/persistence/discussion_store.go:282` PR 目标 id **验过就原样存**：
  对 `'007'` 能通过存在性检查（按 7 去查），却把 `'007'` 存进 `target_id`，
  以后按 `'7'` 列/筛就找不到。修法：pull_request 目标**存解析后的十进制形式**。
- **`make -n <target>` 之类的 CI 步骤自检**：**我倾向于不做**。它要防的失败是**大声**的
  （目标不存在 → make 自己报错），不是静默的，性价比不够。列在这里只为说明**我考虑过并否掉了**。

---

## F. 本窗（2026-09-23）立的三本账 —— 记在这里是为了下次别再重新发现一遍

**它们不是空窗待办，是已立账、等驱动派工的任务**。列出来只有一个理由：这三件事各自的**由来**都在这份
文件的语境里（B 条、T1209 的复核、T1207 的复核），下次开窗时能一眼看出「已经有人立过账了」。

- **T1210**（`v1_required=false`，`tests/acceptance/**`，G3 `mof-canonical`）：V1 证书在新基准上重生成。
  由来：T1207 的第五轮独立复核五条意见 + T1209 让 R10 那句「SAST、容器扫描、SBOM 三项缺席」不再成立。
  它依赖 T1209 与 T1207（都已合并）。
- **T1211**（`v1_required=false`，`tests/security/**`、`ops/security/**`、`ops/ci/**`、`Makefile`，
  G3 `security-smoke`）：安全仪器**自己的**欠账（许可证判决不落地、`POST_SBOM_REUSE` 的真值判断、
  uv/govulncheck 的钉法、死掉的 `check_floor()`、指错文件名的注释、`kinds` 图例、判过期磁盘 SBOM、
  `container-scan` 的剪枝与承诺不对称…），逐条都对着树核过，**共 11 条**——第 11 条是接线作业
  `security-master` 在 PR #356 里第一次实跑才暴露的：`owasp-smoke` 那一行的 `requires` 不写
  `redis-cli`/`node`、`deploy-template` 的不写 pyyaml，于是缺工具的宿主得到的是一次**原因落在
  `tail -25` 窗口之外**的红，而不是门承诺的「NOT ASKED 并说明为什么」。依赖 T1209。
- **T1212**（`v1_required=false`，`tests/e2e/**`、`internal/persistence/testdb/**`，G3 `rsg-real-services`）：
  即上面 B 条。无依赖。

**这三本收口之后**，空窗清单上剩下的活就是 **C**（两处 baseline 的"只许变短"守卫）与 **D**（PR 页
Discussion 区块那条缺口：要么立账，要么改规格把这一格删掉——不许两头都不做）。
