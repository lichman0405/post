# V1 最终验收与交付报告

> 报告位置：`tests/acceptance/v1-final-report.md`  
> 生成基准：`9b22339e6398b714de5cc810454437ee33e51913`  
> 生成日期：2026-09-23  
> 审计脚本：`tests/acceptance/v1-final-audit.sh`（`--emit-tables` 生成第 1.2 / 1.3 节那两张表、生成基准行与计数块）

**这份报告是某一棵树上的证书，审计脚本是它的快照校验器。** `bash tests/acceptance/v1-final-audit.sh`
只在**报告自己的基准**上有意义：报告钉住的每个数字（`v1_required` 清单、`tests.json` 分布、
blocking `not_run` 名单）都是在上面那个 SHA 上取的一次快照，HEAD 前进一次（合并、落账、任何一次提交）
它就与当前树不同步。**合并或账本变动之后，必须先重新生成报告**（用
`bash tests/acceptance/v1-final-audit.sh --emit-tables` 的产物刷新基准行 + 两张表 + 计数块，
判定与证据由人按台账改、数字不许手写），再造新基准的证书；在那之前，在任何别的树上跑那条命令都会
以非 0 退出，并打印「证书过期」的说明与两条出路（第 4 节第 2 条）。

## 0. 结论（放在最前）

**V1 的两条台账条件在本基准 commit 上已经满足：`v1_required=true` 的任务（排除 T1207 自身）全部 `merged`；blocking 测试里唯一一条 `not_run` 就是这份报告自己的审计条目。** 但这**不等于**「V1 可以宣告完成」——下一条说了为什么。

- `tasks/tasks.json` 中 `v1_required=true` 的任务共 **149** 笔。排除 T1207 自身后，未 merged 的有 **0 笔**（脚本会打印 `EXCLUDED=T1207`，让人看得出那是一次具名排除而不是悄悄减一）。
- `tasks/tests.json` 里 blocking 条目共 **177** 条：**177 条 = 176 `passed` + 1 `not_run`**，没有 `failed`、没有 `skipped`、没有非 blocking 条目。唯一那条 `not_run` 是 `T1207-TEST-01`（本报告自己的审计，按 `scripts/record_test_run.py` 的规矩由 Supervisor 在 G2 落账；本报告不修改任何 `status`）。
- `docs/31_MASTER_ACCEPTANCE.md` 的 10 条 Gate 里，**8 条判「通过」，2 条判「未通过」：Gate G 与 Gate H**。
  - **Gate G 未通过**：第 1 条「MCP semantic tools 完整且权限正确」被树反证——`cmd/mcp-server` 的 `/mcp` 返回 501，`specs/mcp/tools.json` 声明 21 条工具而全树 0 个 dispatch 点，而 `docs/02_V1_SCOPE.md:52` 把它列进 V1 必做、§4 没有豁免它。这是 **L3-③**（`tasks/decisions.md` ㊱），等待 owner 裁定，见 R12。
  - **Gate H 未通过**：唯一理由是 SAST / 容器扫描 / SBOM 三项**覆盖缺席**（R10）。
- 未闭合的缺口在剩余风险里逐条点名，共 **13 条**（R1–R13）。其中 4 条是**产品**侧（R3 blob 写入路径、R4 reopen 无入口、R5 search 无模型侧、R7 search_records 503），1 条是**工具**侧（R11 `rddev worker collect` 的证据基线），1 条是**仪器**侧（R13 契约对账仪没有跑它的人），其余是证据成色与 L3 待裁定项。
- **四条 L3 至今未获裁定**，其中三条直接挂在 Gate 判定或缺口处置上：L3-⓪（内容能不能出平台 → Gate E 第一条只按结构化答案判过，R8）、L3-①（登录前能不能搜索 → R9）、L3-②（V1 含不含 blob 上传入口 → R3 的处置）、L3-③（V1 含不含 MCP 工具面 → Gate G 未通过，R12）。

以下每一节都给出真实跑过的命令及其落账位置，未使用「见某次聊天」或「凭记忆」。

---

## 1. 任务台账核对

### 1.1 核对方法

```bash
# 复跑命令
bash tests/acceptance/v1-final-audit.sh
```

该脚本读取：
- `tasks/tasks.json` 中的 `v1_required=true`；
- `tasks/task_status.json` 中的 `status`；
- 排除 T1207 自身（脚本会打印它排除了谁）。

### 1.2 未 merged 的 v1_required 任务（排除 T1207 后）

下表与第 1.3 节的表、计数块，都是 `bash tests/acceptance/v1-final-audit.sh --emit-tables`
**同一次输出**的内容（该命令一次打印：生成注释行、生成基准行、未合并任务表、blocking `not_run` 表、
计数块；本报告把同一次输出按两节的位置逐字贴入，未改一个字符）：

<!-- 生成命令：bash tests/acceptance/v1-final-audit.sh --emit-tables -->

> 生成基准：`9b22339e6398b714de5cc810454437ee33e51913`

| 任务 | 状态 | 阶段 | 标题 |
|------|------|------|------|
| （无） | — | — | v1_required=true 且非 merged 的任务 0 笔（已排除 T1207 自身） |

**计数：0 / 0（目标为 0，排除 T1207 后实际为 0）**。脚本输出会显式打印 `EXCLUDED=T1207`。

> 台账说明：上一版报告（基准 `5b0d7d82`）此表为空且已附上上一轮的对比。T1207 自己**落笔时**的状态是 `rejected`（这份报告正在返工轮里），脚本按定义把它排除并在 `EXCLUDED=` 里具名打印。**排除不是"减一"的同义词**：脚本打印它排除了谁，第 4 节的 4a 还逐条比对名单本身。

### 1.3 blocking 测试状态

```bash
# 复跑命令
jq '.tests | map(select(.blocking == true and .status == "not_run")) | {count: length, ids: map(.id), names: map(.name)}' tasks/tests.json
```

当前结果：**1 条 blocking 测试为 `not_run`**，不是 `passed`。清单（同一次 `--emit-tables` 输出的后半段）：

| 测试 ID | 任务 | 名称 |
|---------|------|------|
| T1207-TEST-01 | T1207 | final audit |

<!-- 计数块：报告引用的每个数字都由脚本给出，不许手写 -->
```text
COUNTS v1_required_total=149
COUNTS v1_required_unmerged=0
COUNTS excluded=T1207
COUNTS tests_total=177
COUNTS tests_passed=176
COUNTS tests_not_run=1
COUNTS tests_failed=0
COUNTS tests_skipped=0
COUNTS tests_blocking=177
```

> 账本规则：没有真实命令的 `passed` 不算证据。本报告未修改 `tasks/tests.json` 的任何 `status`。唯一这条 `not_run` 是**本报告自己的审计脚本**：它是这份交付物的 G2 门，由 Supervisor 按账本规矩落账；报告作者（T1207 自己）不能给自己的门记 `passed`。

本基准上 `tasks/tests.json` 的**整体分布**（复跑命令与结果如下，均在 `9b22339e` 实跑）：

```bash
jq -c '[.tests[]] | group_by(.status) | map({status: .[0].status, count: length})' tasks/tests.json
# 输出：[{"status":"not_run","count":1},{"status":"passed","count":176}]
jq '[.tests[] | select(.blocking==true)] | length' tasks/tests.json
# 输出：177（即全部条目都是 blocking=true）
jq '[.tests[] | select(.status=="failed")] | length' tasks/tests.json
# 输出：0
jq '[.tests[] | select(.status=="skipped")] | length' tasks/tests.json
# 输出：0
jq '[.tests[] | select(.status=="passed") | select(((.evidence // "") | length) == 0)] | length' tasks/tests.json
# 输出：0（没有一条 passed 是空证据）
```

也就是：**177 条 = 176 `passed` + 1 `not_run`**，没有 `failed`、没有 `skipped`、没有非 blocking 条目。上表那 1 条就是这 1 条 `not_run` 的全部。T1206-TEST-01 不在其中——它已是 `passed`（Supervisor 于 2026-09-22T02:36:21Z 以 `bash tests/security/master-security-gate.sh --report /tmp/t1206-master-gate-report.json` 记录，9 checks、exit 0，见 Gate H）。

---

## 2. Master Acceptance Gate 判定表

`docs/31_MASTER_ACCEPTANCE.md` 的每一条 checkbox 给一行：**判定 + 证据 + 落账位置**。判定只取
**通过 / 未通过 / 无证据** 三个值之一（「无证据」也是一种判定，不用「部分覆盖」「基本可用」和稀泥）。
表中列出的测试，凡本轮读过其**测试体**的，都在证据列里给出文件与行号——名字相近不算证据；
只引 `tasks/tests.json` 条目的，写的是那条 `evidence` 记的命令，并标明它是回填的还是复跑。

### Gate A — Domain Integrity

`docs/31_MASTER_ACCEPTANCE.md:5-12`，七条 checkbox：

| # | checkbox（docs/31 原文） | 判定 | 证据（命令 / 树内位置） | 落账位置 |
|---|--------------------------|------|--------------------------|----------|
| A1 | RSG 可导出/重建并有稳定 hash | 通过 | `go test ./internal/rsg/manifest/ -count=1 -run 'TestGolden\|TestManifestContentMirrors' -v`；测试体 `internal/rsg/manifest/golden_test.go:55 TestGoldenManifest`、`internal/rsg/manifest/manifest_test.go:342 TestManifestContentMirrorsDocument` | `tasks/tests.json#T0206-TEST-01` `passed` |
| A2 | Object/relation 版本不可改写历史 | 通过 | 触发器层：`infra/migrations/00014_append_only_enforcement.sql`（`BEFORE UPDATE OR DELETE` 拒绝改写历史）；测试体 `tests/integration/append_only_test.go:420 TestAppendOnlyEnforcement` | `tasks/tests.json#T0013-TEST-01` `passed`（Supervisor 在真 PostgreSQL 上复跑过自己的探针：UPDATE/DELETE 被 P0001 拒、合法 INSERT 仍成功、TRUNCATE 另测） |
| A3 | frozen main 无 direct semantic/Git write | 通过 | 语义侧：`tests/integration/freeze_main_e2e_test.go:357 TestFreezeMainGovernanceEndToEnd`——(1) 冻结后的 main 直接写 = **403 `MAIN_FROZEN_DIRECT_WRITE`**、被拒的写没有移动 main 的头版本；(4) 未冻结项目的直接写**不被拦**（反面对照）；(5b) agent 被两道防线独立拒绝。Git 侧：`bash tests/acceptance/gitea-real-services-e2e.sh`（真 Gitea） | `tasks/tests.json#T0601-TEST-01`、`#T0302-TEST-01`，均 `passed` |
| A4 | Abort/Reopen 保留完整历史 | 通过 | abort：`#T0602-TEST-01`（abort 走 PR、留记录）；reopen：`go test -count=1 -v -run 'TestReopen' ./tests/integration/` | `#T0602-TEST-01`、`#T0610-TEST-01`，均 `passed`。**reopen 没有对外路由**（R4），这条只覆盖领域状态机那一层 |
| A5 | Provenance 与 Evidence 关系分离 | 通过 | 分离被一条真测试钉住：`tests/integration/provenance_test.go:404 TestProvenanceGraphJSON` 断言 provenance 图恰好只有 `uses/performed_on/produces/derived_from/follows_protocol` 五种边，并逐字要求 `related_to`、`supports` **绝不进** provenance 图（"edge type %s must never enter the provenance graph"）；evidence 侧是另一套读（`tests/integration/evidence_graph_test.go:345`） | `#T0505-TEST-01`、`#T0506-TEST-01`，均 `passed`（回填自各自 Worker 记录；`evidence` 里写着不是新一轮复跑） |
| A6 | Claim 可表示支持、反驳、consistent、reproduce 等 | 通过 | `internal/domain/evidence_assertion_test.go:40 TestRelationStance`：九条 canonical relation 映射到三个 stance 桶（`supports/validates/reproduces/consistent_with` → supporting；`contradicts/challenges/inconsistent_with/fails_to_reproduce` → contesting；`contextualizes` → neutral），并要求桶外一律不映射（`"disproves"` 被拒）；`:74 TestRelationStanceIsALabelNotAScore` 钉住"是标签不是分数" | `#T0504-TEST-01`（`-run 'Evidence\|Stance\|Relation'`）、`#T0502-TEST-01`（claim unit），均 `passed` |
| A7 | Citation/Dependency 分离并影响分析只对 dependency 强传播 | 通过 | `tests/integration/dependency_impact_test.go:656 TestDependencyImpactGoldenClosure`：图里同时有 `depends_on` 与 `references`（引用）两种边，闭包按**手算的五笔**逐个比对，并逐字断言两条 `references` 边**必须不出现在输出里**（"a `references` edge is background knowledge, not an input dependency (docs/19 §3)"）；`docs/19_EXTERNAL_REFERENCES.md:11` §3 是它对得上的规格 | `#T1007-TEST-01` `passed`（Supervisor 于 2026-09-21 在真 PostgreSQL 上跑整包，本用例 `--- PASS: TestDependencyImpactGoldenClosure (0.89s)`） |

**处置**：7 / 7 通过。唯一带缺口的是 A4 的 reopen 那一层——**领域能力在、对外入口不在**，
见 R4（含一处今天不可达的真实失败回退缺陷）。

### Gate B — Collaboration

`docs/31_MASTER_ACCEPTANCE.md:14-18`，四条 checkbox：

| # | checkbox | 判定 | 证据（命令 / 树内位置） | 落账位置 |
|---|----------|------|--------------------------|----------|
| B1 | Branch/PR/RSG diff/Scientific Review/Integrity Review/merge 完整 | 通过 | `#T0401-TEST-01` golden diff、`#T0402` pr integration、`#T0403` integrity tests、`#T0404` review tests、`#T0406` merge integration、`#T0411` request review route | 六条 `passed` |
| B2 | Semantic conflict 不被 Agent 自动科学裁决 | 通过 | 「人类裁决、agent 只有建议权」被写进代码并被测试钉住：`internal/application/resolutions/doc.go:43-47` 逐字——"The agent explanation … is advisory-only: nothing in this package, the HTTP surface or the UI derives a decision from it. The only outcomes are the five human-chosen kinds; there is no computed, averaged or auto-selected kind."；`tests/e2e/conflict_e2e_test.go:447 TestE2EConflictResolution` 第 8 步断言审计行的 actor 是**人**（`audit actor = … want alice … (human-governed)`）；`internal/rsg/conflict/conflict.go:37` 把 rights/visibility 冲突标成"never auto" | `#T0405-TEST-01` conflict unit、`#T0407-TEST-01` conflict e2e，均 `passed` |
| B3 | Public Project 外部用户可 fork/contribute | 通过 | `#T0804-TEST-01`（`go test -run 'TestExternalFork' ./tests/integration/`）、`#T0814-TEST-01/02`（fork http e2e / fork propose authorization）、`#T0817-TEST-01`（external fork merge e2e）、`#T0818-TEST-01`（上游项目的 RSG 读到落地的内容） | 五条 `passed` |
| B4 | Private branch 不因 merge/publish 误公开完整历史 | 通过 | `#T0705-TEST-01` 的 `go test ./tests/integration -run 'TestAssetPublish\|TestKnowledgePublish'` 里含 `TestKnowledgePublishDoesNotWidenVisibility`（发布不扩大可见性）；`#T1107-TEST-01` privacy suite（`bash tests/acceptance/privacy-suite.sh`）；`#T0106-TEST-01` privacy negative e2e（`go test ./tests/e2e -run TestE2EPrivacyNegative`） | 三条 `passed` |

**处置**：4 / 4 通过。

### Gate C — Release/Asset

`docs/31_MASTER_ACCEPTANCE.md:20-24`，四条 checkbox：

| # | checkbox | 判定 | 证据（命令 / 树内位置） | 落账位置 |
|---|----------|------|--------------------------|----------|
| C1 | Release immutable、policy/schema/version/hash pinned | 通过 | `#T0605-TEST-01` release golden、`#T0606-TEST-01` release e2e、`#T0608-TEST-01` playwright release flow | 三条 `passed` |
| C2 | Dataset/Protocol/Material Collection/Benchmark 可发布 | 通过 | 四种资产类型就是产品的**全部**类型词表（`internal/assets/asset.go:28-37`，`ValidTypes` 逐个列出这四个），每种各有自己的 manifest schema（`internal/assets/manifest.go:143-163`，`TypeDataset`/`TypeProtocol`/`TypeMaterialCollection`/`TypeBenchmark`）；发布链路 e2e 见 `#T0710-TEST-01`（`go test ./tests/integration/ -run 'TestAssetCanonicalE2E$'`） | `#T0701-TEST-01` asset core、`#T0702-TEST-01` asset validators、`#T0710-TEST-01`，均 `passed` |
| C3 | Asset PID/version/lineage/rights/reference/dependency/fork 工作 | 通过 | 资产族 T0701–T0712 逐笔：`#T0701` core、`#T0702` validators、`#T0703` rights、`#T0706` metadata、`#T0707` dependency、`#T0708` lineage、`#T0709` ui e2e、`#T0710` canonical e2e、`#T0711` governance、`#T0712` preview disclosure | 十条 `passed`（`jq '[.tests[] \| select(.id\|test("^T07(0[1-9]\|1[0-2])-TEST-01$")) \| select(.status!="passed")] \| length'` → `0`） |
| C4 | private→public 有影响预览和显式确认 | 通过 | **预览**：`internal/assets/preview.go:13-19` 逐字记着 docs/23 §4 对 private→public 的要求（"a human explicit action, an impact preview, rights validation, no hidden private dependency leak, and an audit event"），预览是只读的那一半；路由 `POST …/assets:publish-preview` 与 `POST …/assets:publish` 是**两条**（`cmd/api/assetshttp/wiring.go:118-119`）。**显式确认落在「人类显式动作」上、不在某个 `confirm:true` 字段上**：`tests/integration/asset_publish_test.go:521 TestAssetPublishRefusesAnAgentToken` 断言 agent 的 publish 被 `ErrAgentNotPermitted` 拒（两道防线各自独立：领域 backstop + 权限矩阵的 agent 列），**并以同一条人类 session 的成功作为对照**；`:821 TestAssetPublishReRunsThePreviewOverCurrentState` 断言 publish 会在**当前状态**上重跑预览，且拒绝时的响应带**完整预览**（`cmd/api/assetshttp/publish.go:222-246`） | `#T0704-TEST-01`、`#T0712-TEST-01`（`go test ./internal/assets/ -count=1`）、`#T0705-TEST-01`，均 `passed` |

**处置**：4 / 4 通过。C4 的"显式确认"按上面那句读——**它的形状是"人类的一次独立写"**，
不是请求体里的一个确认位；这一条我读了测试体才敢这么写。资产侧另一处缺口（新 blob 没有产品创建入口）
不是 C 的失分项，见 R3。

### Gate D — Network

`docs/31_MASTER_ACCEPTANCE.md:26-30`，四条 checkbox：

| # | checkbox | 判定 | 证据（命令 / 树内位置） | 落账位置 |
|---|----------|------|--------------------------|----------|
| D1 | Public pages 未登录可访问 | 通过 | `bash tests/e2e-anonymous/run.sh`（起真 `next start` + Chromium）；`tests/e2e/explore_e2e_test.go:825 TestE2EExploreIsReadOnlyAndAnonymous`（无 cookie 读 `/api/v1/explore` = 200，写被拒） | `#T0801-TEST-01`、`#T0802-TEST-01`，均 `passed` |
| D2 | Explore/Search 可发现 public project/asset/knowledge/person/org | 通过 | 六个维度逐节被断言：`tests/e2e/explore_e2e_test.go:468 TestE2EExploreIndexIsTheOpenNetworksSixDimensions` 的 fixture 同时放 open 与 private 的 knowledge 行，并断言 private 那条被丢掉（`:587 TestE2EExploreNeverNamesAPrivateProject`）；检索侧 `#T0901-TEST-01` search projection、`#T0904-TEST-01` retrieval integration、`#T0905-TEST-01` ranking golden | 五条 `passed`。**匿名 `/search` 未放行**是另一条（L3-①，见 R9）；这一条的"可发现"由 Explore 那一半独立满足 |
| D3 | Published knowledge 可收到 external evidence，区分 origin/reviewed/unreviewed | 通过 | 分类就是 docs/31 Gate D 这一句的落点：`internal/domain/evidence_network.go:3-6` 逐字引用了这句，并把三类算在两个既有轴上（origin/external、reviewed/unreviewed），**不存第三个冗余列**；测试体 `internal/domain/evidence_network_test.go:14 TestClassifyEvidenceNetwork`、`:71 TestClassifyEvidenceNetworkIsTotal`、`tests/integration/evidence_network_test.go:324 TestExternalEvidenceNetworkIsClassifiedAndDoesNotLeak` | `#T0806-TEST-01` external evidence tests、`#T0511-TEST-01` evidence read audience e2e（两者都跑 `./tests/integration` 包，`evidence_network_test.go` 在其中）、`#T0810-TEST-01` network e2e（Supervisor 复跑整包），均 `passed` |
| D4 | Contribution 落到个人 Profile，组织 affiliation 不吞掉个人历史 | 通过 | `#T0102-TEST-01/02` profile api / profile e2e、`#T0807-TEST-01` contribution projection、`#T0808-TEST-01` research profile e2e（`go test ./tests/e2e/ -run 'TestE2EResearchProfile'`）、`#T0816-TEST-01/02/03`（affiliation 日期口径三处共用一个定义、UTC/本地日界落到"加入的组织"、整包在 `TZ=UTC` 与 `TZ=Asia/Shanghai` 下都绿） | 七条 `passed` |

**处置**：4 / 4 通过。

### Gate E — Search

`docs/31_MASTER_ACCEPTANCE.md:32-36`，四条 checkbox：

| # | checkbox | 判定 | 证据（命令 / 树内位置） | 落账位置 |
|---|----------|------|--------------------------|----------|
| E1 | Seed query 生成 Evidence-backed Answer | 通过 | 见下方**逐字判定** | `#T0906-TEST-01`（`go test -count=1 -v ./tests/answer/`）、`#T0901/T0904/T0905`，均 `passed` |
| E2 | Answer 引用确定版本实体；不得 hallucinate 不存在 source | 通过 | `#T0906-TEST-01` answer grounding tests：引用必须是本次检索选中的 ref（`internal/search/answer/grounding.go:176` 的注释说明 citations 与 selected refs 逐字比对），越界引用走 fallback；审查记录 `.rddev/workers/T0906-review/RESULT.json` 里记着审查人做过的 mutation 验证 | `#T0906-TEST-01` `passed`。**边界**：guard 拒不了无 `@`、无平台前缀的**裸编造名**——`internal/search/answer/grounding.go:172-177` 自己写明了这条边界，见 R6 |
| E3 | Search 无 private leakage | 通过 | `tests/integration/search_projection_test.go:507 TestSearchProjectionKeepsPrivateContentPrivate`、`tests/integration/search_access_test.go:23 TestSearchDocumentsEnforcesAccessControl`、`tests/integration/search_scope_test.go:40 TestSearchScopeIsResolvedFromMembershipAndEnforcedByTheQuery`（三条都在 `./tests/integration` 里）；端到端负例 `#T0106-TEST-01`（`go test ./tests/e2e -run TestE2EPrivacyNegative`：公开项目 + 私密项目，非成员读私密项目被拒） | `#T0106-TEST-01`、`#T1107-TEST-01`、`#T0810-TEST-01`（整包复跑）均 `passed` |
| E4 | Answer → Draft Research Context → new Project 流程可用 | 通过 | `tests/integration/search_start_project_test.go:270 TestStartResearchProjectE2E`：一次**真实检索**（真 PostgreSQL、真 route）→ `POST /api/v1/search/{id}:start-project` → 201 且 draft 状态 `draft`、**不写任何科研状态**（四张状态表逐张断言 0 行）→ `confirm` 落到 state commit；幂等与权限负例在同一个文件里（`:541 TestStartProjectRefusesAnUngroundedRefE2E` 拒绝没有 grounding 的 ref） | `#T0908-TEST-01` `passed` |

**E1 的逐字判定**：按 `tasks/decisions.md` **L3-⓪**（2026-09-19 补记）的处置要求，这一条写成两句话：
**由证据背书的「结构化」答案满足；模型撰写的综述不在 V1 范围内，因为它卡在一条未获裁定的 L3（外部凭证/付费服务）上。**
这两句指到 L3-⓪ 本身：它逐字记着「平台上的内容——项目名、知识对象正文、用户敲进搜索框的那句话——可不可以发给平台之外的模型服务」在 `docs/**` 与 `specs/**` 里**一处都没写**，而且真接一个 provider 就必然命中 §5.1 的停止条件。

**处置**：4 / 4 通过，但 E1 的通过是**在"结构化答案"这个口径下**的通过——把它读成"平台能用模型写综述"就是失真。**不用第三条读法**：本报告不自行决定那条 L3。其余三条 search 缺口（无 planner/embedder/answer provider、grounding 拒不了裸名、`search_records` 写失败转 503）见 R5/R6/R7。

### Gate F — Web UX

`docs/31_MASTER_ACCEPTANCE.md:38-43`，五条 checkbox：

| # | checkbox | 判定 | 证据（命令 / 树内位置） | 落账位置 |
|---|----------|------|--------------------------|----------|
| F1 | Project 首页是 Summary/Research，不是文件树 | 通过 | `#T0211-TEST-01` research page e2e（`go test ./tests/integration -run 'TestResearchPageE2E'`）、`#T0212-TEST-01` overview e2e | 两条 `passed` |
| F2 | Files 只读，无 edit/upload/delete | 通过 | `bash tests/e2e-files/run.sh`；`tests/e2e-files/files-e2e.mjs:321-346` 断言只读 DOM 里没有 mutation-capable 元素、且页面发出的每个 files API 调用都是 GET | `#T0308-TEST-01`、`#T0307-TEST-01`，均 `passed` |
| F3 | GitHub/Primer 中性高密度设计，无渐变紫/玻璃拟态 | 通过 | `tests/web-smoke/visual-regression.mjs:129,142,144`：对签入的基线断言 `FORBIDDEN_FX`（linear/radial/conic-gradient、repeating-\*、backdrop-filter、blur()）、`FORBIDDEN_TOKEN_NAMES = ["brandGradient","purpleGlow","glassBackground"]`（依据 `docs/41:14`）与 `FORBIDDEN_HEX` | `#T1101-TEST-01` visual regression、`#T0107-TEST-01` visual smoke，均 `passed` |
| F4 | PR 默认 Research State Diff，raw Git diff 二级 | 通过 | `tests/e2e-pulls/pulls-e2e.mjs:487` 起的「默认首屏非 raw diff」断言（`:527` 打印 `first screen: the raw file view is NOT in the DOM (默认首屏非 raw diff)`）；`#T0410-TEST-01` playwright pr flows | `#T0408-TEST-01`、`#T0410-TEST-01`，均 `passed` |
| F5 | Research Map 可访问并可 drill-down | 通过 | `#T1102-TEST-01` research map e2e/perf | `passed` |

**处置**：5 / 5 通过。**这一节的判定与上一版一致，是上一轮复核后保留的改判**
（上一版曾判「未通过」，理由是五笔任务未 merged；五笔全部 merged、所列测试全部 `passed` 之后按台账事实改对，
`tasks/decisions.md` ㊲ 记录了这次复核结论）。改判**没有放宽任何断言**：`#T1101-TEST-01`/`#T1103-TEST-01`
等通过证据本身是回填的 Worker 记录（不是独立复跑），这条性质记入 R1。

### Gate G — Agent/API/Git

`docs/31_MASTER_ACCEPTANCE.md:45-49`，四条 checkbox：

| # | checkbox | 判定 | 证据（命令 / 树内位置） | 落账位置 |
|---|----------|------|--------------------------|----------|
| G1 | MCP semantic tools 完整且权限正确 | **未通过** | 见下方三条证据，树**反证**了这一条 | 无——今天没有任何一条记录能说它通过 |
| G2 | Agent 无 merge/publish visibility 扩大权限 | 通过（**真空成立**） | 目录侧禁止：`specs/mcp/tools.json` 的 `forbidden_default_agent_actions` 逐字是 `["merge_main","publish_private_to_public","change_rights_holder","delete_history","force_push_main"]`，四条治理类工具（`release.prepare`、`asset.publish_preview`、`knowledge.publish_preview`、`object.abort_proposal`）的 `mode` 是 `proposal` 而不是 `write`；**人类**路径这一侧有真测试：`#T0105-TEST-01` 权限矩阵、`#T0409-TEST-01` merge governance e2e、`#T0705-TEST-01` publish security e2e（其中 `TestAssetPublishRefusesAnAgentToken` 断言 agent 被两道防线各自独立拒绝） | 三条 `passed`（人类路径）。**agent 路径今天不存在**：0 个 dispatch 点、`/mcp` 501 |
| G3 | Git branch push ingestion；main direct push 拒绝 | 通过 | `#T0302-TEST-01` git protection e2e、`#T0305-TEST-01` git ingestion integration、`#T0306-TEST-01` unstructured negative | 三条 `passed` |
| G4 | Git/RSG drift reconciliation 可检测 | 通过 | `#T0301-TEST-01` gitea integration、`#T0309-TEST-01` reconciliation integration | 两条 `passed` |

**G1 的三条证据（本基准实跑）**：

```bash
# 1. /mcp 是一个 501 存根，不是没接线的半成品——它按设计就答 501
sed -n '77,80p' cmd/mcp-server/main.go
#   mux.HandleFunc("/mcp", func(w http.ResponseWriter, _ *http.Request) {
#     w.Header().Set("Content-Type", "application/json")
#     w.WriteHeader(http.StatusNotImplemented)
#     fmt.Fprintf(w, `{"error":"MCP protocol wiring not implemented yet (agent tasks)"}`)

# 2. 目录声明 21 条工具，全树 0 个 dispatch 点（重建 T1205 的那份对账）
go build -o /tmp/t1207-cgate ./tests/cmd/contractgate
/tmp/t1207-cgate -root . -write-mcp /tmp/t1207-mcp-inventory.json | head -2
# 输出：MCP reconciliation: catalog specs/mcp/tools.json declares 21 tool(s); 0 have a dispatch site; 21 do not.
jq '{declared_tools, tools_with_a_dispatch_site, tools_the_catalog_declares_and_the_tree_does_not_dispatch}' /tmp/t1207-mcp-inventory.json
# 输出：{"declared_tools":21,"tools_with_a_dispatch_site":0,"tools_the_catalog_declares_and_the_tree_does_not_dispatch":21}

# 3. 而规格把这一面列进 V1 必做、§4 没有豁免它
sed -n '51,53p;60p' docs/02_V1_SCOPE.md
#   ### Agent interface
#   - MCP/API 读写科研状态。
#   - Governance 操作（merge/publish/visibility）限制为 Web 或显式 approval path。
#   ## 4. V1 明确不做
sed -n '15p' docs/56_REQUIREMENTS_TRACEABILITY.md
#   | MCP/Agent | 15/47 | T1205 + 各 semantic API task | G |
```

重建出的对账与已合并的 `ops/contract/mcp-inventory.json` 结论一致（`declared_tools=21`、
`tools_with_a_dispatch_site=0`），只有两处行号漂了（提交版记 `cmd/api/rsghttp/page.go:408`/`:401`，
当前树那两个字面量在 `:419`/`:412`）——**漂的是"哪里提到了这个名字"，不是"有没有接线"**。

**G2 的性质要照实写：这是真空成立，不是"已验证"。** 目录**禁止**了那五个动作，而今天**根本没有 agent 写入路径**
（0 个 dispatch 点、`/mcp` 501），所以没有任何一条 agent 路径可以用来验证"它没有越权"。
真空就是真空。行为级验证要等工具面真接线，与 G1 同挂 **L3-③**。

**处置**：**未通过，失分项是 G1**。这不是本报告能定的，也不是 Supervisor 能定的：
`tasks/decisions.md` **㊱** 已把它立为待 owner 裁定的 **L3-③**——两条路（把 21 条工具建起来，或把
`specs/mcp/tools.json` 与 `docs/02:52`、`docs/56:15` 一起降级）都要先有产品语义（每条工具的参数与返回、
`proposal` 模式在操作上意味着什么、`docs/02:53` 那句「显式 approval path」是什么形状），规格一处都没写。
**本报告引用 ㊱，不替它下结论。** 见 R12。

### Gate H — Security/Quality

`docs/31_MASTER_ACCEPTANCE.md:51-57`，六条 checkbox：

| # | checkbox | 判定 | 证据（命令 / 树内位置） | 落账位置 |
|---|----------|------|--------------------------|----------|
| H1 | Critical/High security issues = 0 | 通过 | `bash tests/security/master-security-gate.sh --only vuln-go,vuln-node,vuln-python` → Go `No vulnerabilities found.`（govulncheck 结论行；另有 3 条"依赖模块里存在、本代码不调用"的告警，工具自己分列）、Node `No known vulnerabilities found`、Python `Found no known vulnerabilities and no adverse project statuses in 6 packages`；`--only secret-scan` → `--- PASS: TestRepoExampleFilesAreSecretFree` 与 `TestScanRepoExampleFilesFailsOnPlantedFile` | 本报告实测（三条 exit 0）；`#T1206-TEST-01` master suite 由 Supervisor 于 2026-09-22T02:36:21Z 记 `passed` |
| H2 | permission negative E2E 全过 | 通过 | `#T0106-TEST-01` privacy negative e2e（非成员读私密项目被拒）、`#T0105-TEST-01` 权限矩阵、`#T0705-TEST-01` publish security e2e | 三条 `passed` |
| H3 | backup/restore tested | 通过 | `#T1110-TEST-01` restore drill（`./ops/backup-restore-drill.sh --no-infra` → `TestRestoreDrill`）、`#T1111-TEST-01` restore drill e2e、`#T1204-TEST-01` runbook drill | 三条 `passed` |
| H4 | local one-command dev | 通过 | 机制是 `make dev`（`Makefile:324`，从 `.env.dev` 一条命令起五个应用、Ctrl-C 全停）；环境前置与可执行性由 `#T0000-TEST-01/02`、`#T1204-TEST-01` 落账 | 三条 `passed`。**本报告本轮没有亲自跑 `make dev`**——它起的是整套本地栈，这一条的证据是那三条记录 |
| H5 | CI blocking suites 全绿 | 通过 | 必需作业集合的权威是两个文件本身：`.github/workflows/ci.yml` 与 `specs/orchestrator/gates.json`。本报告实测两边逐字对得上（脚本见第 4 节第 5b 条）：`10 ['a11y','acceptance','go','i18n','migration-integration','observability','python','spec-validation','task-state','web']` 与 `True`（四处的集合相等）。台账侧：blocking 只剩 1 条 `not_run`（本报告自己的审计） | 本报告实测。**本报告没有在本基准上触发 CI**：两个最新必需作业的运行记录是 `#T1104-TEST-01` 的 run 35743090655 与 `#T1105-TEST-01` 的 run 35766550262，且每个必需任务合并前的 G4 以"所有必需作业绿"为断言 |
| H6 | accessibility AA core pages | 通过 | `make a11y`（CI 必需作业 `a11y`）：axe-core WCAG pass + best-practice pass 双双 0 违规，7 个核心页面上有数据 | `#T1104-TEST-01` `passed` |

**处置**：**未通过——H1 与 H5/H6 都成立，但"Critical/High = 0"这个结论只覆盖到已经装了扫描器的那些面**。
未通过的唯一理由是 **SAST / 容器扫描 / SBOM 三项覆盖缺席**，见 R10：

```bash
bash tests/security/master-security-gate.sh --only absence-manifest
# 输出：ok item: sast / ok item: container-scan / ok item: sbom，并打印 witness 命令证明缺口仍在
```

dependency/CVE 扫描（Go/Node/Python 三面）已由 T1206 补齐并实测干净，**不在这份缺席清单里**。
本报告**不自行放宽**：若 Supervisor 的裁定是「V1 完成不以这三项为条件」，Gate H 应随之改判为通过；
在没有那份裁定之前，报告照实写「未通过」。

### Gate I — Canonical Workflow

`docs/31_MASTER_ACCEPTANCE.md:59-60`，一句全文：

> 完整 MOF workflow 从 Research Question 到外部项目贡献 Evidence 与 Profile credit，全程真实 DB/Git/blob/browser，非 mock 演示。

| # | 判定 | 证据 | 落账位置 |
|---|------|------|----------|
| I1 | 通过 | `.rddev/workers/T1202/RESULT.json` 里的三条运行：①`bash tests/acceptance/mof-canonical-workflow.sh` → `G3 mof-canonical: PASSED - 50 step(s), every assertion green`（步骤号 [01]..[50] 无缺口）；②`node tests/e2e-pr-flows/mof-canonical-e2e.mjs` → 21 步真实 Chromium（playwright 1.55.0）；③`bash tests/acceptance/mof-canonical-mutation-check.sh` → `MUTATION CHECK - both mutations were caught. The gate can say no.` 关键断言：外部证据可通过 pid 取回；外部贡献者 `demo-external` 获得 2 条 contribution；affiliation 与个人历史并列显示 | `tasks/tests.json#T1202-TEST-01` `passed`（`evidence` 逐字记着「回填自 T1202 自己的 Worker 记录，不是一轮新的复跑」）。**本轮没有重跑这一条**：它需要整套本地服务栈，复跑命令见第 4 节第 4 条 |

**处置**：通过（证据来自 T1202 的运行记录，账本条目为回填；这条证据性质记入 R2）。

### Development System Gate

`docs/31_MASTER_ACCEPTANCE.md:62-72`，七条：

| # | checkbox | 判定 | 证据 | 落账位置 |
|---|----------|------|------|----------|
| S1 | Ubuntu 24.04 canonical preflight 可复现 | 通过 | `#T0000-TEST-01` environment preflight unit、`#T0000-TEST-02` ubuntu smoke | 两条 `passed` |
| S2 | `rddev` 可恢复 task/worker/worktree 状态 | 通过 | `#T0009-TEST-02` task state integration、`#T0010-TEST-01/02/03` worktree 调度与并发/崩溃恢复 | 四条 `passed` |
| S3 | 至少两个独立 Worker 可并行完成互不冲突的示例任务 | 通过 | `#T0010-TEST-02` two-worker concurrency smoke | `passed` |
| S4 | Worker 无 Git remote credentials，commit/push/branch mutation 尝试被拒绝或验收强制拒绝 | 通过 | `#T0011-TEST-01` git control denial e2e、`#T0012-TEST-03` supervisor git control e2e | 两条 `passed` |
| S5 | Worker 越界 diff 被拒绝 | 通过 | `#T0011-TEST-02` scope violation e2e | `passed` |
| S6 | Supervisor 可独立执行 G2/G3/G4 并只在全绿后 commit/PR/merge | 通过 | `#T0012-TEST-01` four-gate acceptance e2e、`#T0012-TEST-02` rejection/retry e2e | 两条 `passed` |
| S7 | Supervisor 会话中断后可从仓库状态恢复，不依赖旧聊天上下文 | 通过 | 状态外部化在 `tasks/tasks.json`、`tasks/task_status.json`、`tasks/tests.json`、`tasks/progress.md`、`tasks/decisions.md`（本报告本身就是一次"断开后从仓库状态重建判断"的实例：它引用的每一处事实都指得到文件与行） | 台账文件本身（`scripts/record_test_run.py` / `scripts/reconcile_tests_ledger.py` 是唯一写 `status` 的入口） |

**处置**：7 / 7 通过。

---

## 3. 剩余风险（逐条点名）

本节与第 2 节对应：**两条「未通过」的 Gate 各有一条直接理由**（Gate G ← R12；Gate H ← R10），
其余「通过」的 Gate 也各自带缺口，以风险形式保留，一条不落地点名。**没有「基本通过」类措辞**：
每条都写清事实、影响、V1 后怎么补。

### R1. P11 五笔任务的通过证据是回填的 Worker 记录 / CI 运行，不是独立复跑

Gate F 的每一条都能指到一条 blocking 测试记录，**但要把证据的性质写清楚**：

| 测试 | 通过依据 | 来源 |
|------|----------|------|
| T1101-TEST-01 visual regression | 回填自 T1101 的 Worker 记录（不是 Supervisor 复跑） | `tasks/tests.json` 的 `evidence` |
| T1103-TEST-01 publish ux e2e | 回填自 T1103 的 Worker 记录 | 同上 |
| T1104-TEST-01 a11y suite | Supervisor 记录的 `make a11y` 运行（CI 必需作业 `a11y` 在 PR #351 的 run 35743090655） | 同上 |
| T1105-TEST-01 i18n e2e | Supervisor 记录的 `make i18n` 运行（run 35766550262 / job 106877425211） | 同上 |

**影响**：Gate F 通过所依赖的四个 `passed` 里，两个是回填的 Worker 记录，两个是 CI 运行记录；
**本报告没有在最终树上独立复跑它们**（它们需要 `next build` + 真实 Chromium + 服务栈）。

**V1 后补法**：由 Supervisor 在合并后的树上按上表各复跑一次，让账本里留下带自己命令串的证据
（账本规矩是「一次复跑写它自己的 evidence 串」）。

### R2. V1 的完成声明依赖一份被回填过的账本

- 事实（本报告实测）：`tasks/tests.json` 共 177 条，其中 **140 条**的 `evidence` 写明是回填的
  （**127 条**逐字写着 `not a fresh re-run`）。唯一剩下的 `not_run` 是 `T1207-TEST-01`。
- 影响：**账上 176 条 `passed` 不等于 176 次独立复跑**。V1 的完成声明因此建立在「Worker/CI 运行记录 +
  各任务的 G2/G4 验收」之上，而不是建立在本报告作者的一次性全量复跑之上。这是事实层面的限制，
  不是判定的瑕疵；写在这里是为了让读报告的人知道证据的成色。

**V1 后补法**：需要独立复跑的门，由 Supervisor 按任务包的 G2/G3 流程各自跑一次并落账；本报告不复跑（无服务栈、也无权改台账）。

### R3. 产品的 blob 写入路径缺失

- 出处：`tasks/decisions.md` ㉘，处置已被 **㉟ 升级为待裁定的 L3-②**。
- 证据（以下命令均在本基准实跑，输出见行内）：
  - 契约**声明**了这两条端点；`grep -n "request-upload" specs/api/openapi.yaml` 命中
    `234:  /projects/{projectId}/branches/{branchId}/blobs:request-upload:`，同文件 `:242` 声明
    `…/{blobId}:finalize`。这是契约声明，不是实现。
  - 产品代码里没有实现：`grep -Rn "request-upload\|requestUpload" cmd/api internal` 输出为空。
  - `CreateBlob` 真正的调用点只有 `tests/integration/manifest_test.go:178`；`internal/persistence/sqlc/blobs.sql.go:42`
    是 sqlc 从 `internal/persistence/queries/blobs.sql:6` 生成的定义，`internal/persistence/sqlc/querier.go:121` 是生成的接口声明。
  - 把口径放宽到任何 `INSERT INTO blobs`：`grep -Rli "INSERT INTO blobs" --include='*.go' --include='*.sql' .`
    命中 17 处——`internal/persistence/queries/blobs.sql`（sqlc 源查询）、`internal/persistence/sqlc/blobs.sql.go`（生成物）、
    `tests/acceptance/seeddemo/blobs.go`（验收 seed 工具）、`tests/acceptance/portability/export.go`（注释）、
    `tests/e2e-release/harness/main.go:859`（**release e2e 的测试装置**）、以及 `tests/integration/*_test.go` 的 12 个文件。
    **产品代码（`cmd/**`、`internal/**`）里一条调用都没有**；上面清单里的"产品代码"只有 sqlc 的源查询与生成物——
    它们是被生成的存取层，不是任何一处**发起**写入的入口。
  - 生产侧唯一的 `PutObject` 在灾备包里：`cmd/api/backupdr/source.go:79`、`drill.go:603`（调用），
    `drill.go:129`、`s3.go:221`（注释），`s3.go:222`（定义）。**没有一行在产品把内容放进 blob truth 的路上。**

**影响**：资产/发布链路今天只在**已存在的 blob 行**上被验证过；**新 blob 的创建没有产品入口**。
因此「可移植导出」的结论只能声明它导出的是已有内容。

**V1 后补法**：**没有任务承接它，而且它不该由工程侧自行收口**——`tasks/decisions.md` **㉟** 已把它立为
**L3-②**：`docs/15_AGENT_MCP_API.md:23-25`、`docs/17_FILES_STORAGE.md:17-19`、`docs/23_SECURITY_PRIVACY.md:25`、
`docs/27_PERFORMANCE_SLO.md:22-23` 把上传写成一套有协议、有安全要求、有 V1 量级目标的产品能力，
删掉契约那两行只是把不一致从 `specs/` 挪进 `docs/**`。**裁定"包含"→ 立账一笔覆盖
`internal/storage` + `cmd/api` + 迁移的任务，并同时裁定 TTL/GC 语义；裁定"不包含"→ 契约与
`docs/15/17/23/27` 一起对齐。** 在裁定之前，这条的状态是「已记账的缺口 + 一条未获裁定的 L3」，
**不是待补的工程活**。

### R4. reopen 在生产里没有入口，并带有一处真实行为缺陷

- 出处：`tasks/decisions.md` ⑲ 及 **2026-09-21 更正**（`tasks/decisions.md:16971`）。
- 证据（均在本基准复跑，命中行如下）：
  - `internal/authz/action.go:39-40` 注释把前置状态写成 lifecycle `'reopened'`（"reopens a main-branch object that is in lifecycle 'reopened'"），
    而代码要求的是 `aborted`：`internal/application/reopens/service.go:303` 明写
    `if named.LifecycleState != domain.LifecycleAborted` 即拒。注释说的是**结果态**，读起来却像**前置态**。
  - **步骤序**：`internal/application/reopens/service.go` 的步骤表把 `object` 列进第 5 步（`:208`），
    而代码在幂等读（`:246` `GetVersionByReopenRequestKey`）**之前**就 `GetObject`（`:234`）——
    所以注释里那句「Step 4 precedes step 5」（`:213`）只对 version/lifecycle 成立，**对 object 不成立**。
  - `requireAgent` 调用在 `:224`、`requireReopen` 在 `:227`（**backstop 在前**），
    `requireAgent` 的文档注释（`:380-383`）自己写着 'it is deliberately the FIRST of the two lines'。
    **此处注释与代码一致，没有缺陷**（上一轮这里曾写反，本版已更正）。
  - **行为缺陷**：`openProposal`（调用在 `:352`）因**非** `ErrIdempotencyKeyInUse` 失败时（该判断在 `:368`）
    会落进 `replayIfRecorded`（`:371`），读回**本次请求自己刚追加的那一版**（追加在 `:341` 的 `appendReopenVersion`，已成功），
    返回 `Replayed=true`（`resultFromRecorded`，`:918`）而 `PullRequestNumber=0`。**这条回退是有意的**——
    `replay` 的注释在 `:720-723` 逐字写着 'The first attempt committed the version and died before opening the
    proposal (the gap documented in this task's result). The replay answers with what is recorded rather than
    opening a PR on a branch the caller can no longer see the head of.'（`:748` 附近是 `replayIfRecorded` 的定义，
    不是这段注释——上一轮引错了行号，已更正）——但后果仍然成立：**一次真实的「提案没开成」被答成 201 的重放**，
    调用方拿到的是没有 PR 的成功。

**影响**：该缺陷今天**不可达**（没有 reopen 路由），所以不是当前可触发的 bug；
但只要将来补上 reopen 路由，这条缺陷会立刻暴露。

**谁承接：今天没有任何任务承接这一条。** `tasks/decisions.md:16971` 逐字记着「**A/F/G 至今没有承接者**」，
并更正了那句「T0909 里已逐条写明」。我把这个全称断言自己复跑了一遍：

```bash
# (a) 字面写 reopens 的 allowed_scope：0 / 150 笔任务
jq -r '.tasks[] | select(((.allowed_scope // []) | map(test("reopens")) | any)) | .id' tasks/tasks.json | wc -l
# 输出：0
# (b) 未 merged 的任务里，scope 能碰到 internal/application 的：0 笔
jq -s -r '.[0].tasks as $d | .[1].tasks as $s | $d[] | .id as $id | select(((.allowed_scope // []) | map(test("internal/application")) | any)) | select(($s[$id].status // "missing") != "merged") | "\($id) \($s[$id].status)"' tasks/tasks.json tasks/task_status.json
# 输出：（空）
# (c) T0909 的 scope 与标题
jq -r '.tasks[] | select(.id=="T0909") | "\(.title) :: \(.allowed_scope | join(", "))"' tasks/tasks.json
# 输出：confirm 的失败路径必须原子化：状态与记录要么一起落，要么都不落 :: internal/application/researchcontext/**, internal/persistence/**, cmd/api/searchhttp/**, tests/integration/**
```

即 T0909 是 `Confirm` 的失败路径原子化，scope 里没有 `internal/application/reopens`。唯一一笔 scope 与书
都覆盖 reopen 的任务是 **T0610**（`internal/application/**` 等；书 `tasks/packages/T0610.json` 提到 reopen 39 处；
`#T0610-TEST-01` reopen transition 为 `passed`）——但 T0610 是**已 merged 的交付方**，A/F/G 是它
**交付之后**才记下的欠账。**「今天没有任何任务承接 A/F/G」这句按上述三条命令成立。**

**规则**：报告里凡说「任务 X 承接了 Y」，X 都必须能指到那笔任务自己的任务书（`allowed_scope` 或书里的明确条目）；
**指不到就不是承接，是备忘**。本条属于备忘——它在 `tasks/decisions.md:16971` 有记录，但今天没有任何任务书认领它。

**V1 后补法**：reopen 权限是 L3（⑲ 已裁定），需 owner 决定谁可以 reopen；决定后**先立账一笔覆盖
`internal/application/reopens` 的任务**，由它修注释/步骤表/失败回退行为，再补 HTTP/MCP 路由。
往公开 API 加路由是 L2/L3，不在本项目已授权的范围内——这是它留下的原因。

### R5. 部署的 search 没有接任何 planner / embedder / answer provider

- 出处：`.rddev/workers/T0906-review/RESULT.json` `risks[0]`。
- 证据（以下命令均在本基准实跑，命中行抄录如下）：
  - 产品代码里没有构造过 planner：`grep -Rn "planner\.New" --include='*.go' cmd internal | grep -v '_test.go'`
    只命中两行**注释**——`cmd/api/main.go:1298` 与 `internal/search/answer/generator.go:69`，没有一次调用。
  - embedder 是 `nil`：`grep -n "NewRetriever(retrievalStore, nil)" cmd/api/main.go` 命中
    `1324:	searcher, err := retrieval.NewRetriever(retrievalStore, nil)`——第二个参数为 `nil`。
  - answer 生成器没给 provider：`grep -n "answer.New(answer.Deps" cmd/api/main.go` 命中
    `1334:	answerer, err := answer.New(answer.Deps{Logger: logger})`——只设了 logger。
  - `cmd/api/main.go:1295` 起的注释块自称「Two steps are deliberately left unwired」，
    并把 planner 无 provider、embedder 为 nil 写成「the supported state the search packages document
    rather than a gap」；也就是说这是**被记录的现状**，不是接线失败。
  - 因此每次 search 走**结构化回退**，语料的向量那一半**从不参与比较**（检索自身报
    `SignalReport.Skipped = no_embedder`）。

**影响**：这不许被写成「Gate E 通过」的模型综述版本；Gate E 第一条只能按**证据背书的「结构化」答案**
算满足（逐字判定见第 2 节 Gate E）。

**V1 后补法**：先回答 `tasks/decisions.md` L3-⓪（平台内容能不能发给外部模型服务）。
若允许，再派接线任务；若不允许，则 V1 的 search 保持结构化回退。

### R6. grounding guard 拒不了编造的裸名

- 出处：`.rddev/workers/T0906-review/RESULT.json` `risks[1]`；
  边界由 `internal/search/answer/grounding.go:172-177` 的注释自己写明（`:206` 是 `strings.ContainsRune(token, '@')` 的判定行，不是那段说明）。
- 证据：`namesEntity`（`internal/search/answer/grounding.go:205`）把含 `@` 的 token 视为引用，
  但一个 bare invented name（如 `CLM-999`，无 `@`、无平台 ref kind 前缀、无 uuid）不会被拒绝；
  该注释逐字写着 'a bare invented token like "CLM-999" ... is NOT refused by this guard'。

**影响**：未来 provider 可能在 prose 中断言一个未检索到的实体而不触发 fallback；当前结构化回退下此路径不活跃，但 guard 的边界是真实的。

**V1 后补法**：收紧 `namesEntity` 会改变 pinned goldens，应单独立任务并更新 fixtures。

### R7. `search_records` 写失败会把整次 search 变成 503

- 出处：`.rddev/workers/T0906-review/RESULT.json` `risks[2]`。
- 证据（本基准复跑）：记录写在返回答案之前（`cmd/api/searchhttp/service.go:214`：`s.records.Save` 失败即
  `return ErrRecordFailed`（`:225`），答案不返回）；传输层把它答成 **503 `SEARCH_RECORD_FAILED`**
  （`cmd/api/searchhttp/handlers.go:34` 定义该 code、`:209-214` 是它的分支，注释逐字写着
  'the answer was produced and could not be recorded, so it is not returned'）。
  503 的响应包一律带 `retryable=true`（`cmd/api/authhttp/envelope.go:86`：`Retryable: status == http.StatusServiceUnavailable`——
  `:85` 是 `RequestID:` 那行，上一轮引错了行号，已更正）。**这是有意的**：`service.go:116-125` 用一整段注释论证过「没有记录就不交答案」。
- **R7 的 code 名**：本基准上写记录失败有自己的 code `SEARCH_RECORD_FAILED`（泛化的 `SEARCH_UNAVAILABLE` 现在只覆盖「管线失败」）。

**影响**：一次 transient `search_records` 失败以可用性事故的形式表现；重试是新 search、新记录，语义正确但成本高。

**V1 后补法**：将写记录与返回答案解耦（异步 outbox 或返回后补写），并更新可用性预算。

### R8. L3-⓪ 未回答：平台内容能否送出到第三方 embedding / LLM 服务

- 出处：`tasks/decisions.md` L3-⓪（2026-09-19 补记）。
- 证据（以下命令在本基准实跑，命中行抄录如下）：
  - `grep -RniE "第三方|third[- ]party|出境|外部模型|外部服务" docs/*.md specs/*.md` 命中 3 行——
    `docs/62_SUPERVISOR_GOVERNANCE.md:53`（Supervisor 不得注册外部系统或第三方账号）、
    `docs/69_GITHUB_SOURCE_REPOSITORY.md:135`（CodeRabbit 只是 advisory 检查）、
    `docs/59_PRODUCT_ANALYTICS.md:5`（禁止把科研内容送第三方 analytics SaaS，**只针对分析型 SaaS**）。三行都不是对
    「项目名 / 知识对象正文 / 用户搜索词能不能发给平台之外的模型服务」的回答。
  - 放宽到模型词：同一检索加 `LLM|embedding` 共 11 行，其余 8 行是 `docs/20_TECH_ARCHITECTURE.md:71,77`、
    `docs/26_OBSERVABILITY.md:17`、`docs/27_PERFORMANCE_SLO.md:6,13`、`docs/52_BACKEND_STANDARD.md:23`、
    `docs/54_SECURITY_THREAT_MODEL.md:16`、`docs/55_DATA_CLASSIFICATION.md:11`——全都是接口形状、指标或分类规则，
    没有一行裁定内容能否出平台。
  - 口径：本报告只声称「按上述两条检索，`docs/**` 与 `specs/**` 里没有这个问题的答案或裁定」，
    **不**声称「全树不存在答案」；规格真相源是 `docs/**` 与 `specs/**`。

**影响**：它同时是隐私/法律问题与 §5.1 停止条件「需要新的外部凭证、付费服务或账号授权」
（真接 provider 必需要 API key，多数付费）。T0902/T0903 已收窄为「端口 + 确定性假件 + 零联网」，
而 L3-⓪ 里那句「要你答的一句话」至今未得到回答。

**V1 后补法**：owner 明确回答「可以/不可以/有条件可以」；选「可以」需同时定「哪些分类不许出」。

### R9. L3-① 未回答：登录之前能不能搜索

- 出处：`tasks/decisions.md` L3-①。
- 证据：规格 `docs/05_INFORMATION_ARCHITECTURE.md:9` 把搜索入口列在未登录可见栏；
  契约 `specs/api/openapi.yaml:10-11` 全局 `security: - bearerAuth: []`，且 `POST /search` 没有 `security: []`。
  当前实现按契约走，要求登录。

**影响**：Gate D 的「Explore/Search 可发现 public project/asset/…」在**匿名 `/search`** 这一层未启用
（Explore 那一半匿名可用，见 Gate D 的 D2）。

**V1 后补法**：owner 选 (a) 契约是疏漏则给 `/search` 加 `security: []` 并改 scope 组装层；
选 (b) 维持现状并修订规格中的「搜索入口」描述。

### R10. SAST、容器扫描、SBOM 三项缺席

- 出处：`ops/security/absent-checks.json`；`ops/runbook-steps.json:147-149` 的 `kind: absent`（`"kind": "absent"` 在 `:147`，那段 note 在 `:149`——上一轮写成 `:150`，已更正）。
- 证据（本报告实测）：
  ```bash
  bash tests/security/master-security-gate.sh --only absence-manifest
  ```
  输出：`ok item: sast`、`ok item: container-scan`、`ok item: sbom`，并打印 witness 命令证明缺口仍在。

**影响**：
- **SAST**：静态可见的 High（未净化 sink、缺失 authorization）可能无 gate 拦截，是三项中最大的覆盖缺口。
- **容器扫描**：当前无 Dockerfile/应用镜像，所以无对象可扫；一旦构建镜像，无扫描即可能 ship 高危 base layer。
- **SBOM**：Release 无法给出组件清单，下游消费者无合规材料。

**V1 后补法**：
- SAST：推荐 semgrep（单工具三语言）并固定规则集；加 master-gate 行并写入 CI。
- 容器扫描：先补 Dockerfile + CI 构建，再加 trivy/grype image scan。
- SBOM：每生态一个生成器，合并为 CycloneDX 文档绑定 Release。

> dependency/CVE 扫描（Go/Node/Python 三面）已由 T1206 补齐并实测干净，**不在**这份缺席清单里。

> 顺带一处**文档滞后**（不是缺口，也不影响本条的判定）：`ops/runbook-steps.json:149` 那条 `kind: absent` 的
> note 仍写着「there is no dependency/CVE scan and CI has no security job」。**前半句已过时**——T1206 补上了
> dependency/CVE 扫描（见 Gate H 证据第 1 条）；**后半句今天仍成立**——`ci.yml` 的 10 个必需作业里没有
> security 作业，CVE 扫描目前只由 master security gate 在合并门里按需跑。本报告不改 `ops/`，把这条记在这里供 Supervisor 复核。

### R11. `rddev worker collect` 的证据没说清它评了什么

- 出处：T0906 复核第 3 条，`.rddev/workers/T0906-review/RESULT.json` finding 3。
- 证据：那次 `diff.txt` 声明基线 `3e479507`，但逐字节等于 `git diff 3e2c43d 7db633f`（主线 tip → 分支 tip，44 个文件）；
  `collect-report.json` 的 head-baseline/branch-ref 与 `files_changed` 都是上一轮返工的增量。
  内容本身干净（那棵树是干净的、44 个文件都被评过），但 **G4 本应能从自己的记录里回答「评了什么」**。

**影响**：这是**工具**的欠账，不是产品的；它让合并门的记录无法自证审查范围。

**V1 后补法**：修复 `rddev collect` 的基线选择（用实际 merge base 或 main tip）并在 collect 时重新捕获 HEAD/ref/files_changed。

### R12. MCP 工具面未接线（L3-③ 未裁定）——Gate G 未通过的直接理由

- 出处：`tasks/decisions.md` **㊱**（2026-09-23）；复核起点是 `.rddev/workers/T1207-review/RESULT.json`。
- 事实（本基准实跑，命令与输出见第 2 节 Gate G）：
  1. `specs/mcp/tools.json` 声明 **21 条**工具，全树 **0 个 dispatch 点**
     （`/tmp/t1207-cgate -root . -write-mcp …` 重建出同一结论；已合并的 `ops/contract/mcp-inventory.json` 逐字节同结论）；
  2. `cmd/mcp-server/main.go:77-80` 的 `/mcp` 按设计答 **501**，正文
     `{"error":"MCP protocol wiring not implemented yet (agent tasks)"}`；
  3. `docs/02_V1_SCOPE.md:52` 把「MCP/API 读写科研状态」列进 **V1 必做**，§4（`:60-70`）**没有豁免它**；
     `docs/56_REQUIREMENTS_TRACEABILITY.md:15` 也把它挂在 Gate G 上。
- **性质**：**这不是待补的工程活，是一条未获裁定的 L3。** 把 21 条工具建起来要先有产品语义
  （每条工具的参数与返回、`proposal` 模式在操作上意味着什么、`docs/02:53` 那句「显式 approval path」是什么形状），
  规格一处都没写；agent 权限模型本身也是 §5.1 的停止条件。
- **处置**（㊱ 已定，本报告只引用）：两条路交 owner——**裁定「建」**→ 立一笔覆盖 MCP 工具面与 approval path 的任务；
  **裁定「不建」**→ 把 `specs/mcp/tools.json` 与 `docs/02:52`、`docs/56:15` **一起**降级
  （只动代码不动规格 = 把不一致从 specs 挪进代码）。
- **V1 后补法**：在裁定之前，Gate G 的失分就挂在第 1 条上，**不接工、不改 `/mcp` 的 501、不删目录**。

### R13. OpenAPI 契约与挂载路由的对账是红的，而且没有任何东西在跑它

- 出处：T1205 的交付（`ops/contract/route-inventory.json`、`tests/cmd/contractgate`）与其 RESULT 摘要。
- 证据（本基准实跑；**直接跑二进制才看得到它自己的退出码 3**，`go run` 会包一层让 shell 看到 1，两种跑法都试过）：
  ```bash
  go build -o /tmp/t1207-cgate ./tests/cmd/contractgate
  /tmp/t1207-cgate -root . | head -2
  # 输出：contractgate: . (server prefix /api/v1, contract specs/api/openapi.yaml, exemptions specs/api/openapi-exemptions.yaml)
  #       summary: mounted=134 in_contract=30 catchall_resolved=6 exempted=0 undocumented=98 | contract_ops=40 unmounted=3 | exemptions=0 cited=0 files_scanned=735
  /tmp/t1207-cgate -root . >/dev/null 2>&1; echo "exit=$?"
  # 输出：101 finding(s). Exit 3.  →  exit=3
  go run ./tests/cmd/contractgate -root . >/dev/null 2>&1; echo "exit=$?"
  # 输出：exit=1（go run 的包装层；程序自己写的是 3）
  ```
- **红的性质**：这不是「刚变红」。T1205 的 RESULT 摘要逐字写着
  `PINNED RED at merge: mounted=134 in_contract=30 catchall_resolved=6 exempted=0 undocumented=98 | contract_ops=40 unmounted=3 | exemptions=0 cited=0 files_scanned=728 | advisory=41`
  ——**这条红是带着已知状态合并的**（当时的 `files_scanned=728`，今天 735；`undocumented=98`、`unmounted=3` 两处不变）。
- **而且没有人在跑它**：`.github/workflows/`、`Makefile`、`scripts/` 里对 `contractgate` 的引用是 **0 处**
  （`grep -rn contractgate .github/workflows/ Makefile scripts/` 零命中）。`specs/api/openapi-exemptions.yaml`
  存在，但对账自己的读数是 `exemptions=0 cited=0`——**一份没人用的豁免文件**。
- **性质第二层**：这是一条**已知红、且无人跑的对账仪**，不是"新发现的缺陷"。本报告只**点名**它
  （`tasks/decisions.md` ㊲ 把它的收尾记为 V1 之后立账的 **T1211**：98 条 undocumented 的证据、
  `openapi-exemptions.yaml` 的裁定、3 条 unmounted、以及 contractgate 要不要接进 CI——**接 CI 会让 main 变红**，
  必须与那 98 条的证据一起动）。**该不该收进 CI 是 Supervisor 的裁定，本报告不自己下结论。**

---

## 4. 关键复跑命令清单

```bash
# 1. 当前基准
git rev-parse HEAD
# 输出：9b22339e6398b714de5cc810454437ee33e51913

# 2. 审计脚本（本任务的 final audit）：报告标记、数字、名单、计数块、基准 commit 全对才 exit 0
bash tests/acceptance/v1-final-audit.sh
# 输出末行：AUDIT OK: report markers present, counts match (0 unmerged, 1 not_run, 0 failed, 0 skipped).
#
# 2b. 这条命令是**基准快照**：它只在报告自己的基准上有意义。HEAD 前进之后（合并、落账、
#     任何一次提交）它必然以非 0 退出，并打印「证书过期」的说明与两条出路——那不是报告错了，
#     是报告记录的那棵树不是这一棵。要么在那个 SHA 上跑，要么先重新生成报告：
bash tests/acceptance/v1-final-audit.sh --emit-tables
# 输出：生成注释行 + 「> 生成基准：`<当前 HEAD>`」+ 未合并任务表 + blocking not_run 表 + 计数块。
# 把生成基准行贴回报告头部、两张表贴回第 1.2 / 1.3 节、计数块贴回第 1.3 节
# （判定与证据由人按台账改，数字只许来自脚本），再造新基准的证书。

# 3. blocking not_run 测试
jq '.tests | map(select(.blocking == true and .status == "not_run")) | {count: length, ids: map(.id), names: map(.name)}' tasks/tests.json
# 输出：{"count": 1, "ids": ["T1207-TEST-01"], "names": ["final audit"]}（本报告自己的审计条目）
jq -c '[.tests[]] | group_by(.status) | map({status: .[0].status, count: length})' tasks/tests.json
# 输出：[{"status":"not_run","count":1},{"status":"passed","count":176}]

# 4. Gate I / MOF canonical workflow（需要真实服务）
#    注意：本轮**没有**重跑这一条（它需要整套 PostgreSQL/Redis/MinIO/Gitea/浏览器栈）。
#    Gate I 的数字来自 T1202 的运行记录：.rddev/workers/T1202/RESULT.json，账本条目 #T1202-TEST-01。
bash tests/acceptance/mof-canonical-workflow.sh

# 5. Gate H / 依赖与 CVE 审计（本报告实测，三条都 exit 0）
bash tests/security/master-security-gate.sh --only vuln-go,vuln-node,vuln-python
# 输出：Go `No vulnerabilities found.`（结论行；另有 3 条「依赖模块里存在、代码不可达」的告警，工具自己分列）
#       Node `No known vulnerabilities found` / Python `Found no known vulnerabilities and no adverse project statuses in 6 packages`
bash tests/security/master-security-gate.sh --only secret-scan
bash tests/security/master-security-gate.sh --only absence-manifest

# 5b. Gate H / CI 必需作业集合：权威是这两个文件本身，三处必须是同一个集合
python3 - <<'PY'
import re, json
jobs = set(re.findall(r'^  ([a-z0-9_-]+):$', open('.github/workflows/ci.yml').read().split('\njobs:\n',1)[1], re.M))
g = json.load(open('specs/orchestrator/gates.json'))
print(len(jobs), sorted(jobs))
print(jobs == set(g['required_jobs']) == set(g['gates']['G2']['runs_jobs']) == set(g['gates']['G4']['asserts_jobs']))
PY
# 输出：10 ['a11y', 'acceptance', 'go', 'i18n', 'migration-integration', 'observability', 'python', 'spec-validation', 'task-state', 'web']
#       True

# 5c. Gate G / MCP 工具面对账（本报告实测：21 声明的、0 个 dispatch 点）
go build -o /tmp/t1207-cgate ./tests/cmd/contractgate
/tmp/t1207-cgate -root . -write-mcp /tmp/t1207-mcp-inventory.json | head -2
# 输出：MCP reconciliation: catalog specs/mcp/tools.json declares 21 tool(s); 0 have a dispatch site; 21 do not.
jq '{declared_tools, tools_with_a_dispatch_site}' /tmp/t1207-mcp-inventory.json
# 输出：{"declared_tools":21,"tools_with_a_dispatch_site":0}
sed -n '77,80p' cmd/mcp-server/main.go
# 输出：/mcp 的 501 存根与正文 `{"error":"MCP protocol wiring not implemented yet (agent tasks)"}`
sed -n '51,53p;60p' docs/02_V1_SCOPE.md
# 输出：### Agent interface / - MCP/API 读写科研状态。/ … / ## 4. V1 明确不做（§4 没有豁免它）

# 5d. Gate G / 契约对账（R13：已知红、且无人跑它；退出码 3 是程序自己的）
/tmp/t1207-cgate -root . | head -2
# 输出：summary: mounted=134 in_contract=30 catchall_resolved=6 exempted=0 undocumented=98 | contract_ops=40 unmounted=3 | exemptions=0 cited=0 files_scanned=735
/tmp/t1207-cgate -root . >/dev/null 2>&1; echo "exit=$?"
# 输出：exit=3（末行 101 finding(s). Exit 3.）
grep -rn contractgate .github/workflows/ Makefile scripts/ || echo "0 callers"
# 输出：0 callers

# 6. 已知缺口核对（本基准实测的输出写在注释里）
# blob 写入路径：契约声明 vs 产品实现
grep -n "request-upload" specs/api/openapi.yaml
# 期望：234:  /projects/{projectId}/branches/{branchId}/blobs:request-upload:
grep -Rn "request-upload\|requestUpload" cmd/api internal || echo "zero hits in cmd/api and internal"
# 期望：zero hits in cmd/api and internal
grep -Rli "INSERT INTO blobs" --include='*.go' --include='*.sql' . | sort
# 期望：internal/persistence/queries/blobs.sql、internal/persistence/sqlc/blobs.sql.go、
#       tests/acceptance/portability/export.go、tests/acceptance/seeddemo/blobs.go、
#       tests/e2e-release/harness/main.go、tests/integration/*_test.go（12 个文件）
#       ——产品代码（cmd/**、internal/**）里没有调用者
# reopen 路由
grep -Rn "reopen" specs/api/openapi.yaml || echo "zero hits in openapi.yaml"
# 期望：zero hits in openapi.yaml
# search 接入情况：planner / embedder / answer provider 三处
grep -Rn "planner\.New" --include='*.go' cmd internal | grep -v '_test.go'
# 期望：只有两行注释（cmd/api/main.go:1298、internal/search/answer/generator.go:69），无调用
grep -n "NewRetriever(retrievalStore, nil)" cmd/api/main.go
# 期望：1324:	searcher, err := retrieval.NewRetriever(retrievalStore, nil)
grep -n "answer.New(answer.Deps" cmd/api/main.go
# 期望：1334:	answerer, err := answer.New(answer.Deps{Logger: logger})

# 7. 报告里「全部 passed」类全称断言的核对
#    输出落点：tasks/tests.json 的 tests[].status（台账本身就是证据位置）
jq -r '.tests[] | select(.id|test("^T04(0[1-9]|1[01])-TEST-01$")) | "\(.id) \(.status)"' tasks/tests.json | sort
# 期望：T0401-TEST-01 … T0411-TEST-01 共 11 行，全部 passed（Gate B）
jq -r '.tests[] | select(.id|test("^T030[1-5]-TEST-01$")) | "\(.id) \(.status)"' tasks/tests.json | sort
# 期望：5 行全 passed（Gate B/G 的 Git 边界断言）
jq -r '.tests[] | select(.id|test("^T060[568]-TEST-01$")) | "\(.id) \(.status)"' tasks/tests.json | sort
# 期望：3 行全 passed（Gate C 的 Release 断言）
jq -r '.tests[] | select(.id|test("^T090[1-9]-TEST-01$")) | "\(.id) \(.status)"' tasks/tests.json | sort
# 期望：9 行全 passed（Gate D/E 的结构化检索与 grounding 断言）
jq -r '.tests[] | select(.id|test("^T00(09|10|11)-TEST-")) | "\(.id) \(.status)"' tasks/tests.json | sort
# 期望：8 行全 passed（Gate G 的开发系统隔离断言）
jq '[.tests[] | select((.id|test("^T04(0[1-9]|1[01])-TEST-01$")) or (.id|test("^T030[1-5]-TEST-01$")) or (.id|test("^T060[568]-TEST-01$")) or (.id|test("^T090[1-9]-TEST-01$")) or (.id|test("^T00(09|10|11)-TEST-"))) | select(.status!="passed")] | length'
# 期望：0（本基准实测输出 0，即上列全称断言没有反例）
jq '[.tests[] | select(.status=="passed") | select(((.evidence // "") | length) == 0)] | length'
# 期望：0（没有一条 passed 是空证据）

# 8. 谁承接：reopen 的 A/F/G 今天没有任何任务书认领（R4 的全称断言）
jq -r '.tasks[] | select(((.allowed_scope // []) | map(test("reopens")) | any)) | .id' tasks/tasks.json | wc -l
# 期望：0（150 笔任务里没有一笔的字面 scope 写 reopens）
jq -s -r '.[0].tasks as $d | .[1].tasks as $s | $d[] | .id as $id | select(((.allowed_scope // []) | map(test("internal/application")) | any)) | select(($s[$id].status // "missing") != "merged") | "\($id) \($s[$id].status)"' tasks/tasks.json tasks/task_status.json
# 期望：空（未 merged 只剩 T1207 自己一笔，它的 scope 碰不到 internal/application）

# 9. Gate C 资产那一半的状态（任务与测试分开取，别把「已合并」读成「已通过」）
jq -r '.tasks | to_entries[] | select(.key|test("^T070[689]$|^T0710$")) | "\(.key) \(.value.status)"' tasks/task_status.json | sort
# 期望：T0706 merged / T0708 merged / T0709 merged / T0710 merged
jq -r '.tests[] | select(.id|test("^T070[6789]-TEST-01$|^T0710-TEST-01$")) | "\(.id) \(.status)"' tasks/tests.json | sort
# 期望：T0706-TEST-01 passed / T0707-TEST-01 passed / T0708-TEST-01 passed /
#       T0709-TEST-01 passed / T0710-TEST-01 passed
jq '[.tests[] | select(.id|test("^T07(0[1-9]|1[0-2])-TEST-01$")) | select(.status!="passed")] | length'
# 期望：0（整个资产族 T0701–T0712 没有一条不是 passed）

# 10. 回填成色（R2 的数字）：账本里有多少条 evidence 自述是回填
jq '[.tests[] | select((.evidence // "") | test("Backfilled|补跑|backfilled"))] | length' tasks/tests.json
# 输出：140（177 条里，140 条的 evidence 写明是回填的；其中 127 条逐字写着 not a fresh re-run）
```

---

## 5. 验收与交付声明

- 本报告已覆盖 `docs/31_MASTER_ACCEPTANCE.md` Gate A–I（`:5-60`）与 Development System Gate（`:62-72`）共 10 条，
  每条都有判定与证据；**逐条 checkbox 共 46 项**（含 Gate I 的一句与 Dev Gate 的 7 条），每项一个判定：
  **44 项「通过」、1 项「未通过」（Gate G 第 1 条）、1 项「真空成立」（Gate G 第 2 条，按真空写）**。
  Gate 层面：**8 条「通过」、2 条「未通过」（Gate G、Gate H）**，最小判定单位在表里，不在这一句里。
- 判定为「未通过」的两条已在剩余风险里逐条出现：**Gate G ← R12**、**Gate H ← R10**；
  判定为「通过」但带缺口的 Gate A/C/D/E/F 已在 R1–R9、R11 中逐条出现；
  R13 是一条 Gate 判定之外的仪器欠账（点名，不自行处置）。两处条数能对上：未通过 2 = R12 + R10。
- `tasks/tests.json` 中 176 条 `passed`、1 条 `not_run`（`T1207-TEST-01`，本报告自己的审计条目）、
  **0 条 `failed`、0 条 `skipped`**；**本报告未修改任何 `status`**，账本只由 `scripts/record_test_run.py` /
  `scripts/reconcile_tests_ledger.py` 写真跑结果。
- 本任务（T1207）的 `allowed_scope` 内产物为 `tests/acceptance/v1-final-report.md` 与
  `tests/acceptance/v1-final-audit.sh`（后者含 `--emit-tables` 生成模式）；未触碰 `tasks/task_status.json`、
  `tasks/progress.md`、`tasks/**`、`specs/**` 与任何产品代码。
- **本版（第 5 轮 / 收尾轮）相对上一版（基准 `5b0d7d82`）改了什么**：
  ① 基准换成 `9b22339e`；② 第 2 节从「每个 Gate 一段散文」改成**每条 checkbox 一行**（判定 + 证据 + 落账位置，
     判定只取三值），并**读了测试体**再落笔（A2/A3/A5/A6/A7、B2、C2/C4、D2/D3/E3/E4 都给出文件与行号）；
  ③ **Gate G 由「通过」改判为「未通过」**（第 1 条被树反证：21 条声明 / 0 个 dispatch 点 / 501 / `docs/02:52` 未豁免），
     第 2 条按**真空成立**写；④ 新增 **R12**（MCP 工具面，L3-③）与 **R13**（契约对账仪，已知红且无人跑）；
  ⑤ R3 的 blob 清单补全（`tests/e2e-release/harness/main.go:859`，并写明那是测试装置不是产品代码），
     处置升级为 **L3-②**（㉟）；⑥ 三处行号更正（R4 的 `:720-723`、R7 的 `:86`、R10 的 `:149`）；
  ⑦ R8/Gate E 的第二句 L3-⓪ 判定改成**逐字陈述句**；⑧ 审计脚本新增 `failed`/`skipped` 非 0 即红的显式断言、
     新增计数块校验与「基准行必须在报告头部」的位置断言，并把「基准不符」的失败改成**专门的快照说明**（退出码照样非 0）；
  ⑨ `--emit-tables` 扩成够重建报告的那一份（生成基准行 + 两张表 + 计数块），第 1.2/1.3 节逐字贴入。
- **本报告不自行放宽任何门**：Gate H 的 SAST/容器扫描/SBOM 三项缺席照实写「未通过」，
  Gate G 的 MCP 工具面照实写「未通过」并把裁定权交回 owner（L3-③）；报告作者**不改台账、不改测试、不改判定口径**。
