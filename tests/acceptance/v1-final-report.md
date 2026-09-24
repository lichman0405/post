# V1 最终验收与交付报告

> 报告位置：`tests/acceptance/v1-final-report.md`  
> 生成基准：`91ed2d5fdb24c7a0b0f332d6861fb5ffde5eb86f`  
> 生成日期：2026-09-23  
> 审计脚本：`tests/acceptance/v1-final-audit.sh`（`--emit-tables` 生成第 1.2 / 1.3 节那两张表、生成基准行与计数块）

**这一版为什么换基准（第九轮 / 最终重钉轮，T1214，2026-09-23）**：**上一版把证书钉在 `a8850f51` 上，
而它的结论里带着一条「Gate G 未通过」，理由是 `L3-③`（V1 含不含 MCP 工具面）**还没有 owner 裁定**。
那份裁定在 2026-09-23 到了——owner 的回复是「按你说的来。」，五条 `L3` 建议**逐条照准**，落地记录是
`tasks/decisions.md` ㊻、落地提交是 `90167ba`（`docs/02_V1_SCOPE.md` §4 由此收窄：MCP / Agent 工具面、
面向用户的上传入口、把内容送出平台、公开声明的发现面四条**移出** V1 范围）。**裁定动了判据，所以这份
证书要最后一次重钉**：本版把证书钉到 T1214 的树（`91ed2d5`）上，并在**这一棵树上同一时刻**重新挣得：
① **Gate H**：总门实跑（`2026-09-23 19:27:32 → 19:28:07`）退出码 **0**、17 行全绿、`NOT ASKED` **0** 行、
`MIN_CHECKS=17`；② **Gate I**：MOF canonical workflow 实跑（`19:25:59 → 19:26:41`）退出码 **0**、
50 步全绿、`FAIL` **0** 条；③ **机械块**（§1.2 / §1.3 两张表、生成基准行、计数块）：逐字来自本轮
`bash tests/acceptance/v1-final-audit.sh --emit-tables` 的**同一次输出**。**哪几层重挣、哪几层沿用、
沿用的机械理由**逐层写在**第 6 节 6.0「修订记录（本次重钉）」**里，连同每层驱动路径的
`git diff --stat` 原文。**基准行换掉之后，正文里提到旧基准的地方都改写成「上一版（基准 `a8850f51`）」
这类说法**，逐个交代谁是哪一版的树；`a8850f51` / `115a4286` / `b6c1fb1a` / `9b22339e` 一律照此标注，
不留一个光秃秃的 SHA 指代当前基准。
**这一版改判的是 Gate G 的**处置**（按范围豁免），不是它的四条判据**——`docs/31_MASTER_ACCEPTANCE.md`
一个字节没动（`git diff --stat HEAD -- docs/` 是空的）：被豁免的是**主体**，理由是 owner 的裁定把它移出了
V1 范围；`L3-②` 的两条端点则从契约里删掉了（`specs/api/openapi.yaml`，依据与两次实跑见 ㊻ §3）。

**上一版为什么换基准（第八轮 / 重钉轮，T1213，2026-09-23）**：**上一版把证书钉在 `115a4286` 上，
而那一版之后 `main` 又前进了三次合并（T1210 / T1211 / T1212），其中 T1211 改的正是 Gate H 赖以得绿
的那套仪器本身**——`tests/security/**` 与 `ops/security/**` 十一处、428 插入（`git diff --stat
b6c1fb1a..HEAD -- tests/security/ ops/security/` 的原文见第 6 节）。仪器变了，用旧仪器挣来的绿就不是
这份交付上的绿，所以**那一版（第八轮）**把证书重钉到 T1213 的树（`a8850f51`）上，并在**那一棵树上
同一时刻**重新挣得：
① **Gate H**：总门实跑（`2026-09-23 11:00:36 → 11:01:22`）退出码 **0**、17 行全绿、`MIN_CHECKS=17`；
② **Gate I**：MOF canonical workflow 实跑（`11:01:40 → 11:02:15`）退出码 **0**、50 步全绿、`FAIL` **0** 条；
③ **机械块**（§1.2 / §1.3 两张表、生成基准行、计数块）：逐字来自本轮
`bash tests/acceptance/v1-final-audit.sh --emit-tables` 的同一次输出。**哪几层重挣、哪几层沿用、沿用的
机械理由**逐层写在**第 6 节「修订记录（本次重钉）」**里，连同每层驱动路径的 `git diff --stat` 原文。
**基准行换掉之后，正文里提到旧基准的地方都改写成「上一版（基准 `115a4286`）」这类说法**，逐个交代
谁是哪一版的树；旧的 `b6c1fb1a` / `9b22339e` 一律照此标注，不留一个光秃秃的 SHA 指代当前基准。

**再上一版为什么换基准（第七轮 / 返工轮，T1210，2026-09-23）**：那一版证书（基准 `b6c1fb1a`）在验收时 `G2` 红了，
红的不是这份报告与它的审计脚本，而是 CI 的 `security-master` 作业里第 3 步——`.github/workflows/ci.yml`
里那句**无条件**的 `sudo apt-get update -qq && sudo apt-get install -y --no-install-recommends redis-tools`。
G2 在**不是 GitHub runner** 的机器上逐字重跑 CI 的步骤，那里 `sudo` 没有 tty 可用来读密码，
于是这一步退出码 1，作业在任何一条安全行跑起来之前就失败了（逐字报错：`sudo: a terminal is required to read
the password`）。它要装的工具其实已经在 `/usr/bin/redis-cli`（7.0.15）。**修的是那句话，不是断言**：
`115a4286` 把它改成「先断言工具在，缺了才去装」，改在 `.github/workflows/ci.yml` 与随之重新生成的
`specs/**` 派生文件里——那在本任务的 `allowed_scope`（`tests/acceptance/**`）**之外**，所以这不是本报告改的，
是本报告要认的新基准。那一版把证书重新钉到 `115a4286` 上，并把判定依据在新树上再跑一遍——
**出处在上一版（第七轮）报告的第 5.3 节**，那条 `**本版（T1210，第七轮 / 返工轮）相对上一版（基准
`b6c1fb1a`）改了什么**` 的第 **②**（总门在新基准上真跑一遍）与第 **③** 条（MOF canonical workflow
在新基准上真跑一遍）；**第八轮把那一组条目重排为第 6 节「修订记录」的一部分**，逐层说清「重挣 / 沿用」。
（**T1214 订正**：这段原文指向的「上一版的第 5.3 节 ⑧」是一个**从来没有解析过的编号**——上一版报告
（`f5a8a8a`）与本版所依据的那份 §5.3 都只编到 ⑦/⑥，`⑧` 只在那一句话自己里出现过（拿它去两份报告上各 `grep -n` 一遍：各只有两处命中——这句指路语自己、以及一条 `grep -oE` 命令的字符类；输出在 RESULT 里）。改法是把它换成
**真实存在的那两个条目**，不是删掉出处：就是**第 6 节 6.2 里标着「重挣」的那两行——Gate H 行与 Gate I 行**（「总门在新基准上真跑一遍」与「MOF canonical workflow 在新基准上真跑一遍」）。）

**这份报告是某一棵树上的证书，审计脚本是它的快照校验器。** `bash tests/acceptance/v1-final-audit.sh`
只在**报告自己的基准**上有意义：报告钉住的每个数字（`v1_required` 清单、`tests.json` 分布、
blocking `not_run` 名单）都是在上面那个 SHA 上取的一次快照，HEAD 前进一次（合并、落账、任何一次提交）
它就与当前树不同步。**合并或账本变动之后，必须先重新生成报告**（用
`bash tests/acceptance/v1-final-audit.sh --emit-tables` 的产物刷新基准行 + 两张表 + 计数块，
判定与证据由人按台账改、数字不许手写），再造新基准的证书；在那之前，在任何别的树上跑那条命令都会
以非 0 退出，并打印「证书过期」的说明与两条出路（第 4 节第 2 条）。

## 0. 结论（放在最前）

**V1 的两条台账条件在本基准 commit 上已经满足：`v1_required=true` 的任务（排除 T1207 自身）全部 `merged`；blocking 测试里剩下的 `not_run` 只有 1 条，就是「本报告自己那道还没落账的门」。** 但这**不等于**「V1 可以宣告完成」——下一条说了为什么。

- `tasks/tasks.json` 中 `v1_required=true` 的任务共 **150** 笔。排除 T1207 自身后，未 merged 的有 **0 笔**（脚本会打印 `EXCLUDED=T1207`，让人看得出那是一次具名排除而不是悄悄减一；**这条排除今天是空操作**，理由见第 6 节）。
- `tasks/tests.json` 里 blocking 条目共 **183** 条：**183 条 = 182 `passed` + 1 `not_run`**，没有 `failed`、没有 `skipped`、没有非 blocking 条目。**这 1 条 `not_run` 是 `T1214-TEST-01`（certificate final revision）——就是这份交付物自己的门**，按 `scripts/record_test_run.py` 的规矩由 Supervisor 在它的 G2 落账；**报告作者不能给自己的门记 `passed`**，本报告不修改任何 `status`。上一版钉的那一条（`T1213-TEST-01` certificate revision）在 T1213 合并之后已由 Supervisor 按既定程序实跑并落账为 `passed`（`tasks/decisions.md` ㊺ §6 记着那次运行的退出码与负对照）。
- `docs/31_MASTER_ACCEPTANCE.md` 的 10 条 Gate 里，**10 条判「通过」，0 条判「未通过」**。
  - **Gate G 由「未通过」改判为按范围豁免通过**：上一版判「未通过」的唯一失分项是第 1 条「MCP semantic tools 完整且权限正确」，而它挂的是一条**待 owner 裁定的 L3**（`L3-③`，V1 含不含 MCP 工具面）。owner 2026-09-23 裁定 **V1 不含**，`docs/02_V1_SCOPE.md` §4 把主体移出了 V1 范围（㊻、落地提交 `90167ba`）。**这四个字要说准：是范围豁免，不是「造出来并验过」**——`/mcp` 今天仍然按设计返回 501、21 条工具仍然 0 个 dispatch 点（本基准实测），周知的不是它能不能用，而是它**不在 V1 的范围里**。第 2 条的处置同见 Gate G 一节。见 R12。
  - **Gate H 的贯通**：更早一版（基准 `9b22339e`）未通过的唯一理由是 **SAST / 容器扫描 / SBOM 三项覆盖缺席**（R10）。T1209（合并提交 `034a341`，PR #355）之后这条理由不再成立。**这一条的判定在本版重新挣过一次**：本版在 `91ed2d5` 上**真跑了一遍**总门（**不带** `--allow-not-asked`）：退出码 **0**、17 行全绿、`NOT ASKED` **0** 行、`MIN_CHECKS=17`。H1 那一行的判定带着限定——**「0」是扫描面上的结论，扫描面之外仍有一条有守卫的缺席（容器扫描）**，见 Gate H 与 R10。
- 未闭合的缺口在剩余风险里逐条点名，共 **14 条**（R1–R14）。其中 4 条是**产品**侧（R3 blob 写入路径、R4 reopen 无入口、R5 search 无模型侧、R7 search_records 503），1 条是**工具**侧（R11 `rddev worker collect` 的证据基线），1 条是**仪器**侧（R13 契约对账仪没有跑它的人），其余是证据成色与**已裁定的范围外事项**（R8 / R9 / R12 / R14 四条今天都已由 owner 的裁定收口——**收口的意思是「主体不在 V1」，不是「验证过了」**）。
- **五条 L3 已全部获得裁定**（owner 2026-09-23「按你说的来。」，逐条记录在 `tasks/decisions.md` **㊻**，落地提交 **`90167ba`**）。**逐条点名，每条给出裁定结果、日期与那个指针**；每一条下面写的是**裁定结果**，不是「待办」：

**L3-⓪（平台内容能不能出平台）**
**裁定**：**不外发**——V1 不把平台内容发送到平台之外的第三方服务（模型、向量、分析），只保留接口位置与确定性假件、不接线；日期 **2026-09-23**；落点 `tasks/decisions.md` **㊻** 第 1 节那一行 + `docs/02_V1_SCOPE.md` §4 第 1 条 + `docs/55`。**R8 由此收口为「范围外」**：撤销的是一条接线需求，不是一条「已验证不外发」的结论。

**L3-①（登录之前能不能搜索）**
**裁定**：**不改**——搜索与 Research Answer 一律要求登录，匿名只可见公开页面本身；日期 **2026-09-23**；落点 `tasks/decisions.md` **㊻** 第 1 节 + `docs/02_V1_SCOPE.md` §3 Open Network 那一句。**R9 由此收口**：契约本来就是登录后可用，今天的产品行为就是裁定后的行为。

**L3-②（V1 含不含 blob 上传入口）**
**裁定**：**不要**——V1 不含面向用户的大文件上传入口，契约不声明那两条端点、Web 不含上传 UI；日期 **2026-09-23**；落点 `tasks/decisions.md` **㊻** 第 1 节与 §3 + `docs/02_V1_SCOPE.md` §4 第 2 条 + `docs/15`/`17`/`23`/`27` 四份规格同步。**R3 由此重新定性为范围外，不是欠账**（标记句 `产品的 blob 写入路径缺失` 作为事实仍然成立，见 R3）。

**L3-③（V1 含不含 MCP 工具面）**
**裁定**：**V1 不含** MCP / Agent 工具面——`specs/mcp/tools.json` 的 21 条工具与 `docs/47` 是 V1.x 的设计输入，V1 不交付、不接线；日期 **2026-09-23**；落点 `tasks/decisions.md` **㊻** 第 1 节 + `docs/02_V1_SCOPE.md` §3/§4 + `docs/56_REQUIREMENTS_TRACEABILITY.md` 那一行 + `specs/mcp/tools.json` 的 `"scope": "post-v1"` 标记 + `docs/47_MCP_TOOL_CATALOG.md` 的横幅。**Gate G 第 1 条由此按范围豁免处置**（见 §2 Gate G 与 R12）。

**L3-④（公开声明该在哪里被发现）**
**裁定**：**保持现状**——attestation 只有知道编号 `pid` 才可达，不提供列表、搜索或导航入口，「只有知道编号才可达」是产品决定、不是缺口；日期 **2026-09-23**；落点 `tasks/decisions.md` **㊻** 第 1 节 + `docs/02_V1_SCOPE.md` §4 第 4 条 + `docs/12_PERMISSIONS_RIGHTS_POLICY.md` §2。**R14 由此收口**。

五条的裁定都不改判据本身：`docs/31_MASTER_ACCEPTANCE.md` 与 `docs/12`/`docs/23` 一个字没动，
动的是**范围**（`docs/02` §4）与随之收窄的契约（`specs/api/openapi.yaml` 删两条从未实现的端点）。

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

> 生成基准：`91ed2d5fdb24c7a0b0f332d6861fb5ffde5eb86f`

| 任务 | 状态 | 阶段 | 标题 |
|------|------|------|------|
| （无） | — | — | v1_required=true 且非 merged 的任务 0 笔（已排除 T1207 自身） |

**计数：0 / 0（目标为 0，排除 T1207 后实际为 0）**。脚本输出会显式打印 `EXCLUDED=T1207`。

> 台账说明：更早一版（基准 `9b22339e`）此表为空，但当时 T1207 自己还在返工轮里；这一版为空是因为 T1207 早已合并
> （`392e6d6`）且 `T1207-TEST-01` 已落账。脚本仍按定义排除 T1207 并在 `EXCLUDED=` 里具名打印。
> **排除不是"减一"的同义词**：脚本打印它排除了谁，第 4 节的 4a 还逐条比对名单本身。
> **这条排除今天是空操作**（T1207 已 merged，本来就不会进这张表）——本版照原样保留它，理由与现状见第 6 节 6.0(d)。

### 1.3 blocking 测试状态

```bash
# 复跑命令
jq '.tests | map(select(.blocking == true and .status == "not_run")) | {count: length, ids: map(.id), names: map(.name)}' tasks/tests.json
```

当前结果：**1 条 blocking 测试为 `not_run`**，不是 `passed`。清单（同一次 `--emit-tables` 输出的后半段）：

| 测试 ID | 任务 | 名称 |
|---------|------|------|
| T1214-TEST-01 | T1214 | certificate final revision |

<!-- 计数块：报告引用的每个数字都由脚本给出，不许手写 -->
```text
COUNTS v1_required_total=150
COUNTS v1_required_unmerged=0
COUNTS excluded=T1207
COUNTS tests_total=183
COUNTS tests_passed=182
COUNTS tests_not_run=1
COUNTS tests_failed=0
COUNTS tests_skipped=0
COUNTS tests_blocking=183
```

> 账本规则：没有真实命令的 `passed` 不算证据。本报告未修改 `tasks/tests.json` 的任何 `status`。
> 这 1 条 `not_run` 是**这份交付物自己的门**：`T1214-TEST-01`（certificate final revision）由 Supervisor 在
> 本笔的 G2 上按账本规矩落账——**报告作者不能给自己的门记 `passed`**，所以它在报告交付的这一刻必然
> 还是 `not_run`，这不是缺口，是归属。上一版钉的那一条（`T1213-TEST-01` certificate revision）已在
> T1213 合并后由 Supervisor 实跑落账为 `passed`（㊺ §6 记着那次运行与它的负对照）。

本基准上 `tasks/tests.json` 的**整体分布**（复跑命令与结果如下，均在本基准 `91ed2d5` 实跑）：

```bash
jq -c '[.tests[]] | group_by(.status) | map({status: .[0].status, count: length})' tasks/tests.json
# 输出：[{"status":"not_run","count":1},{"status":"passed","count":182}]
jq '[.tests[] | select(.blocking==true)] | length' tasks/tests.json
# 输出：183（即全部条目都是 blocking=true）
jq '[.tests[] | select(.status=="failed")] | length' tasks/tests.json
# 输出：0
jq '[.tests[] | select(.status=="skipped")] | length' tasks/tests.json
# 输出：0
jq '[.tests[] | select(.status=="passed") | select((((.evidence // "") | gsub("[[:space:]]"; "")) | length) == 0)] | length' tasks/tests.json
# 输出：0（没有一条 passed 是空证据；这一条 T1210 起也钉进审计脚本，直接数台账）
```

> **最后那条命令本版换了写法（T1210 最终复核 nit，T1213 改）**：报告原来印给读者复跑的是
> `select(((.evidence // "") | length) == 0)`，而审计脚本第 3 节钉的是**先去空白再去长度**的那一版；
> 一条 `evidence` 全是空白的 `passed` 会让脚本红、却让照报告复跑的人读到 `0`。这里改成与脚本**逐字相同**
> 的写法。**改的是报告印的那条命令，不是脚本的断言**——脚本那一行一个字没动，断言没有被放宽。

也就是：**183 条 = 182 `passed` + 1 `not_run`**，没有 `failed`、没有 `skipped`、没有非 blocking 条目。
上表那 1 条就是这 1 条 `not_run` 的全部。T1206-TEST-01、T1207-TEST-01、T1209-TEST-01 都已是 `passed`
（分别是 9 行、快照、17 行那三版 master suite / 审计的运行记录，见 Gate H 与第 4 节）；
上一版钉的 T1213-TEST-01 也已在合并后由 Supervisor 实跑落账（㊺ §6）。
**上一版报的是 182 条 = 181 `passed` + 1 `not_run`（基准 `a8850f51`）：那 1 条 `not_run` 是 T1213 自己
那道门；那笔合并、那条落账、再加上本笔自己的那一行之后，账本变成今天的 183 / 182 / 1。**

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
| B2 | Semantic conflict 不被 Agent 自动科学裁决 | 通过 | 「人类裁决、agent 只有建议权」被写进代码并被测试钉住：`internal/application/resolutions/doc.go:43-47` 逐字——"The agent explanation … is advisory-only: nothing in this package, the HTTP surface or the UI derives a decision from it. The only outcomes are the five human-chosen kinds; there is no computed, averaged or auto-selected kind."；`tests/e2e/conflict_e2e_test.go:437 TestE2EConflictResolution` 第 8 步断言审计行的 actor 是**人**（`audit actor = %s, want alice %s (human-governed)`，`:621`；**本版把行号从 `:447` 改成 `:437`**——T1212（合并提交 `79aaa8d`）把该文件顶部那段本地的 `pgReachable` 探针换成 `testdb.RequireDB`，函数整体上移，`:447` 今天是 `if view.Report.AutoMergeable {`；断言的**内容**一个字没动，见第 6 节 6.0 第 ② 条）；`internal/rsg/conflict/conflict.go:37` 把 rights/visibility 冲突标成"never auto" | `#T0405-TEST-01` conflict unit、`#T0407-TEST-01` conflict e2e，均 `passed` |
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
**由证据背书的「结构化」答案满足；模型撰写的综述不在 V1 范围内——`L3-⓪` 已裁定「不外发」，模型综述因此不在 V1 里。**
（**T1214 订正这一句**：原文写「因为它卡在一条**未获裁定**的 L3（外部凭证/付费服务）上」——
那条 L3 在 2026-09-23 已由 owner 裁定为**不外发**，`tasks/decisions.md` **㊻**、`docs/02_V1_SCOPE.md` §4 第 1 条。
**判定一个字没动**：E1 仍然只在「结构化答案」这个口径下通过；变的只是「为什么没有模型综述」的答案——
从「等裁定」变成「裁定说不做」。）
这两句指到 L3-⓪ 本身：它逐字记着「平台上的内容——项目名、知识对象正文、用户敲进搜索框的那句话——可不可以发给平台之外的模型服务」，真接一个 provider 就必然命中 §5.1 的停止条件（新的外部凭证/付费服务）——**owner 答的是「不外发」，所以这条路 V1 不走**。

**处置**：4 / 4 通过，但 E1 的通过是**在"结构化答案"这个口径下**的通过——把它读成"平台能用模型写综述"就是失真。**这条 L3 现在有裁定了**（不外发），所以「模型综述」从「悬置」变成「不在 V1 范围内」。其余三条 search 缺口（无 planner/embedder/answer provider、grounding 拒不了裸名、`search_records` 写失败转 503）见 R5/R6/R7。

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
（更早一版曾判「未通过」，理由是五笔任务未 merged；五笔全部 merged、所列测试全部 `passed` 之后按台账事实改对，
`tasks/decisions.md` ㊲ 记录了那次复核结论）。改判**没有放宽任何断言**：`#T1101-TEST-01`/`#T1103-TEST-01`
等通过证据本身是回填的 Worker 记录（不是独立复跑），这条性质记入 R1。

### Gate G — Agent/API/Git

`docs/31_MASTER_ACCEPTANCE.md:45-49`，四条 checkbox。**这一节在本版改判。改判的是处置，不是判据**：
`docs/31` 一个字节没动（`git diff --stat HEAD -- docs/` 是空的），动的是**范围**——owner 2026-09-23
裁定把两条的主体移出了 V1（`L3-②`/`L3-③`，`docs/02_V1_SCOPE.md` §4，㊻、落地提交 `90167ba`）。
四条逐条处置如下，一条不含糊：

| # | checkbox | 判定 | 证据（命令 / 树内位置） | 落账位置 |
|---|----------|------|--------------------------|----------|
| G1 | MCP semantic tools 完整且权限正确 | **范围豁免** | **主体不在 V1**：owner 的 `L3-③` 裁定把 MCP / Agent 工具面移出了 V1 范围，书面依据是**规格与契约**（下一条列全四处：`docs/02` §3/§4、`docs/56`、`specs/mcp/tools.json` 的 `"scope": "post-v1"`、`docs/47` 横幅）。**这不是「造出来了、验过了」**——恰恰相反，树里**没有**这一面：`/mcp` 返回 501、21 条工具 0 个 dispatch 点（同一节第 1 段实测），今天与上一版一模一样 | 无——**豁免不需要落一条「通过」的账**：它引的是那四处规格，不是某一次运行 |
| G2 | Agent 无 merge/publish visibility 扩大权限 | **通过（保护在已交付面上成立；主体不在 V1）** | **主体（agent 工具面）同样不在 V1**（同 `L3-③`）；而这一条要防的**保护**——「没有任何路径能不经过人就把可见性扩大、把 main 合掉」——在 V1 **真正交付的那两个面**上成立，且每一面都有既有证据：见下方第 2 段（Semantic HTTP API 面 / Web 面，各点名到命令与记录） | 三条 `passed`（人类路径）+ 本报告在本基准那次 Gate I 实跑里的 `[33]..[35]` 负向断言 |
| G3 | Git branch push ingestion；main direct push 拒绝 | 通过 | `#T0302-TEST-01` git protection e2e、`#T0305-TEST-01` git ingestion integration、`#T0306-TEST-01` unstructured negative | 三条 `passed` |
| G4 | Git/RSG drift reconciliation 可检测 | 通过 | `#T0301-TEST-01` gitea integration、`#T0309-TEST-01` reconciliation integration | 两条 `passed` |

**1. G1 的事实没变，变的是定性。本基准实测（与上一版同一棵树上的结论逐条相同）**：

```bash
# 1. /mcp 是一个 501 存根——它按设计就答 501，不是「没接线的半成品」
sed -n '77,80p' cmd/mcp-server/main.go
#   mux.HandleFunc("/mcp", func(w http.ResponseWriter, _ *http.Request) {
#     w.Header().Set("Content-Type", "application/json")
#     w.WriteHeader(http.StatusNotImplemented)
#     fmt.Fprintf(w, `{"error":"MCP protocol wiring not implemented yet (agent tasks)"}`)

# 2. 目录声明 21 条工具，全树 0 个 dispatch 点（重建 T1205 的那份对账）
go build -o /tmp/t1214-cgate ./tests/cmd/contractgate
/tmp/t1214-cgate -root . -write-mcp /tmp/t1214-mcp-inventory.json | head -2
# 输出：MCP reconciliation: catalog specs/mcp/tools.json declares 21 tool(s); 0 have a dispatch site; 21 do not.
jq '{declared_tools, tools_with_a_dispatch_site, tools_the_catalog_declares_and_the_tree_does_not_dispatch}' /tmp/t1214-mcp-inventory.json
# 输出：{"declared_tools":21,"tools_with_a_dispatch_site":0,"tools_the_catalog_declares_and_the_tree_does_not_dispatch":21}

# 3. **豁免的书面依据在这里**（T1214 换掉的那三条：原文引的是「docs/02:52 列进必做、§4 没有豁免它」，
#    而裁定之后那三行已经不成立——规格反过来了）
sed -n '54,55p' docs/02_V1_SCOPE.md
#   - MCP / Agent 工具面 **V1 不含**：`specs/mcp/tools.json` 与 `docs/47` 是 V1.x 的设计输入，
#     不是 V1 交付物（`L3-③` 裁定，见 §4）。
sed -n '73,74p' docs/02_V1_SCOPE.md
#   - **MCP / Agent 工具面**：`specs/mcp/tools.json` 声明的 21 条工具与 `docs/47` 是 V1.x 设计，
#     V1 不交付、不接线（`L3-③`）。
sed -n '16p' docs/56_REQUIREMENTS_TRACEABILITY.md
#   | MCP 工具面 | 15/47 | 无 —— **V1 不含**（`docs/02` §4，`L3-③`） | — |
sed -n '3p' specs/mcp/tools.json
#     "scope": "post-v1",
sed -n '3,5p' docs/47_MCP_TOOL_CATALOG.md
#   > **V1 不含 MCP 工具面**（`L3-③` 裁定，`docs/02` §3/§4）：本目录是 V1.x 的设计输入，
#   > V1 不交付、不接线（`cmd/mcp-server` 的 `/mcp` 按设计返回 501）。机器定义因此保留原样，
#   > 并在 `specs/mcp/tools.json` 里带 `"scope": "post-v1"` 标记。
```

上面第 3 段那**四处**（`docs/02_V1_SCOPE.md` §3 与 §4 的两条、`docs/56_REQUIREMENTS_TRACEABILITY.md`
第 16 行、`specs/mcp/tools.json` 第 3 行的 `"scope": "post-v1"`、`docs/47_MCP_TOOL_CATALOG.md` 第 3-5 行的
横幅）就是这一条豁免的全部依据。**豁免与「通过」必须不可能被读混，所以这里再说一遍**：
「范围豁免」的意思是 **owner 把主体移出了 V1 范围**（`L3-③`：「V1 不含 MCP / Agent 工具面」），
**不是**「这一面建起来了、验过了」。后者今天在树里是**假**的：21 条工具仍然 0 个 dispatch 点、
`/mcp` 仍然是 501。报告不写「已验证」，也不写「待补」——它写的是**范围**。
`L3-②` 是 §4 里并列的另一条范围裁定（用户上传入口也不在 V1），它的落点是 R3。

重建出的对账与已合并的 `ops/contract/mcp-inventory.json` 结论一致（`declared_tools=21`、
`tools_with_a_dispatch_site=0`），只有两处行号漂了（提交版记 `cmd/api/rsghttp/page.go:408`/`:401`，
当前树那两个字面量在 `:419`/`:412`）——**漂的是"哪里提到了这个名字"，不是"有没有接线"**。

**2. G2 的保护落在哪两个面上、各有什么证据。** 这一条要防的是**一条路径**：
「不经过人就能把可见性扩大、把 main 合掉」。主体（agent 工具面）不在 V1，所以**不存在
一条 agent 路径可以用来做行为级验证**——这句话照实写着，它上一版就是这么写的（当时叫「真空成立」）。
但这条 checkbox 要防的那个**保护**并不依赖 agent 工具面，它在 V1 真正交付的两个面上都被钉住了：

- **Semantic HTTP API 面**（`cmd/api`，契约 `specs/api/openapi.yaml`，`docs/56` 第 15 行把它挂在本 Gate 上）：
  权限矩阵 `#T0105-TEST-01`（`go test ./internal/authz/ -count=1`）；
  merge governance e2e `#T0409-TEST-01`（`TestMergeGovernanceEndToEnd`，`-count=20`）；
  publish security e2e `#T0705-TEST-01`（`go test ./tests/integration -count=1 -v -run 'TestAssetPublish|TestKnowledgePublish'`）
  ——其中 `tests/integration/asset_publish_test.go:521 TestAssetPublishRefusesAnAgentToken` 断言 agent 的 publish 被
  `ErrAgentNotPermitted` 拒（`:530`），**并以同一条人类 session 的成功作为对照**（两道防线各自独立）；
  另外 `tests/integration/freeze_main_e2e_test.go:613` 的 (5b) 让带 agent 标记的 **owner** 去冻结 main，
  断言它被**两道防线各自独立**拒绝（`:623`、`:627`、`:630`）——对照是同一名 owner 不带 agent 标记时能冻结。
- **Web 面**（`apps/web`）：治理动作在页面上就是**一次人类的写**，不是一次自动升级。
  本报告在**本基准**上跑的那次 Gate I（下方 Gate I 一节，命令与输出同源）里有它的负向半边，逐字：
  `[33] NEGATIVE: a non-member's merge of the same proposal is refused [fetch]` →
  `ok   the server refused it 403 AUTH_FORBIDDEN and reported no merge`；
  `[34] … ok   the proposal is still merge_ready`；`[35] … ok   main's head state is the one it had before the refusal`；
  而 `[36]` 那次 merge 是 **owner 自己**发的、`[37]` 在真 Chromium 里渲染出来。
  Web 面上唯一一处「能写文件」的地方（Files）被断言为严格只读（`#T0308-TEST-01`，`bash tests/e2e-files/run.sh`）。

**这两面的证据成色照实说**：上面 API 面那几条是**回填的 Worker 记录 / Supervisor 补跑**，不是本报告在本基准上的
独立复跑（成色记在 R1 / R2）；Web 面那三条负向断言是**本报告在本基准上刚跑过的那一次**（Gate I 的实跑，
`19:25:59 → 19:26:41`，退出码 0）。**G2 的判定因此写成「保护在已交付面上成立；主体不在 V1」，
而不是「agent 工具面验证通过」**——后者今天没有对象。

**处置**：**通过**。四条里 **G1 按范围豁免**、**G2 按「主体不在 V1 + 保护在已交付面上成立」**、
G3 与 G4 沿用原有证据。**为什么 G1 不是「未通过」**：它的判据要么要求「21 条工具完整且权限正确」，
要么要求「这一面在 V1 里」；owner 的裁定明确选了后者之外的第三条——**这一面不在 V1 里**
（`docs/02` §4、`docs/56` 那一行、`90167ba` 里的 `scope` 标记与 `docs/47` 横幅）。**为什么 G1 也不是「通过」**：
树里没有它，把它写成「通过」就是**把范围豁免说成已验证**——那正是这一节要避免的混读。
失分项随之消失，是因为**它守的那个东西被移出了范围**，不是因为断言松了。见 R12（改写后）。

### Gate H — Security/Quality

`docs/31_MASTER_ACCEPTANCE.md:51-57`，六条 checkbox：

| # | checkbox | 判定 | 证据（命令 / 树内位置） | 落账位置 |
|---|----------|------|--------------------------|----------|
| H1 | Critical/High security issues = 0 | **通过（限定：扫描面之外仍有一条有守卫的缺席）** | 总门实跑（**不带** `--allow-not-asked`）退出码 **0**、17 行全绿；SAST 三行（`sast-go`/`sast-python`/`sast-node`）、SBOM 三行（`sbom-go`/`sbom-node`/`sbom-python`）与 `license-audit` 各自打印自己的证据行；扫描面之外唯一的缺席是容器扫描（有守卫 + 书面接受），见下方三段 | 本报告实测；`#T1206-TEST-01`（9 行那版）与 `#T1209-TEST-01`（17 行那版）两条 master suite 均 `passed` |
| H2 | permission negative E2E 全过 | 通过 | `#T0106-TEST-01` privacy negative e2e（非成员读私密项目被拒）、`#T0105-TEST-01` 权限矩阵、`#T0705-TEST-01` publish security e2e | 三条 `passed` |
| H3 | backup/restore tested | 通过 | `#T1110-TEST-01` restore drill（`./ops/backup-restore-drill.sh --no-infra` → `TestRestoreDrill`）、`#T1111-TEST-01` restore drill e2e、`#T1204-TEST-01` runbook drill | 三条 `passed` |
| H4 | local one-command dev | 通过 | 机制是 `make dev`（`Makefile:337`，从 `.env.dev` 一条命令起五个应用、Ctrl-C 全停；**本版把行号从 `:324` 改成 `:337`**——T1212 在 Makefile 顶部改了 `GO_UNIT_PKGS` 那段注释与定义，三行下移，`:324` 今天是一个空行；目标本身没动）；环境前置与可执行性由 `#T0000-TEST-01/02`、`#T1204-TEST-01` 落账 | 三条 `passed`。**本报告本轮没有亲自跑 `make dev`**——它起的是整套本地栈，这一条的证据是那三条记录 |
| H5 | CI blocking suites 全绿 | 通过 | 必需作业集合的权威是**三个**文件本身：`.github/workflows/ci.yml`、`specs/orchestrator/gates.json` 与那条把两者钉在一起的单测。本报告实测四处逐字对得上（脚本见第 4 节第 5b 条）：`11 ['a11y','acceptance','go','i18n','migration-integration','observability','python','security-master','spec-validation','task-state','web']` 与 `True`（四处的集合相等）。**第 11 个必需作业是 T1209 的 `security-master`**，本报告在它进 CI 之后才生成证书 | 本报告实测。**本报告没有在本基准上触发 CI**：每个必需任务合并前的 G4 以"所有必需作业绿"为断言，本基准的 G2 就是 CI 的 exact steps（第 5.2 节） |
| H6 | accessibility AA core pages | 通过 | **两个入口，分别说清是哪一次给了哪一句**：① **`make a11y`**（CI 必需作业 `a11y`，`tests/web-smoke/run-a11y.sh`，真 PostgreSQL + 真 `next start`，自带「核心页面**带数据**的条数 ≥ 7」这条下限断言）：axe-core WCAG pass + best-practice pass 双双 0 违规，`coverage: 7 core pages scanned with data (>= 7)`——本报告本轮**没有**亲自跑它，这一句的证据是 `#T1104-TEST-01`（记的是一次真实的 CI `a11y` 作业运行）。② **总门里的 `a11y` 那一行**（`tests/web-smoke/run.sh`，对的是桩 API `API_BASE_URL=http://api.e2e.test`）：本报告本轮实跑，`ok a11y`，两种规则集同样 0 违规、键盘与 reduced-motion 断言全绿；但这一行里**核心页面没有数据**（它自己打印 `core pages scanned with data: 0`，`/search` 那条按 `服务关` 计、不计入 core-page AA pass）——它证明的是无障碍结构，不是带数据页面。两份证据合起来才是这一条 checkbox | ①`#T1104-TEST-01` `passed`；②本报告本轮总门实跑（第 2 节 Gate H 第 1 段，退出码 0、17 行） |

**处置**：**通过——这一节在更早一版是「未通过」，改判的依据是树变了，不是断言松了。**
更早一版写在报告里的条件是「若 Supervisor 的裁定是『V1 完成不以这三项为条件』，Gate H 应随之改判为通过」；
那份裁定**已经存在**，而且不是一句话，是一件一件的落地，逐条引在下面。

**1. 总门在承版本版证书的那棵树上真跑了一遍（本基准 `91ed2d5` 实测，不带 `--allow-not-asked`）。**

```bash
make security-tools    # 先把总门每一行的前置装齐（写的是被忽略的路径；T1209 的 CI 作业里同一顺序）
bash tests/security/master-security-gate.sh --report /tmp/t1214-master-gate-report.json
# 退出码：0（本轮实跑：2026-09-23 19:27:32 +08:00 起，19:28:07 止）
# 末行逐字：master-security-gate: PASS — 17 check(s) ran and each printed its own evidence (report: /tmp/t1214-master-gate-report.json)
# `NOT ASKED` 行数：0（17 行全部真跑；这一点由这一行自己的措辞与注册表下限共同钉住）
# summary 表 17 行逐行 `ok`（`grep -c '^  ok  '` = 17），没有任何一行是 NOT ASKED 或 FAIL
```

**本版（第九轮）为什么又在新树上跑了一次——照实说：不是因为安全仪器变了。**

```bash
git diff --stat a8850f51..HEAD -- tests/security ops/security
# （无输出：这一层的驱动路径一个字没动，所以「沿用」本来也成立）
```

**沿用本来成立，本版仍然重跑了一次**：重跑四十秒，误判的代价却是一份证书里最吃重的一条。上面那次运行
就是它。**上一版（第八轮，基准 `a8850f51`）那次运行（`11:00:36 → 11:01:22`）没有作废**——它仍然
正确地记录了 `a8850f51` 那一刻的事实；**它只是不再是本版的依据**。两次数值一致这一点本版照实写：
同为 17 行、同为退出码 0、`NOT ASKED` 同为 0 行。**这不是把旧证据删掉**，是把「哪一次属于哪棵树」
说清楚，见第 6 节 6.0 的逐层账。

**更早那一层为什么要重跑（第八轮的理由，照原样留在这里）**：**因为仪器变了，而它上一层的绿是旧仪器给的。**
第八轮把证书钉在 `115a4286`（**T1211 合并之前**的树）时，T1211 改的正是 `tests/security/**` 与
`ops/security/**`（总门注册表、`absent-checks.json` 的词表与守卫、工具钉版、变异脚本共十一处，428 插入）。
**在一套已经被换掉的仪器上挣来的绿，只能证明旧仪器在那个旧树上是绿的**，所以第八轮在 `a8850f51` 上
同一时刻重跑了整门。（第八轮之前那次 07:13 的运行同理没有作废，只是不再是谁的依据。）

**更早那一次换基准（`b6c1fb1a` → `115a4286`）的原因仍照原样留在这里**：更早一版证书（基准 `b6c1fb1a`）
在验收时 `G2` 红的正是这个作业里的**安装步**，不是总门本身——CI 的 `security-master` 作业在第 3 步无条件跑
`sudo apt-get` 装 `redis-tools`，而 G2 不是在 GitHub runner 上跑（那里 `sudo` 没有 tty 可读密码），
于是它退出码 1，总门那一行**根本没轮到**。修法是把那一步换成「先断言 `redis-cli` 在
（`command -v redis-cli >/dev/null || { … }`）、缺了才装」，落在 `115a4286` 的 `.github/workflows/ci.yml` 里
（那是上一笔任务的 `allowed_scope` 之外，所以不是那份报告改的）。**本版核对了那段接线的落点仍在**：
`.github/workflows/ci.yml` 里那句守卫逐字是 `command -v redis-cli >/dev/null || { sudo apt-get update -qq && … }`。

**这一条要照实说清楚**：在一棵没有 `node_modules` / `.venv` 的冷树上，同一命令的退出码是 **2**，
有 5 行 `NOT ASKED`（`a11y`、`sast-node`、`sbom-node`、`sbom-python`、`license-audit`——它们的前置是
工作区依赖与适配器虚拟环境）。`make security-tools` 装齐之后这 5 行变成真跑的行，退出码才是 0。
**本报告没有用 `--allow-not-asked` 把那个 2 说成绿**：`--allow-not-asked` 是给裸 CI runner 的，
它的语义是「承认这次没问全」，不是「问了」。

**2. 注册表下限 `MIN_CHECKS=17`**（`tests/security/master-security-gate.sh:140`，这一行逐字是
`MIN_CHECKS=17`）。少一行注册即注册表完整性失败、退出码 3——删检查不是把门变绿的办法。
这个下限与「守卫能红」都是 T1209 自己 G2 逐条变异验过的，结论写在
`docs/adr/ADR-028-v1-ships-no-container-image.md` 的 Decision 第 2 条与 Consequences。
**本版把行号从 `:120` 改成 `:140`**：T1211 往同一个脚本里加了行（uv 钉版断言、词表与守卫），
`MIN_CHECKS=` 那一行整体下移 20 行——**这正是「仪器变了」在报告里的第一个具体后果**，
也是本版必须重挣 Gate H 而不是沿用旧判定的原因之一；`:120` 今天属于别的代码。
`check(s) registered` 的计数本次实测是 **17**（「17 check(s) registered」逐字出现在
`absence-manifest` 那一行自己的证据行里，见下面第 3 段的第 3 行）。

**3. `absence-manifest` 那一行的证据行（本基准实测，逐字抄录）**：

> **这一段是逐字引文，里面的绝对路径属于被抄录的那次输出，不是报告在给读者指路。**
> 在这一段的输出里出现 `/home/shibo/code/post/.rddev/worktrees/T1214/ops/security/absent-checks.json`
> 是那个 checker 自己打印的**它读的是哪个文件**——改成相对路径就不再是逐字抄录了。
> **本版对绝对路径的处置是「保留 + 在标题里说明」**（另一种做法是脱敏，但那样这一段就不能再声称逐字），
> 全报告的绝对路径都按这一条处理，说明与理由见第 6 节。

```text
---- absence-manifest: Absence manifest: every §11 capability covered by a named row or named as absent, and the guard itself checked ----
     ok   selftest: the guard it cites is not a check of this gate makes this checker exit 1 and name container-scan
     ok   selftest: the absence entry is deleted outright makes this checker exit 1 and name container-scan
     ok   manifest: /home/shibo/code/post/.rddev/worktrees/T1214/ops/security/absent-checks.json parses and the gate registry was read (17 check(s) registered)
     ok   manifest: items are exactly the required capabilities of docs/23 §11 (plus the SBOM of docs/25 item 10)
     ok   manifest: the absent set is exactly the container scan (SAST and the SBOM are gate rows now)
     ok   item: dependency-audit — covered by vuln-go, vuln-node, vuln-python, present in the gate registry
     ok   item: sast — covered by sast-go, sast-python, sast-node, present in the gate registry
     ok   item: secret-scan — covered by secret-scan, present in the gate registry
     ok   item: owasp-smoke — covered by owasp-smoke, present in the gate registry
     ok   item: permission-e2e — covered by permission-negative-e2e, present in the gate registry
     ok   item: container-scan — witness still shows the gap (`find . \( -name .git -o -name .rddev -o -name node_modules -o -name .next -o -name .venv -o -name .sbom -o -name .backup-dr -o -name .dev -o -name post-wt \) -prune -o -type f \( -name 'Dockerfile' -o -name 'Dockerfile.*' -o -name '*.Dockerfile' -o -name 'Containerfile' -o -name 'Containerfile.*' \) -print` printed nothing)
     ok   item: container-scan — witness `grep -c '"id": "no-dockerfile"' ops/runbook-steps.json` matches '^1$'
     ok   item: container-scan — witness `grep -c 'no-dockerfile' docs/adr/ADR-028-v1-ships-no-container-image.md` matches '^[1-9][0-9]*$'
     ok   item: container-scan — guarded absence: the 'container-scan' row of this gate is registered and goes red the moment the absence stops being real
     ok   item: sbom — covered by sbom-go, sbom-node, sbom-python, license-audit, present in the gate registry
ok   absence-manifest
```

那三行 `ok   item: container-scan — witness …` 就是这份「有守卫的缺席」的证据行：第一行是那次**真跑的**
「全树找容器构建文件」走查（要求无输出），第二行要求 `no-dockerfile` 这条 tree claim 仍登记在
`ops/runbook-steps.json` 里、且**恰好一次**，第三行要求**那份书面接受自己还在**
（`grep -c 'no-dockerfile' docs/adr/ADR-028-v1-ships-no-container-image.md` 非零）——
**书面接受被改名或删掉，这一行当场红**。

**3b. 这一行的两个调用方式，以及复核意见 S-3 的处置。** 复核意见说上面那段引文「漏抄了结尾的
`check-absent-manifest: OK — …`」。**那一行属于这个 checker 的另一种调用方式，不属于总门**：
`tests/security/check-absent-manifest.py` 只在**非 `--selftest`** 模式的最后打印它（`:371`），
而总门那一行注册的命令是 `python3 tests/security/check-absent-manifest.py --selftest`
（`tests/security/master-security-gate.sh` 的 `add_check absence-manifest`），它的 `--selftest` 分支在
`:358-363` 处 `return 1 if FAILS else 0`——**根本不走那句话**。本报告两次独立实跑（全门一次、
`--only absence-manifest` 一次）的 stdout 里都没有这一行，`--report` 写出的 JSON 里也没有；
把它抄进那段引文，就是把两次不同的运行混成一次。两种调用方式各自的末尾，逐字如下：

```text
$ python3 tests/security/check-absent-manifest.py --selftest   # 总门注册的就是这一条
...（15 行 ok，末行）
     ok   item: sbom — covered by sbom-go, sbom-node, sbom-python, license-audit, present in the gate registry
（没有结尾的 check-absent-manifest: OK 行；退出码 0）

$ python3 tests/security/check-absent-manifest.py               # 非 selftest：有那一行，但没有 selftest 两行
...（13 行 ok，末两行）
（空行）
check-absent-manifest: OK — every §11 capability is covered or named, and every named gap is still a gap
（退出码 0）
```

`diff` 两次输出只有两处不同：`--selftest` 多两行 `ok   selftest: …`、非 `--selftest` 多一行结尾 OK。
**所以第 3 段那段引文在它自己的调用方式下是完整的，这一条复核意见按「不采纳」处置，证据就是上面这两次实跑。**

> **第八轮（T1213）的复核（2026-09-23，基准 `a8850f51`）：上面两段是那一轮复核的对象，逐字保留、不改。**
> 那一版在这棵树上把两种调用方式各重跑一次，结果同形——`--selftest` 15 行 `ok`、末行仍是
> `ok   item: sbom — …`、**无**结尾 OK 行、退出码 0；不带参数 13 行 `ok`、末两行是空行 + OK 行、退出码 0；
> `diff` 两次输出仍只有上面说的那两处不同。**一处行号不精确在此点名、不就地改**（改了就是改上一轮被复核的东西）：
> 上一段引的区间 `:358-363` **不含**那个 `return`——`tests/security/check-absent-manifest.py` 里
> `if args.selftest:` 在 **`:357`**、`return 1 if FAILS else 0` 在 **`:364`**、非 `--selftest` 那句 OK 的
> `print` 在 **`:371`**（`grep -n` 实测），正确区间是 `:357-364`。这不影响这一段的结论（结论是「那句 OK 属于
> 另一种调用方式、总门这次调用不打印它」），但引文里的行号应更正。本报告不改那两行原文，只在此点名。

**4. SAST 与 SBOM 已经是总门里的实检行，不再是缺席。** 本基准实测它们各自的证据行
（逐字出自**上面第 1 段那次总门实跑**的同一份输出 `/tmp/t1214-gate.log`，不是另跑一次）：

```bash
grep -n 'sast-go:\|sbom-node:' /tmp/t1214-gate.log
# 129:     ok   sast-go: gosec v2.29.0: 798 file(s) scanned, 254 finding(s) reported (254 baselined (reviewed), 0 unbaselined), 2 file(s) it could not type-check (listed below — a finding inside one of them would not be a finding)
# 158:     ok   sbom-node: pnpm 12.4.1 sbom (pnpm-lock.yaml, CycloneDX 1.5): 364 component(s), spec 1.5, licence: 363 spdx id / 0 expression / 1 none
```

**本版这两个数（798 / 254、364）与第八轮（基准 `a8850f51`）逐个相同，理由是可查的**：
`git diff --name-status a8850f51..HEAD` 里**没有一个 `.go` 文件、没有一个 `pnpm-lock.yaml`**——
这一版换基准只动了 `docs/**`、`specs/**`、`tasks/**` 与 `tests/acceptance/**`。
上一层（第八轮）这两个数在它自己更早一版上曾从 `797 file(s) scanned` 变成 798，那一层已经把
口径差异写清楚并声明**不拿它当证据**（`sast-go` 这一行印的是 gosec 自己的 `Stats.files`，
`tests/security/sast_report.py:288,300` 打印、`tests/security/sast.sh:118` 取值）：这一行的判定是
「有这一行、它自己打印 OK、`unbaselined` 为 0」，与行数浮不浮动无关。下方 `container-scan`
那一行的走查数同理。

八条新行是 `sast-go` / `sast-python` / `sast-node`、`sbom-go` / `sbom-node` / `sbom-python`、`license-audit`、
`container-scan`（注册表从 9 行升到 17 行）。**一个可复现的 High 是红，不是"已记录"**：SAST 每一处 finding
都要在 `ops/ci/gosec-baseline.txt` / `bandit-baseline.txt` / `eslint-security-baseline.txt` 里有**逐条**的
基线条目（不允许目录、glob 或整条规则的豁免），`sast_report.py` 拒收那种条目。

**5. 唯一剩下的缺席是容器扫描，它有一份书面风险接受。**
`docs/adr/ADR-028-v1-ships-no-container-image.md`（**存在，且被 `absence-manifest` 那一行以 witness 指着**）
逐字写明：它**就是** `docs/23_SECURITY_PRIVACY.md:45`（§11）要求的那份「书面 ADR / risk acceptance」在
container scan 这一项上的形式，范围**仅限**「V1 交付不含任何镜像」，并且**自我失效**——镜像一出现，
`container-scan` 那一行转红即失效信号。它引用的守卫行名就是 `container-scan`（也就是
`tests/security/master-security-gate.sh` 注册表里的那一行，`tests/security/container-scan.sh` 是它的实现）。

**H1 的限定写在这里，也写在那一行里**：「Critical/High = 0」是**扫描面上的结论**（dependency audit /
SAST / secret scan / OWASP smoke / permission E2E 都有真跑的行走过），**扫描面之外**仍有一条有守卫的缺席：
容器扫描今天没有对象可扫（全树无 `Dockerfile`/`Containerfile`，本基准实测走查 **2113** 个文件——这个数由那一行自己打印，
随树里**被忽略的**构建产物浮动（本地跑一次门就会多出 `tests/web-smoke/server.log`、`__pycache__/*.pyc` 之类；
上一个基准上它先后打印过 2110 与 2112，同一棵树、同一行，差别只在装没装前置产物），
判定不依赖它，判定依赖的是同一行打印的 tree claim `no-dockerfile`），所以 `docs/23 §11` 六项里
有五项由实检行覆盖、第六项由一份**可失效的书面接受**覆盖。**这不是把缺席说成扫描**：那一行自己逐字写着
"it is not a container scan and does not pretend to be one"。

> **若这份改判的哪一条不成立，Gate H 就回到「未通过」**：ADF-028 被撤（第三行 witness 红）、
> 树里出现镜像（守卫红）、或某一行的证据行不再打印。三条都是可复跑的。

> 顺带一处**文档滞后**（不是缺口，也不影响本条的判定）：`ops/runbook-steps.json:149` 那条 `kind: absent`
> 的 note 仍写着「there is no dependency/CVE scan and CI has no security job」。**这两半今天都不成立**——
> 依赖/CVE 扫描由 T1206 补齐（`vuln-go`/`vuln-node`/`vuln-python` 三行），CI 的**第 11 个必需作业
> `security-master`** 由 T1209 接上（本报告实测四处的必需作业集合都是 11 个）。同一文件 `:144` 是那条
> 条目的 id（`release-2-changelog-flags-scan`）、`:147` 是 `"kind": "absent"`、`:149` 是这段 note。
> 本报告不改 `ops/`，把这条记在这里供 Supervisor 复核——**它是一处笔记过期，不是一处缺口**。

> dependency/CVE 扫描（Go/Node/Python 三面）已由 T1206 补齐并实测干净，**不在这份缺席清单里**，
> 它今天是总门的三条实检行。

### Gate I — Canonical Workflow

`docs/31_MASTER_ACCEPTANCE.md:59-60`，一句全文：

> 完整 MOF workflow 从 Research Question 到外部项目贡献 Evidence 与 Profile credit，全程真实 DB/Git/blob/browser，非 mock 演示。

| # | 判定 | 证据 | 落账位置 |
|---|------|------|----------|
| I1 | 通过 | **本报告在承载这份证书的那棵树（`91ed2d5`）上实跑**：`bash tests/acceptance/mof-canonical-workflow.sh` → 退出码 0，末行逐字 `G3 mof-canonical: PASSED — 50 step(s), every assertion green`（它下面还跟着一句 `  the seeded world, left in place for inspection: post_mof_demo_canonical_gate`）；步骤 `[01]..[50]` 无缺口（`50` 条 `[NN]`、`50` 条 `ok`）、`FAIL` 行 0 条。关键断言（逐字，本次运行）：[45] `ok   pid wwgh8wqztadda713d84yhfgc9r renders 1 assertion(s) from the external group: fails_to_reproduce/unreviewed`（pid 每次播种都新生成，所以它是**这一次**的那一个）；[46] `ok   demo-external is credited with 2 contribution row(s): scientific_object.version_created`；[47] `ok   affiliations=1 (demo-mof-external) and contributions=2 are rendered side by side`；[48] `ok   HTTP status and entries in its public knowledge feed (got 200|1)`、[49] `ok   publications the knowledge network names for the unpublished version (HTTP 404) (got 0)`（发布的那一版可发现、未发布的兄弟版本 404）；[50] `ok   git status --porcelain (a gate that changes the tree it grades has graded nothing) (got )`——**这一次 `got` 后面是空的**：本次运行前后两次 `git status --porcelain` 逐字相等、且两次都是空（`$DIRT_BEFORE` 在 `tests/acceptance/mof-canonical-workflow.sh:226` 取，比较在 `:634-635`），也就是跑的时候树是干净的（**这一句本版订正过一次**：第一稿写「跑的时候却已经是改完的静止状态」——那是错的，写它的时候我没查时间戳。查法：`grep -n "porcelain\|HEAD" /tmp/t1214-mof-out.txt` → 第 12 行 `ok   HEAD (got 91ed2d5fdb24c7a0b0f332d6861fb5ffde5eb86f)`、第 162 行 `ok   git status --porcelain (…) (got )`；再 `stat -c '%y'` 两个文件 → `tests/acceptance/v1-final-audit.sh` 改于 **19:29:06**、变异还原用的备份 `/tmp/t1214-report-backup.md` 建于 **19:36:23**，都在这次实跑（**19:25:59 → 19:26:41**）之后。所以**跑的时候这棵树还是 HEAD 原样、本轮改动一个字都还没落下**——`got` 后面空着，量的就是「原样」这个状态。**这不削弱这条引文**：Gate I 判的是这棵树，跑时 HEAD 正是证书钉的 `91ed2d5`，跑完 HEAD 逐字未变；它判的从来不是报告改没改完）；**更早那次运行（第八轮基准 `a8850f51`，pid `za4ezyp4wfzbm6jzjmzrzych1c`）这里同样 `got` 为空**——两次都说明「这次运行没有多改一个字节」 | `tasks/tests.json#T1202-TEST-01` `passed`（**回填**自 T1202 自己的 Worker 记录），**不是本轮这条结论的依据**；本轮依据是上面那次实跑。它的正式落账位置是 **T1214 自己的 G3 作业 `mof-canonical`**（`specs/orchestrator/gates.json` 的 `task_overrides.T1214.g3_jobs`，第 5.2 节）——**本报告不说那条 G3 记录已经落了**：本报告作者没有 orchestrator 权限，落 G3 记录的那一步在 Supervisor 那边；本报告给的是这次实跑的完整输出与退出码（完整 50 步输出留在 `/tmp/t1214-mof-out.txt`，同一终端会话 19:25:59 → 19:26:41） |

**处置**：通过。这一条**不是引用一条更早的记录**——T1214 的任务书把 Gate I 指定为它的 G3 作业
（`specs/orchestrator/gates.json` 的 `task_overrides.T1214.g3_jobs` = `["mof-canonical"]`，与 T1213 那笔逐字相同），
理由与 T1210 / T1213 那两版相同：证书里最吃重的跨边界断言是 Gate I（MOF canonical workflow 全闭环），
它必须在**承载这份证书的那棵树上重新挣得**。**本版重跑还有一个这一版独有的理由**：
这一版改的正是**报告自己**（§0 的 L3 裁定、Gate G 的处置、R3 的重新定性），而 Gate I 的 I1 行是这份报告
里唯一一条**必须当场实跑**、不能靠引用别人的记录支撑的断言。本报告照此在这棵树上跑了一次
（50 步全绿、退出码 0、`FAIL` **0** 条），命令与 T1214 的 G3 作业**是同一条**。
**这一条同时是 Gate G 第 2 条的一半证据**（`[33]..[35]` 那三条负向断言），而它们的步骤标签逐字是
`[fetch]`、不是 `[ui]`——**本报告不把那三条说成浏览器证据**：它们走的是产品的 HTTP 面
（带真实登录 session 的请求），浏览器那一半由同一次运行里 7 个 `[ui]` 步骤（`[20]`/`[27]`/`[28]`/`[29]`/`[32]`/`[37]`/`[40]`）承担。
**本报告不把「跑过一次」写成「以后不会再坏」**：这条证据的性质仍是一次快照（与整份报告同级），
而且它落成 `.rddev/runtime/gates/T1214/gate-run-*-g3.json` 那一步由 Supervisor 的 `rddev gate run` 完成——
本报告只交这次运行的输出与退出码，不声称那条记录已经存在。

### Development System Gate

`docs/31_MASTER_ACCEPTANCE.md:62-72`，七条：

| # | checkbox | 判定 | 证据 | 落账位置 |
|---|----------|------|------|----------|
| S1 | Ubuntu 24.04 canonical preflight 可复现 | 通过 | `#T0000-TEST-01` environment preflight unit、`#T0000-TEST-02` ubuntu smoke | 两条 `passed` |
| S2 | `rddev` 可恢复 task/worker/worktree 状态 | 通过 | `#T0009-TEST-02` task state integration、`#T0010-TEST-01/02/03` worktree 调度与并发/崩溃恢复 | 四条 `passed` |
| S3 | 至少两个独立 Worker 可并行完成互不冲突的示例任务 | 通过 | `#T0010-TEST-02` two-worker concurrency smoke | `passed` |
| S4 | Worker 无 Git remote credentials，commit/push/branch mutation 尝试被拒绝或验收强制拒绝 | 通过 | `#T0011-TEST-01` git control denial e2e、`#T0012-TEST-03` supervisor git control e2e | 两条 `passed` |
| S5 | Worker 越界 diff 被拒绝 | 通过 | `#T0011-TEST-02` scope violation e2e | `passed` |
| S6 | Supervisor 可独立执行 G2/G3/G4 并只在全绿后 commit/PR/merge | 通过 | `#T0012-TEST-01` four-gate acceptance e2e、`#T0012-TEST-02` rejection/retry e2e | 两条 `passed`。**逐层的权威与「绿记在哪」在第 5.2 节** |
| S7 | Supervisor 会话中断后可从仓库状态恢复，不依赖旧聊天上下文 | 通过 | 状态外部化在 `tasks/tasks.json`、`tasks/task_status.json`、`tasks/tests.json`、`tasks/progress.md`、`tasks/decisions.md`（本报告本身就是一次"断开后从仓库状态重建判断"的实例：它引用的每一处事实都指得到文件与行） | 台账文件本身（`scripts/record_test_run.py` / `scripts/reconcile_tests_ledger.py` 是唯一写 `status` 的入口） |

**处置**：7 / 7 通过。

---

## 3. 剩余风险（逐条点名）

本节与第 2 节对应：**本版 10 条 Gate 全部「通过」（0 条「未通过」），但每一条「通过」的 Gate 各自带缺口**，
以风险形式保留，一条不落地点名。**没有「基本通过」类措辞**：每条都写清事实、影响、以及
「V1 后怎么补 / 为什么不需要补」。**本版有四条（R3 / R8 / R9 / R14）的落点从「待裁定的 L3」变成了
「范围外」**——那是 owner 的裁定，不是工程侧的收口；四条各自在标题里写明改判，事实一字未动。
R12 从「Gate G 未通过的直接理由」变成「Gate G 按范围豁免的直接理由」，同样只动定性。

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

- 事实（本报告实测，**条数按本基准的 183 条重数**）：`tasks/tests.json` 共 **183** 条，其中 **140 条**的
  `evidence` 写明是回填的（**127 条**逐字写着 `not a fresh re-run`）。两条查询与输出：

  ```bash
  jq '[.tests[] | select((.evidence // "") | test("Backfilled|补跑|backfilled"))] | length' tasks/tests.json
  # 输出：140（就是第 4 节第 10 条那条命令；本版在 183 条的账本上重跑，得数不变）
  jq '[.tests[] | select((.evidence // "") | test("not a fresh re-run"))] | length' tasks/tests.json
  # 输出：127（"写明是回填"的 140 条里，逐字用这句话自述的那一部分）
  ```

  **一条自纠**：本版起草时先按 `backfill|not a fresh re-run`（忽略大小写）数出 128，
  并据此一度以为上一版的 140 复现不出来——**那是我的查询写错了，不是那个数错了**：
  报告自己第 4 节第 10 条钉的模式是 `Backfilled|补跑|backfilled`（大小写敏感），它给出 140。
  **数额改回 140，改的是"随着台账从 182 条长到 183 条重新数过"这件事**。
- 影响：**账上 182 条 `passed` 不等于 182 次独立复跑**（127 条自己就写着不是）。V1 的完成声明因此建立在
  「Worker/CI 运行记录 + 各任务的 G2/G4 验收」之上，而不是建立在本报告作者的一次性全量复跑之上。
  这是事实层面的限制，不是判定的瑕疵；写在这里是为了让读报告的人知道证据的成色。

**V1 后补法**：需要独立复跑的门，由 Supervisor 按任务包的 G2/G3 流程各自跑一次并落账；本报告不复跑（无服务栈、也无权改台账）。

### R3. 产品的 blob 写入路径缺失

> **这一条本版重新定性（T1214）：由「已记账的缺口 + 一条未获裁定的 L3」改成「范围外，不是欠账」。**
> 改的是**定性**，不是事实——下面那段实测的产物一个字不动：树里**没有**产品侧的 blob 写入路径。
> owner 2026-09-23 裁定 **L3-② =「不要」**：V1 不含面向用户的大文件上传入口
> （`tasks/decisions.md` **㊻** 第 1 节与 §3、`docs/02_V1_SCOPE.md` §4 第 2 条），落地提交 **`90167ba`**
> 把契约里那两条**从未实现**的端点删掉了（`specs/api/openapi.yaml`，`1 file changed, 20 deletions(-)`），
> `docs/15`/`17`/`23`/`27` 四份规格同笔对齐。**「缺口」这个词从这一条上撤掉了**：守着一个 V1 不打算
> 交付的能力，欠的是 V1.x 的设计，不是 V1 的账。**"缺口"→"范围外"这一处改的是这一条的性质，
> 不是它的事实**：把这条读成「已经建好了」同样是错的，树里今天仍然没有这一面。

- 出处：`tasks/decisions.md` ㉘，处置被 **㉟ 升级为待裁定的 L3-②**，**㊻ 已裁定「不要」**。
- 证据（以下命令均在本基准实跑，输出见行内）：
  - **契约今天不再声明那两条端点**——本版把这一句的正负号换了过来（原句写「契约**声明**了这两条
    端点」，那描述的是**裁定之前**的树）：`grep -n "request-upload" specs/api/openapi.yaml`
    **零命中、退出码 1**；`git diff a8850f51..HEAD -- specs/api/openapi.yaml` 逐字只有删除
    （`1 file changed, 20 deletions(-)`）。**「契约声明了它们」这句话在本版被扫干净了**——
    `grep -n "request-upload" tests/acceptance/v1-final-report.md` 在**报告主体**（不含 §6.0(e)
    里为留证而抄的那些引文——引文自己会被下一次 grep 数到）今天只剩 **5 行、5 处字样**
    （原始输出贴在 §6.0(e)），逐行读过：**三行是这一段在叙述这件事本身、两行是第 4 节那条复跑命令**，
    **没有一行**还在声称契约里声明着它们。**被扫掉的是两行断言**（原文的
    「契约**声明**了这两条端点……`:234` / `:242`」与第 4 节的「期望：234: …」），
    以及同一段里那句现在为假的「期望：234: …/…:finalize」。
  - 产品代码里从来没有实现：`grep -Rn "request-upload\|requestUpload" cmd/api internal` 输出为空。
  - `CreateBlob` 真正的调用点只有 `tests/integration/manifest_test.go:178`；`internal/persistence/sqlc/blobs.sql.go:42`
    是 sqlc 从 `internal/persistence/queries/blobs.sql:6` 生成的定义，`internal/persistence/sqlc/querier.go:121` 是生成的接口声明。
  - 把口径放宽到任何 `INSERT INTO blobs`：`grep -Rli "INSERT INTO blobs" --include='*.go' --include='*.sql' .`
    命中 17 处——`internal/persistence/queries/blobs.sql`（sqlc 源查询）、`internal/persistence/sqlc/blobs.sql.go`（生成物）、
    `tests/acceptance/seeddemo/blobs.go`（验收 seed 工具）、`tests/acceptance/portability/export.go`（注释）、
    `tests/e2e-release/harness/main.go:859`（**release e2e 的测试装置**）、以及 `tests/integration/*_test.go` 的 12 个文件。
    **产品代码（`cmd/**`、`internal/**`）里一条调用都没有**；上面清单里的"产品代码"只有 sqlc 的源查询与生成物——
    它们是被生成的存取层，不是任何一处**发起**写入的入口。
  - 生产侧唯一的 `PutObject` 在灾备包里：`cmd/api/backupdr/source.go:79`、`drill.go:603`（调用），
    `drill.go:129`、`s3.go:221`（注释）、`s3.go:222`（定义）。**没有一行在产品把内容放进 blob truth 的路上。**

**影响**：资产/发布链路今天只在**已存在的 blob 行**上被验证过；**新 blob 的创建没有产品入口**。
因此「可移植导出」的结论只能声明它导出的是已有内容。**这条影响在 V1 范围内不构成失分**——
V1 不交付用户上传入口（`L3-②`），所以「没有产品入口」正是裁定后的应有状态；它构成的是
**V1.x 的设计起点**：那套上传协议要有 TTL/GC，`docs/17_FILES_STORAGE.md` 今天没有回答
「**未被使用的上传文件多久清掉**」。

**V1.x 的前瞻（不是 V1 的欠账）**：`tasks/decisions.md` **㉟** 曾把这一条立为待裁定的 **L3-②**，
**㊻ 已裁定为「不要」**：V1 不含面向用户的上传入口，于是 `docs/15_AGENT_MCP_API.md`、
`docs/17_FILES_STORAGE.md`、`docs/23_SECURITY_PRIVACY.md`、`docs/27_PERFORMANCE_SLO.md` 里
那套「有协议、有安全要求、有 V1 量级目标」的上传描述随同笔（`90167ba`）一起被降级为 V1.x 设计输入，
契约里那两条从未实现的端点被删除。**所以这一条今天的状态是「范围外」，不是「已记账的缺口」**——
它没有任务承接，也**不需要**任务承接；要它重新变成欠账，得先有一条把上传纳入某个后续版本的产品裁定。
**V1.x 开工时要先答的问题**（本报告只列出，不替它答）：上传协议的形状、配额与 TTL/GC 语义、
以及未被使用的上传文件多久清掉。

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
    **此处注释与代码一致，没有缺陷**（更早一版这里曾写反，已更正）。
  - **行为缺陷**：`openProposal`（调用在 `:352`）因**非** `ErrIdempotencyKeyInUse` 失败时（该判断在 `:368`）
    会落进 `replayIfRecorded`（`:371`），读回**本次请求自己刚追加的那一版**（追加在 `:341` 的 `appendReopenVersion`，已成功），
    返回 `Replayed=true`（`resultFromRecorded`，`:918`）而 `PullRequestNumber=0`。**这条回退是有意的**——
    `replay` 的注释在 `:720-723` 逐字写着 'The first attempt committed the version and died before opening the
    proposal (the gap documented in this task's result). The replay answers with what is recorded rather than
    opening a PR on a branch the caller can no longer see the head of.'（`:748` 附近是 `replayIfRecorded` 的定义，
    不是这段注释——更早一版引错了行号，已更正）——但后果仍然成立：**一次真实的「提案没开成」被答成 201 的重放**，
    调用方拿到的是没有 PR 的成功。

**影响**：该缺陷今天**不可达**（没有 reopen 路由），所以不是当前可触发的 bug；
但只要将来补上 reopen 路由，这条缺陷会立刻暴露。

**谁承接：今天没有任何任务承接这一条。** `tasks/decisions.md:16971` 逐字记着「**A/F/G 至今没有承接者**」，
并更正了那句「T0909 里已逐条写明」。我把这个全称断言自己复跑了一遍：

```bash
# (a) 字面写 reopens 的 allowed_scope：0 / 155 笔任务
jq -r '.tasks[] | select(((.allowed_scope // []) | map(test("reopens")) | any)) | .id' tasks/tasks.json | wc -l
# 输出：0
# （上一版这里写 154：本基准比上一版多一笔任务定义，就是 T1213 自己；计数只作语境，判定看输出 0）
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

**本版更新（T1214）：这条今天不是"待接线"，是"按裁定不接线"。** `tasks/decisions.md` **㊻** 已裁定
**L3-⓪ = 不外发**（V1 不把平台内容送到平台之外的模型服务），所以「planner 无 provider、embedder 为 nil、
answer 只有 logger」这一组事实是**裁定后的应有状态**，与 R3 同属「范围外」：
T0902/T0903 的「端口 + 确定性假件 + 零联网」正是这条裁定的落地形状，`cmd/api/main.go:1295` 那段注释
自称的「the supported state the search packages document rather than a gap」在今天**成立**了。
**要它重新变成欠账，得先有另一条产品裁定**（允许外发）——在那之前 V1 的 search 保持结构化回退，
不接任何 provider、不引入任何外部凭证。

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
  503 的响应包一律带 `retryable=true`（`cmd/api/authhttp/envelope.go:86`：`Retryable: status == http.StatusServiceUnavailable`）。
  **这是有意的**：`service.go:116-125` 用一整段注释论证过「没有记录就不交答案」。
- **R7 的 code 名**：本基准上写记录失败有自己的 code `SEARCH_RECORD_FAILED`（泛化的 `SEARCH_UNAVAILABLE` 现在只覆盖「管线失败」）。

**影响**：一次 transient `search_records` 失败以可用性事故的形式表现；重试是新 search、新记录，语义正确但成本高。

**V1 后补法**：将写记录与返回答案解耦（异步 outbox 或返回后补写），并更新可用性预算。

### R8. L3-⓪ **已裁定**（不外发）——本项收口为「范围外」

> **本版更新（T1214）：标题从「未回答」改成「已裁定」。** owner 2026-09-23 裁定 **不外发**：
> V1 不把平台内容发送到平台之外的第三方服务（模型、向量、分析），只保留接口位置与确定性假件、不接线
> （`tasks/decisions.md` **㊻** 第 1 节那一行 + `docs/02_V1_SCOPE.md` §4 第 1 条 + `docs/55`）。
> **收口的意思是「主体不在 V1」，不是「验证过不外发」**——下面那段检索证据照实保留：它证明的是
> 「按那两条检索，`docs/**` 与 `specs/**` 里没有这个问题的答案」，那是**裁定之前**的取证；
> 裁定之后这条问题的答案是「不做」，不是「查过没问题」。

- 出处：`tasks/decisions.md` L3-⓪（2026-09-19 补记），**已由 ㊻ 裁定**。
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
而 L3-⓪ 里那句「要你答的一句话」**已在 2026-09-23 得到回答：不外发**。

**本版的状态**：**范围外，不是欠账。** 这条风险保留在这里，保留的是**取证过程**与**再评估的触发条件**：
如果哪天有任务要接模型服务，它必须同时带来（i）owner 的新裁定、（ii）新的外部凭证/付费授权，
而这两样都是 §5.1 的停止条件。**V1 不做这件事**，也不因为「做了也没人拦」而做。

### R9. L3-① **已裁定**（不改）——本项收口

> **本版更新（T1214）：标题从「未回答」改成「已裁定」。** owner 2026-09-23 裁定 **不改**：
> 搜索与 Research Answer 一律要求登录，匿名只可见公开页面本身
> （`tasks/decisions.md` **㊻** 第 1 节 + `docs/02_V1_SCOPE.md` §3 Open Network 那一句）。
> **产品今天的行为就是裁定后的行为**，所以这一条下的两个自洽读法之争（"规格疏漏" vs "维持现状"）
> 已经由 owner 收口为后者。

- 出处：`tasks/decisions.md` L3-①，**已由 ㊻ 裁定**。
- 证据：规格 `docs/05_INFORMATION_ARCHITECTURE.md:9` 把搜索入口列在未登录可见栏；
  契约 `specs/api/openapi.yaml:10-11` 全局 `security: - bearerAuth: []`，且 `POST /search` 没有 `security: []`。
  当前实现按契约走，要求登录。

**影响**：Gate D 的「Explore/Search 可发现 public project/asset/…」在**匿名 `/search`** 这一层未启用
（Explore 那一半匿名可用，见 Gate D 的 D2）。**裁定之后这不是失分**：匿名 `/search` 不在 V1 的
产品语义里，`docs/05_INFORMATION_ARCHITECTURE.md:9` 那句「未登录可见」的描述属于 V1.x 的输入。

**本版的状态**：**范围外，不是欠账**（`㊻` 裁定「不改」）。**本报告不为它接工**：
`docs/**` 与 `specs/api/**` 的措辞对齐属 L1/L2，由 Supervisor 在后续任务里按裁定处理；
产品代码一个字不改，因为产品今天的行为就是裁定后的行为。

### R10. 「三项缺席」的处置结果：两项成了实检行，第三项成了有守卫的缺席（书面接受在 ADR-028）

**这条风险在上一版里叫「SAST、容器扫描、SBOM 三项缺席」，是 Gate H 判「未通过」的直接理由。
那个句子在今天这棵树上不再成立，本版按树改写了它——不是把句子删掉，是把它换成一个可复跑的事实。**
（审计脚本里钉的那条标记也随之换成下面这两句，标记本身没有放松，只是换成了对新树的断言。）

- **SAST 与 SBOM 不再是缺席**：它们是总门里的**实检行**——
  `sast-go` / `sast-python` / `sast-node`（gosec v2.29.0、bandit 1.8.6、eslint 9.39.5 + eslint-plugin-security 4.0.1）
  与 `sbom-go` / `sbom-node` / `sbom-python`（cyclonedx-gomod v1.9.0、`pnpm sbom`、`uv export --format cyclonedx1.5`）
  加 `license-audit`。本基准上它们各自打印了自己的证据行（见 Gate H 第 4 段）。
- **容器扫描是唯一剩下的缺席，而且是一条有守卫的缺席**：`container-scan` 那一行在树里没有容器构建文件时**绿**
  （本基准上它走查了 **2113** 个文件并打印它所依赖的 tree claim；这个数随本地被忽略的构建产物浮动，
  上一个基准上先后跑出过 2110 与 2112、更早那次冷树跑出来是 2106——**判定只看那条 claim**），
  出现任何 `Dockerfile`/`Containerfile` 即**红**。
  它的书面风险接受是 `docs/adr/ADR-028-v1-ships-no-container-image.md`——按 `docs/23_SECURITY_PRIVACY.md:45`（§11）
  那句「Critical/High 必须修复或有书面 ADR/risk acceptance（V1 不接受 Critical）」，这份 ADR **就是**这一项上的
  那份书面接受，范围仅限「V1 交付不含任何镜像」，且**自我失效**（镜像一出现，守卫转红即失效信号）。
- **出处**：`ops/security/absent-checks.json`（`absent` 数组今天只有 `container-scan` 一条，`kind` 是
  `guarded-absence`，`guard_check_id` 指向总门里那条 `container-scan` 行，`risk_accepted_in` 指向 ADR-028）；
  `ops/runbook-steps.json:147` 的 `kind: absent`（`"kind": "absent"` 在 `:147`，那段 note 在 `:149`）。
- **一处笔记过期（不是缺口）**：`ops/runbook-steps.json:149` 那条 note 仍写着「there is no dependency/CVE scan
  and CI has no security job」，两半今天都不成立（T1206 补了 CVE 扫描；第 11 个必需作业 `security-master` 已进 CI）。
  本报告不改 `ops/`，只点名，供 Supervisor 复核。

**残余风险（照实写，不修辞）**：真正在跑的上游基础设施镜像（`docker-compose.yml` 拉的 pgvector 0.8.6-pg16、
redis 7.4.11-alpine、minio、gitea 1.27.3、mailpit v1.31.1）**按 tag 钉住、无人扫描**——它们里面的 CVE
只能靠读 release notes 发现，不由任何门发现。今天没有任何交付物是镜像，所以这个暴露面是**基础设施的**
而不是 V1 交付物的；`docs/adr/ADR-028-*.md` 的 Consequences 第 3 条与 `tests/security/key-components.json`
把它的位置与价钱都记下来了（许可证那一半已由 `license-audit` 行每次跑门打印并要求书面裁决）。

**V1 后补法**：先有镜像（每应用一个 Dockerfile + CI 的 build 阶段，docs/25 第 10 条），再有扫描器
（trivy/grype image mode，High/Critical 即红 + ignore 文件），**并且那条守卫行必须被扫描器取代，而不是与它一起删掉**
（它是树里唯一说得出「现在是哪一种状态」的东西）。ADR-028 逐字写着这一点。

> 一句话总结这条的**判定影响**：Gate H 的「未通过」在上一版只有这一个理由；这个理由的每一块都在本基准上
> 有可复跑的替代物（实检行的证据行、守卫行的证据行、书面接受本身），所以 Gate H 在这一版判「通过」，
> 而 H1 那一行的判定带上限定（扫描面之外仍有这一条）。

### R11. `rddev worker collect` 的证据没说清它评了什么

- 出处：T0906 复核第 3 条，`.rddev/workers/T0906-review/RESULT.json` finding 3。
- 证据：那次 `diff.txt` 声明基线 `3e479507`，但逐字节等于 `git diff 3e2c43d 7db633f`（主线 tip → 分支 tip，44 个文件）；
  `collect-report.json` 的 head-baseline/branch-ref 与 `files_changed` 都是上一轮返工的增量。
  内容本身干净（那棵树是干净的、44 个文件都被评过），但 **G4 本应能从自己的记录里回答「评了什么」**。

**影响**：这是**工具**的欠账，不是产品的；它让合并门的记录无法自证审查范围。

**V1 后补法**：修复 `rddev collect` 的基线选择（用实际 merge base 或 main tip）并在 collect 时重新捕获 HEAD/ref/files_changed。

### R12. MCP 工具面未接线（L3-③ **已裁定：V1 不含**）——Gate G 由「未通过」改判为按范围豁免的直接理由

- 出处：`tasks/decisions.md` **㊱**（2026-09-23）立为待裁定，**㊻ 同日记下裁定**；复核起点是
  `.rddev/workers/T1207-review/RESULT.json`。
- 事实（本基准实跑，命令与输出见第 2 节 Gate G）：
  1. `specs/mcp/tools.json` 声明 **21 条**工具，全树 **0 个 dispatch 点**
     （`/tmp/t1214-cgate -root . -write-mcp …` 重建出同一结论；已合并的 `ops/contract/mcp-inventory.json` 逐字节同结论）；
  2. `cmd/mcp-server/main.go:77-80` 的 `/mcp` 按设计答 **501**，正文
     `{"error":"MCP protocol wiring not implemented yet (agent tasks)"}`；
  3. **规格在本版已经反过来了**（这是本版与上一版之间唯一一处实质差别）：`docs/02_V1_SCOPE.md:54-55`
     与 §4 的 `:73-74` 都把 MCP / Agent 工具面写成 **V1 不含**，`docs/56_REQUIREMENTS_TRACEABILITY.md:16`
     那一行是「无 —— **V1 不含**（`docs/02` §4，`L3-③`）」，`specs/mcp/tools.json:3` 带上
     `"scope": "post-v1"`，`docs/47_MCP_TOOL_CATALOG.md:3-5` 挂上同义横幅。**上一版那句
     「`docs/02:52` 把它列进 V1 必做、§4 没有豁免它」在今天的树上是假的了**，本版把它换成了这一条。
- **性质（本版改判）：范围外，不是欠账，也不是待裁定的 L3。** owner 2026-09-23 的裁定是
  **「V1 不含 MCP / Agent 工具面」**——`specs/mcp/tools.json` 的 21 条工具与 `docs/47` 是 **V1.x 的设计输入**，
  V1 不交付、不接线（`tasks/decisions.md` **㊻**、落地提交 `90167ba`）。
  上一版在这里写的是「把 21 条工具建起来要先有产品语义」——**那句话今天仍然成立，但它已经不构成 V1 的欠账**，
  因为它描述的是 V1.x 开工时要补的设计输入，不是 V1 没做完的活。
- **Gate G 第 1 条因此按范围豁免处置**（四条 checkbox 的逐条处置见第 2 节 Gate G）：
  **豁免的是主体，不是判据**——`docs/31_MASTER_ACCEPTANCE.md:45-49` 一个字节没动，
  21 条工具没有删、`/mcp` 的 501 没有改、`ops/contract/mcp-inventory.json` 没有重建。
- **V1 后补法**：**不需要**——除非另有一条把工具面纳回某个后续版本的产品裁定。真要建的时候，
  起点是现成的：`specs/mcp/tools.json` 的 21 条定义与 `docs/47` 都还在，V1.x 从一份被评审过的面开始，
  而不是从空白页开始（`90167ba` 的 `scope_note` 逐字写着这一点）。

### R13. OpenAPI 契约与挂载路由的对账是红的，而且没有任何东西在跑它

- 出处：T1205 的交付（`ops/contract/route-inventory.json`、`tests/cmd/contractgate`）与其 RESULT 摘要。
- 证据（本基准实跑；**直接跑二进制才看得到它自己的退出码 3**，`go run` 会包一层让 shell 看到 1，两种跑法都试过）：
  ```bash
  go build -o /tmp/t1214-cgate ./tests/cmd/contractgate
  /tmp/t1214-cgate -root . | head -2
  # 输出：contractgate: . (server prefix /api/v1, contract specs/api/openapi.yaml, exemptions specs/api/openapi-exemptions.yaml)
  #       summary: mounted=134 in_contract=30 catchall_resolved=6 exempted=0 undocumented=98 | contract_ops=38 unmounted=1 | exemptions=0 cited=0 | files_scanned=736
  /tmp/t1214-cgate -root . >/dev/null 2>&1; echo "exit=$?"
  # 输出：99 finding(s). Exit 3.  →  exit=3
  ```
- **这一版为什么两个计数动了**（照实写，只报读数与那一处契约改动，**不追述哪一个 finding 落到哪一条**）：
  `contract_ops` **40 → 38**、`unmounted` **3 → 1**、findings **101 → 99**，
  而 `a8850f51..HEAD` 之间唯一的契约改动就是 `specs/api/openapi.yaml` 的 `20 --------------------`
  —— 那是 `90167ba` 按 `L3-②` 删掉的两条从未实现的 blob 端点。**数量上一致（各减 2）**，
  本报告不去证明逐条对应。剩下的那一条 unmounted，本版实测逐字是：
  `GET /projects/{projectId}/branches`（`declared in specs/api/openapi.yaml … and no route in cmd/** or internal/** registers it`）。
- **红的性质**：这不是「刚变红」。T1205 的 RESULT 摘要逐字写着
  `PINNED RED at merge: mounted=134 in_contract=30 catchall_resolved=6 exempted=0 undocumented=98 | contract_ops=40 unmounted=3 | exemptions=0 cited=0 files_scanned=728 | advisory=41`
  ——**这条红是带着已知状态合并的**（当时的 `files_scanned=728`；上一版基准 735、本基准 736：**这个数只跟着被扫的文件总数走**，
  `undocumented=98` 从合并那天到今天一次没变；`contract_ops`/`unmounted` 两处这一版因为契约删了两条端点而各减 2，见上一条）。
- **而且没有人在跑它**：`.github/workflows/`、`Makefile`、`scripts/` 里对 `contractgate` 的引用是 **0 处**
  （`grep -rn contractgate .github/workflows/ Makefile scripts/` 零命中）。`specs/api/openapi-exemptions.yaml`
  存在，但对账自己的读数是 `exemptions=0 cited=0`——**一份没人用的豁免文件**。
- **性质第二层**：这是一条**已知红、且无人跑的对账仪**，不是"新发现的缺陷"。本报告只**点名**它
  （`tasks/decisions.md` ㊲ 把它的收尾记为 V1 之后立账的 **T1211**：98 条 undocumented 的证据、
  `openapi-exemptions.yaml` 的裁定、3 条 unmounted、以及 contractgate 要不要接进 CI——**接 CI 会让 main 变红**，
  必须与那 98 条的证据一起动）。**该不该收进 CI 是 Supervisor 的裁定，本报告不自己下结论。**

### R14. L3-④ **已裁定**（保持现状）——「只有知道编号才可达」是产品决定，不是缺口

> **本版更新（T1214）：标题从「未回答」改成「已裁定」。** owner 2026-09-23 裁定 **保持现状**：
> attestation 只有知道编号 `pid` 才可达，不提供列表、搜索或导航入口；**「只有知道编号才可达」
> 是产品决定、不是缺口**（`tasks/decisions.md` **㊻** 第 1 节 + `docs/02_V1_SCOPE.md` §4 第 4 条 +
> `docs/12_PERMISSIONS_RIGHTS_POLICY.md` §2）。**收口的是那条二选一，不是下面的事实**——
> 「今天入口为零」仍然是本基准的实测结果，仍然逐字保留在下面。

- 出处：`tasks/decisions.md` **㊳**（2026-09-23 补记；那条决定**不在** `tasks/progress.md` 的 L3 清单里，
  是清点待办时才补上编号的——这正是本版把它写进报告的理由），**已由 ㊻ 裁定**。
- 事实（㊳ 里逐条实跑核对过）：路由与页面都在（`apps/web/app/(main)/attestations/[pid]/page.tsx`），
  但**入口为零**——`attestationHref` 定义在 `apps/web/lib/attestations.ts:237`，**产品侧的**调用者只有那一页自己
  （`page.tsx:72` 的 canonical 链接、`:131` 的「Reload this page」自链；`apps/web/lib/attestations.test.mjs:84/88/89`
  也调它，那是单测，不是入口）。**今天只有"知道 pid"的人能打开它。**
- **性质（裁定后）**：CLAUDE.md §5.1 把**公开性**列进必须停下的情形，所以这一条**不能由工程侧自己选**——
  两种读法都自洽（① 声明是给"拿到 pid 的第三方"核验的，不需要发现面；② 一份没人能发现的公开声明不服务于它的目的），
  在两者之间替 owner 选一条就是在发明产品语义。**owner 选了 ①**（保持现状）：于是"入口为零"从
  「一条待裁定的 L3 下的悬置状态」变成「**产品决定下的应有形状**」。**它不是工程欠账，它不挡 V1 的任何一环**，
  这一句在裁定前后都成立。
- 契约那一半**不是 L3**：`specs/api/openapi.yaml` 里 `attestation` 零命中，三条路由在契约对账里是
  `undocumented`——属于 R13 那 98 条的清单，收尾在 T1211。
- **V1 后补法：不需要**（`㊻` 裁定「保持现状」）。要重新开这一条，得先有一条相反的产品裁定；
  真开的时候按 ㊳ 已经写明的形状做（只做「目标版本页列出针对它的 attestation 链接」，不新增任何发现面），
  并与 `specs/api/**` 的登记同笔。**本报告不接工、不改产品代码。**

---

## 4. 关键复跑命令清单

```bash
# 1. 当前基准
git rev-parse HEAD
# 输出：91ed2d5fdb24c7a0b0f332d6861fb5ffde5eb86f

# 2. 审计脚本（本任务的 final audit）：报告标记、数字、名单、计数块、基准 commit 全对才 exit 0
bash tests/acceptance/v1-final-audit.sh
# 输出末行：AUDIT OK: report markers present, counts match (0 unmerged, 1 not_run, 0 failed, 0 skipped, 0 passed-without-evidence).
# （本版实测退出码 0。更早那一版那次是 178 `passed` / 3 `not_run`，措辞与数字都跟着台账走，
#  所以这一行逐字与更早几版不同——**不是断言变了，是台账长了 4 条 `passed`**。）
# 这次运行同时把**计数块原样打印出来**（COUNTS v1_required_total=150 … COUNTS tests_blocking=183，
# 版本与台账逐字相等）：脚本一边打印它、一边校验报告里那 9 行与它逐字相等——
# 「打印出来的」与「被校验的」是同一份东西，读输出的人不必去读脚本。
#
# 2b. 这条命令是**基准快照**：它只在报告自己的基准上有意义。HEAD 前进之后（合并、落账、
#     任何一次提交）它必然以非 0 退出，并打印「证书过期」的说明与两条出路——那不是报告错了，
#     是报告记录的那棵树不是这一棵。要么在那个 SHA 上跑，要么先重新生成报告：
#     两条出路里，出路 (a) 指回的是**报告自己钉的那个 SHA**（不是当前 HEAD）——本版修了这一点，
#     在那之前它会把「去哪棵树核对」说反（复核 minor，证据：拿一份钉在旧基准上的报告跑，
#     它打印的是 `git checkout <报告里的 SHA>`）。
bash tests/acceptance/v1-final-audit.sh --emit-tables
# 输出：生成注释行 + 「> 生成基准：`<当前 HEAD>`」+ 未合并任务表 + blocking not_run 表 + 计数块。
# 把生成基准行贴回报告头部、两张表贴回第 1.2 / 1.3 节、计数块贴回第 1.3 节
# （判定与证据由人按台账改，数字只许来自脚本），再造新基准的证书。
# 本版第 1.2 / 1.3 节那张表与基准行**逐字**来自本基准上一次 `--emit-tables` 的**同一次输出**
# （生成注释行里的基准就是 `91ed2d5…`，见第 6 节）。

# 3. blocking not_run 测试
jq '.tests | map(select(.blocking == true and .status == "not_run")) | {count: length, ids: map(.id), names: map(.name)}' tasks/tests.json
# 输出：{"count":1,"ids":["T1214-TEST-01"],"names":["certificate final revision"]}
jq -c '[.tests[]] | group_by(.status) | map({status: .[0].status, count: length})' tasks/tests.json
# 输出：[{"status":"not_run","count":1},{"status":"passed","count":182}]
jq '[.tests[] | select(.status=="failed")] | length' tasks/tests.json
# 输出：0        （skipped 同：jq '[.tests[] | select(.status=="skipped")] | length' → 0）
jq '[.tests[] | select(.status=="passed") | select((((.evidence // "") | gsub("[[:space:]]"; "")) | length) == 0)] | length' tasks/tests.json
# 输出：0        （空 evidence 的 passed 一条都没有——与审计脚本第 3 节那条断言逐字同源）

# 4. Gate I / MOF canonical workflow（本报告在 91ed2d5 上实跑：2026-09-23 19:25:59 → 19:26:41，退出码 0，50 步全绿，FAIL 0 条）
#    它需要整套真实栈（PostgreSQL/Redis/MinIO/Gitea + 真 next start + 真 Chromium），
#    起栈的命令与脚本自己的前置在 docs/66 与 tests/acceptance/mof-canonical-workflow.sh 的头注释里。
bash tests/acceptance/mof-canonical-workflow.sh
# 输出末两行：G3 mof-canonical: PASSED — 50 step(s), every assertion green
#              the seeded world, left in place for inspection: post_mof_demo_canonical_gate
# 完整输出留在 /tmp/t1214-mof-out.txt（pid 每次播种都新生成：本次是 wwgh8wqztadda713d84yhfgc9r）。

# 5. Gate H / 依赖与 CVE 审计 + 总安全门（本报告在 91ed2d5 上实跑：2026-09-23 19:27:32 → 19:28:07）
make security-tools    # 冷树上先装齐前置；不装的话总门会以退出码 2 报 5 行 NOT ASKED
bash tests/security/master-security-gate.sh --report /tmp/t1214-master-gate-report.json
# 输出末行：master-security-gate: PASS — 17 check(s) ran and each printed its own evidence (report: /tmp/t1214-master-gate-report.json)
# 退出码 0；注册表下限 MIN_CHECKS=17（tests/security/master-security-gate.sh:140——T1211 在它上面加了 20 行，
# 上一版引的 :120 在那棵树上是对的，这一版实测是 :140）
bash tests/security/master-security-gate.sh --only absence-manifest
# 输出：那三行 ok   item: container-scan — witness …（有守卫的缺席），见 Gate H 第 3 段

# 5b. Gate H / CI 必需作业集合：权威是这三个文件本身，四处必须是同一个集合
python3 - <<'PY'
import re, json
jobs = set(re.findall(r'^  ([a-z0-9_-]+):$', open('.github/workflows/ci.yml').read().split('\njobs:\n',1)[1], re.M))
g = json.load(open('specs/orchestrator/gates.json'))
print(len(jobs), sorted(jobs))
print(jobs == set(g['required_jobs']) == set(g['gates']['G2']['runs_jobs']) == set(g['gates']['G4']['asserts_jobs']))
PY
# 输出：11 ['a11y', 'acceptance', 'go', 'i18n', 'migration-integration', 'observability', 'python', 'security-master', 'spec-validation', 'task-state', 'web']
#       True

# 5c. Gate G / MCP 工具面对账（本报告在 91ed2d5 实测：21 声明的、0 个 dispatch 点）
go build -o /tmp/t1214-cgate ./tests/cmd/contractgate
/tmp/t1214-cgate -root . -write-mcp /tmp/t1214-mcp-inventory.json | head -2
# 输出：MCP reconciliation: catalog specs/mcp/tools.json declares 21 tool(s); 0 have a dispatch site; 21 do not.
jq '{declared_tools, tools_with_a_dispatch_site}' /tmp/t1214-mcp-inventory.json
# 输出：{"declared_tools":21,"tools_with_a_dispatch_site":0}
sed -n '77,80p' cmd/mcp-server/main.go
# 输出：/mcp 的 501 存根与正文 `{"error":"MCP protocol wiring not implemented yet (agent tasks)"}`
sed -n '54,55p;73,74p' docs/02_V1_SCOPE.md
# 输出：- MCP / Agent 工具面 **V1 不含**：… / - **MCP / Agent 工具面**：…（§3 与 §4 都写了「V1 不含」）
sed -n '16p' docs/56_REQUIREMENTS_TRACEABILITY.md
# 输出：| MCP 工具面 | 15/47 | 无 —— **V1 不含**（`docs/02` §4，`L3-③`） | — |
sed -n '3p' specs/mcp/tools.json
# 输出：  "scope": "post-v1",

# 5d. Gate G / 契约对账（R13：已知红、且无人跑它；退出码 3 是程序自己的）
/tmp/t1214-cgate -root . | head -2
# 输出：contractgate: . (server prefix /api/v1, contract specs/api/openapi.yaml, exemptions specs/api/openapi-exemptions.yaml)
#       summary: mounted=134 in_contract=30 catchall_resolved=6 exempted=0 undocumented=98 | contract_ops=38 unmounted=1 | exemptions=0 cited=0 | files_scanned=736
# （上一版这里写 contract_ops=40 unmounted=3：本版契约删了那两条 blob 端点，两个计数各减 2；
#  `undocumented=98` 与其余数字逐字未变——R13 那 98 条的名字在报告里仍逐条列着，见 R13。）
/tmp/t1214-cgate -root . >/dev/null 2>&1; echo "exit=$?"
# 输出：99 finding(s). Exit 3.  →  exit=3
grep -rn contractgate .github/workflows/ Makefile scripts/ || echo "0 callers"
# 输出：0 callers

# 6. 已知缺口核对（本基准实测的输出写在注释里）
# blob 写入路径：契约不再声明那两条端点（`L3-②` 裁定「不要」，90167ba 删掉）
grep -n "request-upload" specs/api/openapi.yaml || echo "zero hits in specs/api/openapi.yaml"
# 期望：zero hits in specs/api/openapi.yaml（契约里没有这两条端点；产品里也从来没有实现）
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
jq '[.tests[] | select(.status=="passed") | select((((.evidence // "") | gsub("[[:space:]]"; "")) | length) == 0)] | length'
# 期望：0（没有一条 passed 是空证据；审计脚本第 3 节把这一条也钉住了，见第 3 节分布的第二/第三道。
#         **这条命令本版换成与脚本逐字相同的写法**——原先是 `select(((.evidence // "") | length) == 0)`，
#         一条 evidence 全是空白的 passed 会让脚本红、却让照报告复跑的人读到 0；改的是报告印的命令，
#         脚本那条断言一个字没动，见第 6 节 6.6 的 nit⑤）

# 8. 谁承接：reopen 的 A/F/G 今天没有任何任务书认领（R4 的全称断言）
jq -r '.tasks[] | select(((.allowed_scope // []) | map(test("reopens")) | any)) | .id' tasks/tasks.json | wc -l
# 期望：0（156 笔任务里没有一笔的字面 scope 写 reopens；上一版这里写 155，本基准多了 T1214 自己那一笔）
jq -s -r '.[0].tasks as $d | .[1].tasks as $s | $d[] | .id as $id | select(((.allowed_scope // []) | map(test("internal/application")) | any)) | select(($s[$id].status // "missing") != "merged") | "\($id) \($s[$id].status)"' tasks/tasks.json tasks/task_status.json
# 期望：空

# 9. Gate C 资产那一半的状态（任务与测试分开取，别把「已合并」读成「已通过」）
jq -r '.tasks | to_entries[] | select(.key|test("^T070[689]$|^T0710$")) | "\(.key) \(.value.status)"' tasks/task_status.json | sort
# 期望：T0706 merged / T0708 merged / T0709 merged / T0710 merged
jq '[.tests[] | select(.id|test("^T07(0[1-9]|1[0-2])-TEST-01$")) | select(.status!="passed")] | length'
# 期望：0（整个资产族 T0701–T0712 没有一条不是 passed）

# 10. 回填成色（R2 的数字）：账本里有多少条 evidence 自述是回填
jq '[.tests[] | select((.evidence // "") | test("Backfilled|补跑|backfilled"))] | length' tasks/tests.json
# 输出：140（183 条里，140 条的 evidence 写明是回填的；其中 127 条逐字写着 not a fresh re-run）

# 11. L3 的编号集合：报告第 0 节点名的 L3 与台账记的 L3 必须相等
#     （审计脚本自己做这条比对；这条命令是它的可复跑版本）
grep -oE 'L3-[⓪①②③④⑤⑥⑦⑧⑨]' tasks/decisions.md | sort -u
# 输出：L3-⓪ L3-① L3-② L3-③ L3-④（五条；少一条就说明有未登记的裁定项）
```

---

## 5. 验收与交付声明

### 5.1 `CLAUDE.md` §12 的四个完成条件，逐条对上

`CLAUDE.md` §12 把「完成」定义成**四条同时成立**，不是一个「所有 task 显示 done」：

| # | §12 的条件 | 本报告里的回答 | 结论 |
|---|-------------|----------------|------|
| ① | 所有 V1-required task merged | 第 1 节（审计脚本实测：**150** 笔 `v1_required`，排除 T1207 后未 merged **0** 笔） | 满足 |
| ② | **四层 Gate 通过** | **第 5.2 节**（逐层给权威与「绿记在哪」） | 满足（逐层见下） |
| ③ | Master Acceptance 通过 | 第 2 节（docs/31 的 Gate A–I 与 Development System Gate **逐条**判定：**10 条 Gate 全通过、`未通过 = 0`**） | 满足（**Gate G 按范围豁免**、Gate H 带一条限定，两处逐条写在第 2 节） |
| ④ | MOF canonical workflow 从 Research Question 到 external contribution 全闭环通过，并有可复现证据 | Gate I（**本报告在 `91ed2d5` 上实跑**：50 步全绿、退出码 0、`FAIL` 0 条） | 满足 |

**四条的执行条件在本基准上全部成立。** 这仍然是**本报告能给出的最强结论**，而不是 V1 的完成宣告——
理由与本报告每一版都写过的那一条相同：**这份报告是某一棵树上的快照**（`91ed2d5`），
它的每一条判定都只对那棵树有效；下一个提交落下去，它就必须按第 4 节第 2b 条的规矩重新生成。
另外 ③ 的「满足」里有两处**限定**（Gate H 的扫描面之外有一条有守卫的缺席；Gate G 第 2 条的「主体不在 V1」），
限定写在判定表里，不藏在这一句概括里。

### 5.2 「四层 Gate 通过」：逐层的权威与「绿记在哪」

第 5.1 节 ② 那一句在更早一版只散落在 S6、H5、R2 与 Gate I 四处；这里把它自己的块补上。
**四层的权威是 `specs/orchestrator/gates.json` 本身**（配合 `docs/67_TEST_GATES.md` 的定义），
每层「绿」都有一条可读的记录——本报告不自己跑 `rddev`（本报告作者没有 Git control 权限），
它给的是**去哪读那条绿**，外加本报告自己那一层（T1214 的 G3 = `mof-canonical`）的实跑。

| 层 | 权威（定义在哪） | 谁跑它 / 怎么红的 | 绿记在哪（路径 + 记录里的字段） |
|----|------------------|-------------------|----------------------------------|
| **G1** Worker Local | `docs/67_TEST_GATES.md` §G1；`specs/orchestrator/gates.json#gates.G1` | `rddev worker collect`：RESULT 内部一致性 + worker 边界。`completed` 却带 `not_run` / `failed`、带 INTERIM 标记、或 RESULT 本身 failed/blocked，**即拒** | `.rddev/runtime/gates/<TASK>/collect-run-*.json`，`"record_type": "collect"` 且 `"status": "ok"`。例：`…/T1209/collect-run-5521413a29796199.json` |
| **G2** Supervisor Acceptance | `gates.json#gates.G2` 的 `runs_jobs`（**11** 个作业，与 `.github/workflows/ci.yml` 的**同一步骤**逐字一致，不是"长得像的子集"） | `rddev task accept`：本地重跑 CI 的 exact steps；任一作业红即拒绝 `verification → accepted` | `.rddev/runtime/gates/<TASK>/gate-run-*-g2.json`，`"gate": "G2"` 且 `"status": "passed"`；随之的 `.rddev/runtime/gates/<TASK>/accept-run-*.json` 里 `"status": "accepted"`。例：`…/T1209/gate-run-run-61790e741add423c-g2.json`、`accept-run-61790e741add423c.json` |
| **G3** Integration / E2E | `docs/67` §G3；`gates.json#gates.G3` + `task_overrides.<TASK>.g3_jobs`（**没有条目 = `not_required`，记下来，不静默跳过**） | `rddev gate run … g3`：起真服务（PostgreSQL / Gitea / MinIO / Redis / 真浏览器）跑那个作业 | `.rddev/runtime/gates/<TASK>/gate-run-*-g3.json`，`"gate": "G3"` 且 `"status": "passed"`。**本任务（T1214）的 G3 就是 `mof-canonical`**（`task_overrides.T1214.g3_jobs`），本报告已在**这棵树**上跑过一次（Gate I：50 步全绿） |
| **G4** Merge | `gates.json#gates.G4` 的 `asserts_jobs`（同一个 **11** 作业集合）+ `docs/67` §G4 | `rddev pr merge`：断言该任务最新 G2 记录里**每一个必需作业都绿**，且那条记录晚于 collect；红或缺一个即拒绝合并 | `.rddev/runtime/gates/<TASK>/git-pr-merge-run-*.json`，`"action": "pr-merge"` 且 `"gate_status": "passed"`。例：`…/T1209/git-pr-merge-run-7fbf9ed13b28377e.json` |

三点要写明白：

- **G2 与 CI 的相等不是"人保证的"**：`gates.json` 与 `ci.yml` 的作业集合由一条单测钉着，漂了就红；
  本报告第 4 节 5b 的脚本是那条关系的可复跑版本（四处集合相等，`True`）。
- **G3 的"没要求"不等于"跳过"**：`task_overrides` 里没有条目的任务在 `gates.json` 的注释里写明是
  `not_required` 并且会**被记录下来**；本报告的 Gate I 之所以能拿 T1202 的记录做旁证、又能在本树上再挣一次，
  就是因为 T1214 自己带着 `mof-canonical` 这条 G3 作业。
- **`.rddev/**` 是本地运行记录，不是提交物**（`.gitignore:10` 忽略整个目录）。引用它是在说「这条绿当时记在哪」，
  不是在说它进了版本库。

### 5.3 判定与统计（与第 2 节的表逐格一致）

- 本报告已覆盖 `docs/31_MASTER_ACCEPTANCE.md` Gate A–I（`:5-60`）与 Development System Gate（`:62-72`）共 10 条，
  每条都有判定与证据；**逐条判定共 46 项**——`- [ ]` checkbox **38 条**（Gate A 7 / B 4 / C 4 / D 4 / E 4 /
  F 5 / G 4 / H 6，`grep -c '^- \[ \]' docs/31_MASTER_ACCEPTANCE.md` = 38）+ Gate I 的那**一句** +
  Development System Gate 的 **7 条**（38 + 1 + 7 = 46；后两项在 `docs/31` 里不是 checkbox，本报告把它们
  一并逐条判定，但不把它们的条数算进 checkbox 那一列）。每项一个判定：
  **45 项「通过」（其中 H1 那一项带一条限定：扫描面之外仍有一条有守卫的缺席；Gate G 第 2 条那一项带一条限定：
  保护在已交付面上成立、主体不在 V1）、1 项「范围豁免」（Gate G 第 1 条，主体被 owner 的裁定移出 V1 范围）**。
  Gate 层面：**10 条「通过」、0 条「未通过」**，最小判定单位在表里，不在这一句里。
  **这两个数字是脚本从报告自己的处置行里数出来的**（`tests/acceptance/v1-final-audit.sh:420-457`）：
  它抓 `^\*\*处置\*\*：` 行（本版 10 条）、数其中以「未通过」开头的（本版 0 条），
  再拿这两个数拼出上面那句话去比对——**手写一个数字就会红**。本版是首次出现 `未通过 = 0` 的一版，
  所以脚本里那句**不再带任何括号里的门名**（`（Gate G）` 那种写死的东西在 `未通过 = 0` 时会自相矛盾：
  同一句里既说「0 条未通过」又点名一个门未通过）。这一处的双向证明见第 6 节 6.0(c)。
- **这一版的统计与上一版差在哪，以及为什么**：上一版是 `9 通过 / 1 未通过（Gate G）`；本版是 `10 / 0`。
  差的那一条是 **Gate G**，而改判的理由**不是断言松了，是主体被移出了范围**：
  上一版判它「未通过」的唯一失分项是第 1 条「MCP semantic tools 完整且权限正确」，它挂的是一条
  **待 owner 裁定的 L3**（`L3-③`，V1 含不含 MCP 工具面）。owner 2026-09-23 裁定 **V1 不含**
  （`tasks/decisions.md` ㊻、落地提交 `90167ba`、`docs/02` §3/§4、`docs/56` 那一行、
  `specs/mcp/tools.json` 的 `"scope": "post-v1"`、`docs/47` 横幅），于是这一条从「未通过」变成「范围豁免」。
  **`docs/31_MASTER_ACCEPTANCE.md` 的判据一个字节没动**（`git diff --stat HEAD -- docs/` 是空的），
  21 条工具没删、`/mcp` 的 501 没改。**这三件事一起说才完整**：判据没松、主体移出、树里今天仍然没有这一面。
- **判定为「通过」但带缺口的每一条**都在剩余风险里逐条出现：Gate A/C/D/E/F/H 与 Gate G 第 2 条在 R1–R10、R12、R14 中；
  R11 是工具的欠账、R13 是仪器欠账（点名，不自行处置）。**本版 `未通过 = 0`，所以没有「未通过 ← Rn」这一项**；
  上一版那一项是 `Gate G ← R12`，那条风险**没有消失，只是换了性质**（R12 的标题与内容见第 3 节）。
- `tasks/tests.json` 中 182 条 `passed`、1 条 `not_run`（`T1214-TEST-01` 本报告自己的审计条目；
  上一版钉的 `T1213-TEST-01` 已随那笔任务合并而落成 `passed`，见 `tasks/decisions.md` ㊺ §6），
  **0 条 `failed`、0 条 `skipped`**；
  **本报告未修改任何 `status`**，账本只由 `scripts/record_test_run.py` / `scripts/reconcile_tests_ledger.py` 写真跑结果。
- 本任务（T1214）的 `allowed_scope` 内产物为 `tests/acceptance/v1-final-report.md` 与
  `tests/acceptance/v1-final-audit.sh`（后者含 `--emit-tables` 生成模式）；未触碰 `tasks/task_status.json`、
  `tasks/progress.md`、`tasks/**`、`specs/**`、`docs/**` 与任何产品代码
  （本版 `git status --porcelain -uall` 只有这两个文件）。
- **本版（T1210，第六轮 / 证书刷新轮）相对上一版（基准 `9b22339e`）改了什么**：
  ① 基准换成 `b6c1fb1a`，第 1.2 / 1.3 节的两张表、基准行、计数块**逐字**来自
     `bash tests/acceptance/v1-final-audit.sh --emit-tables` 的同一次输出（四个数字全部按树重取：
     150 笔 v1_required、181 条测试、178 `passed`、3 `not_run`）；
  ② **R10 重写**：旧句「SAST、容器扫描、SBOM 三项缺席」在新树上不成立，换成「两项成了总门里的实检行、
     第三项成了有守卫的缺席（书面接受 `docs/adr/ADR-028-*.md`）」，并补上前一版漏掉的两句文档滞后更正；
  ③ **Gate H 由「未通过」改判为「通过」**（唯一理由已由 T1209 的落地取代：总门实跑退出码 0、`MIN_CHECKS=17`、
     `absence-manifest` 三行 witness 逐字引在第 2 节），且 **H1 那一行的判定带上限定**；§5.3 的统计随之一致重算；
  ④ **Gate I 不再引用 T1202 的记录**：本报告在**承载这份证书的那棵树**上实跑了一次 MOF canonical workflow
     （50 步全绿、退出码 0），因为 T1210 的 G3 作业就是 `mof-canonical`；
  ⑤ §0 的 L3 清单补上第 ④ 条（`tasks/decisions.md` ㊳ 的漏登记），并新增 **R14** 点名它；
  ⑥ 新增 **5.1 / 5.2 两节**：把 `CLAUDE.md` §12 的四个完成条件逐条对上，并给「四层 Gate 通过」它自己的块
     （逐层的权威与「绿记在哪」，到 `.rddev/runtime/gates/` 的字段一级）；
  ⑦ 审计脚本：**三处**只取第一处匹配的数字检查**收成集合唯一**（4d：登记数那两句、台账分布那一句、
     以及**基准行本身**——一份在头部写对基准、却在正文里留一条旧 SHA 的报告现在也会红）、
     新增「空 evidence 的 `passed` 必须为 0」的台账断言（4e）、R10/H1/总门那几处标记换成能承载新事实的措辞、
     风险编号钉到 R14、§0 的 L3 条数改成**脚本从两个文件数出来**再比对（不是手写）、
     §5.3 的门层统计「9 通过 / 1 未通过」也改成**数报告自己的处置行**再比对（判定与统计必须一致，4b）、
     并且普通运行与 `--emit-tables` 走**同一个**计数块函数——前者打印、后者供重建，读到的就是被校验的那一份。
- **本版（T1210，第七轮 / 返工轮）相对上一版（基准 `b6c1fb1a`）改了什么**：
  ① **基准换成 `115a4286`**，理由在报告头部：上一版在验收时红的是 CI `security-master` 作业的**安装步**
     （无条件 `sudo apt-get` 在没有 tty 的机器上退出码 1，总门那一行根本没轮到），修在
     `.github/workflows/ci.yml`（本任务 `allowed_scope` 之外，`115a4286` 带上它），所以证书要重新钉。
     两张表与计数块按新树重取（**数字未变**：150 笔 `v1_required`、181 条测试、178 `passed`、3 `not_run`）。
  ② **总门在新基准上真跑一遍**：`2026-09-23 07:13:13 → 07:13:47`，退出码 0、17 行、`NOT ASKED` 0 行
     （第 2 节 Gate H 第 1 段）。
  ③ **MOF canonical workflow 在新基准上真跑一遍**：`2026-09-23 07:14:28 → 07:15:03`，50 步全绿、退出码 0、
     `FAIL` 0 条；Gate I 的引文换成**这一次**运行的逐字输出（外部组的 pid 每次播种都会变，上一版那一个已经不再是
     树里存在的那个）。
  ④ **复核意见逐条处置**（复核给的是 1 条 minor + 4 条 nit）：
     minor——审计脚本「证书过期」那段的出路 (a) 打印的是**当前 HEAD**，而它说的是「在报告自己的基准上跑」，
     已改成打印**报告钉的那个 SHA**（第 4 节 2b 的说明与它一致）；nit①——走查文件数 2106 → **2108**，
     并写明这个数随本地**被忽略的**构建产物浮动、判定只看那条 tree claim；nit②——R14 的「全部调用者只有那一页自己」
     改成「**产品侧**的调用者只有那一页自己」，并把 `apps/web/lib/attestations.test.mjs:84/88/89` 那三个单测调用点点名；
     nit③——「absence-manifest 引文漏了结尾一行」**不采纳**，两次实跑核对后确认那一行属于这个 checker 的
     **另一种调用方式**（非 `--selftest`），总门跑的是 `--selftest`，证据是第 2 节 Gate H 第 3b 段；
     nit④——脚本里剩下的两个 `head -n1` 是**位置**断言（哪一行在前），已加注释说明它们为什么不走 `collect_all`。
  ⑤ **H6 那一行的证据拆成两个入口**：`make a11y`（真数据、核心页面带数据 ≥ 7 的下限）由 `#T1104-TEST-01` 落账，
     总门里的 `a11y` 那一行是本报告本轮实跑（对桩 API，`core pages scanned with data: 0`）——
     上一版把两次运行合成了一句，本版分开写，**判定不变、措辞收紧**。
  ⑥ 第 4 节 5d 的 `summary:` 引文补上漏掉的一个 `|`（本轮逐字重跑核对出来的）。
- **本报告不自行放宽任何门**（这一句在 `未通过 = 0` 的版本里更要紧，所以照旧留着并改写）：
  Gate G 的处置在裁定之前是「未通过、把裁定权交回 owner（L3-③）」；裁定到了之后它按**范围豁免**通过，
  改的是**主体在不在 V1 里**，不是判据——`docs/31` 没动、`未通过` 的判定口径没动、
  门层统计仍由脚本从处置行里数出来。Gate H 的改判逐条给了可复跑的替代物，并写明三条会让它回到「未通过」的条件。
  报告作者**不改台账、不改测试、不改判定口径**。

---

## 6. 修订记录（本次重钉）

### 6.0 本次重钉的修订记录（T1214，第九轮 / 最终重钉轮）

#### (a) 这一版钉在哪棵树、哪一天、基准 SHA 变成什么

- **树**：`91ed2d5fdb24c7a0b0f332d6861fb5ffde5eb86f`（`git rev-parse HEAD` 实测，第 4 节第 1 条）；
  **日期**：2026-09-23。
- **上一版的基准**：`a8850f51`（第八轮 / T1213）。**这一版换基准，换的理由不是仪器变了，是判据变了**：
  `a8850f51..91ed2d5` 之间落地的**八笔**提交里，`90167ba`（按 owner 五条 L3 裁定收窄 V1 范围）
  改掉的是**报告赖以判定的规格本身**——`docs/02` §3/§4、`docs/12`/`15`/`17`/`23`/`27`/`47`/`55`/`56`、
  `specs/SPEC_VERSION.json`、`specs/api/openapi.yaml`（−20 行）、`specs/mcp/tools.json`（+2 行）、
  `specs/orchestrator/gates.json`。**判据动了，钉在旧判据上的证书就必须重钉。**
  这八笔逐条列出（`git log --format='%h %s' a8850f51..91ed2d5 | cat -n` 实测输出，
  **最后一笔就是本版基准自己**——这也是「八」而不是「七」的原因）：

  ```text
  1  91ed2d5 chore(tasks): T1214 任务书第 5 条里那句「脚本里有 10 个门标题的循环」是我编的
  2  8aabe12 chore(tasks): T1214 的任务书里那四条记实错误：把复核给的原话换成我自己量出来的数
  3  202a4a6 docs(scope): 把 §4 引言的归属写准——新裁定的四条 vs 立项时就定下的各条
  4  ade80a3 chore(tasks): 立 T1214（证书最后一次重钉）——DAG、状态、台账、G3 覆盖同笔补齐
  5  90167ba specs(scope): 按 owner 五条 L3 裁定收窄 V1 范围；契约删两条无实现的 blob 端点
  6  3e4dbac docs(tasks): ㊺ §8 — §12 完成声明的四条逐条核：三条已成立，第四条缺的是产品裁定不是工程
  7  f8015d8 docs(tasks): ㊺ — T1213 合并；复核四条意见逐条核实后记账；第三个骗人的仪器
  8  3a1b5cf [T1213] 把 V1 证书在最终树上重钉一次：Gate H 在 T1211 的新安全仪器上重挣，返工轮已处置的四处逐条复核 (#360)
  ```
- **基准行的落点**：头部第 4 行与第 1.2 节那一条 `> 生成基准：`，都由 `a8850f51…` 换成
  `91ed2d5…`，逐字来自本轮 `bash tests/acceptance/v1-final-audit.sh --emit-tables` 的**同一次输出**
  （`<!-- 生成命令：bash tests/acceptance/v1-final-audit.sh --emit-tables -->`）。
- **SHA 对照表**（本文正文一律按这个说法标注，不留一个光秃秃的 SHA 指代当前基准）：
  `9b22339e` = 第五轮的基准；`b6c1fb1a` = 第六轮（发放时）的基准；`115a4286` = 第七轮（返工轮）的基准；
  `a8850f51` = 第八轮（重钉轮）的基准；**`91ed2d5` = 本版（第九轮）基准**。

#### (b) 逐层的「重挣 / 沿用」

**判据**：把「该层的驱动路径」定义为**第 2 节该节每一行的证据列里点名过的测试/代码路径**
（台账文件不算：它在每一层都被读过，且它自己随本轮合并而前进）。驱动路径在
`a8850f51..HEAD` 之间没变 ⇒ **沿用**；变了 ⇒ 说清变的是什么、以及沿用/重挣为什么成立。

| 层 | 本轮 | `git diff --stat a8850f51..HEAD -- <该层驱动路径>` 原文与理由 |
|----|------|----------------------------------------------------------------|
| **Gate A** Domain Integrity | **沿用** | `-- internal/rsg internal/domain internal/assets infra/migrations tests/integration` → **（无输出）** |
| **Gate B** Collaboration | **沿用** | `-- internal/application/resolutions internal/rsg/conflict tests/integration tests/e2e/conflict_e2e_test.go` → **（无输出）**（第八轮那次这里改过 `conflict_e2e_test.go`，本版没动） |
| **Gate C** Release/Asset | **沿用** | `-- internal/assets tests/integration tests/e2e-release` → **（无输出）** |
| **Gate D** Network | **沿用** | `-- internal/domain tests/e2e-anonymous tests/e2e/conflict_e2e_test.go tests/e2e/explore_e2e_test.go` → **（无输出）** |
| **Gate E** Search | **沿用** | `-- internal/search tests/answer tests/integration` → **（无输出）** |
| **Gate F** Web UX | **沿用** | `-- tests/web-smoke tests/e2e-files tests/e2e-pulls` → **（无输出）** |
| **Gate G** Agent/API/Git | **改判**（既非沿用、也非重挣） | `-- cmd/mcp-server specs/mcp specs/api tests/cmd/contractgate` → **有输出**：`specs/api/openapi.yaml \| 20 --------------------`、`specs/mcp/tools.json \| 2 ++`（`2 files changed, 2 insertions(+), 20 deletions(-)`）。**这一层的驱动路径是本版唯一动过的一层**——动的正是它的**判据**（契约删两条端点、工具目录加 `scope` 标记）。没有跑任何测试就改判，改的是**主体在不在 V1 里**；四条 checkbox 的逐条处置与依据见第 2 节 Gate G |
| **Gate H** Security/Quality | **重挣**（本轮实跑） | `-- tests/security ops/security` → **（无输出）**：**仪器一个字没动，所以沿用本来也成立**。本版仍然实跑了整门（`19:27:32 → 19:28:07`，退出码 0、17 行 `ok`、`NOT ASKED` 0 行），理由与逐层账写在 Gate H 第 1 段 |
| **Gate I** Canonical Workflow | **重挣**（本轮实跑） | `-- tests/acceptance/mof-canonical-workflow.sh` → **（无输出）**：**脚本没变**；重挣的理由是「证书是某一棵树上的快照」，而这一版换的正是那棵树。实跑 `19:25:59 → 19:26:41`，退出码 0、50 步全绿、`FAIL` 0 条 |
| **Development System Gate** | **沿用** | `-- cmd/rddev internal/devorchestrator` → **（无输出）**；`-- .github/workflows/ci.yml Makefile scripts/ci.sh` → **（无输出）**（第八轮那一次这三处是**有**输出的：ci.yml `20+/7−`、Makefile `7+/4−`、scripts/ci.sh `6+/1−`，都是 T1210/T1212 自己落的改动）。这一层读的是台账事实，本版**没有**重跑任何 `rddev` 门 |
| **机械块**（基准行、§1.2 / §1.3 两张表、计数块） | **重挣** | 逐字来自本轮 `--emit-tables` 的同一次输出；逐行核对方式是拿那次输出的每一行去报告里 `grep -qF`，零缺失 |

#### (c) Gate G 的处置为什么改判——不是条件变松，是主体被移出了 V1

- **上一版为什么判「未通过」**：四条 checkbox 里第 1 条「MCP semantic tools 完整且权限正确」没有通过，
  而它挂的是一条**待 owner 裁定的 L3**（`L3-③`：V1 含不含 MCP 工具面）。当时规格把这一面列进 V1 必做
  （`docs/02:52`）、`docs/56:15` 把它挂在 Gate G 上、而树里 21 条工具 0 个 dispatch 点、`/mcp` 答 501。
- **这一版为什么改成「范围豁免」**：owner 2026-09-23 裁定 **V1 不含 MCP / Agent 工具面**，
  依据与落点逐条可查——`docs/02_V1_SCOPE.md` §3（`:54-55`）与 §4（`:73-74`）、
  `docs/56_REQUIREMENTS_TRACEABILITY.md:16`（「无 —— **V1 不含**（`docs/02` §4，`L3-③`）」）、
  `specs/mcp/tools.json:3` 的 `"scope": "post-v1"`、`docs/47_MCP_TOOL_CATALOG.md:3-5` 的横幅、
  `tasks/decisions.md` **㊻**、落地提交 **`90167ba`**。
- **豁免的是哪两条、通过的是哪两条**：**豁免** Gate G 的**第 1 条**（MCP semantic tools 完整且权限正确）
  与**第 2 条的主体**（agent 无 merge/publish 扩权——主体是 agent 工具面，同样不在 V1）；
  **通过**的是**第 2 条的保护那一半**（「不经过人就扩大可见性 / 合掉 main」的保护在 V1 真正交付的
  Semantic HTTP API 面与 Web 面上都有既有证据，逐条引在第 2 节 Gate G 第 2 段），以及**第 3、第 4 条**
  （Git branch push ingestion / main direct push 拒绝、Git/RSG drift reconciliation 可检测），后两条的证据原样沿用。
- **一句话**：判据（`docs/31_MASTER_ACCEPTANCE.md`）一个字节没动，树里也**没有**这一面——
  **变的是「它要不要在 V1 里」这条产品范围**，不是「它做得够不够好」这条工程标准。
- **这一处的机械证明写在这里**：审计脚本里那句统计**不再写死门名**。上一版硬编码着 `（Gate G）`，
  在 `未通过 = 0` 的版本里它会同一句话里既说「0 条未通过」又点名一个门未通过——
  本版改成「只在 `未通过 > 0` 时才点名，且门名由**处置行的位置**与 10 个门标题配对得出」。
  三个方向的实跑证明见 6.0(e)（**在同一棵工作树上原地改、当场还原**，方法与备份见那一节）。

#### (d) `EXCLUDE_TASK=T1207` 的状态：**今天是空操作，本版原样保留**

脚本第 1 节那句 `EXCLUDE_TASK="T1207"` 是 **T1207 那个年代的产物**（那一版证书就是 T1207 自己写的，
不排除自己的话，第 1.2 节那张表里会多出正在写它的这一笔）。**T1207 早已合并**（`392e6d6`）、
`T1207-TEST-01` 也已落账，所以这条排除**今天是空操作**：即使不排除，第 1.2 节那张表也是空的
（脚本实测打印 `EXCLUDED=T1207`、`V1_REQUIRED_UNMERGED_COUNT=0`）。
**本版不删它**——删掉就是在改判据、也在改被复核的那个脚本；只在这里写清它现在是空的、以及它为什么还在。
（逐字原文与更早那几版的说法见 6.3，那里原样保留。）

#### (e) 这一版的两处清扫、以及五个「仪器会说 no」的实跑证明

**先说方法**：下面五条证明都在**同一棵工作树**上做（把报告按原样备份到 `/tmp/t1214-report-backup.md`，
改一处、跑一次、立刻从备份整份还原，用 `sha256sum` 核对还原到位：**备份、还原后两次 `sha256sum`
逐字相等，并且等于交付这一份**——这一组原文（含那把 sha256 的完整值）记在 RESULT 里，
**不写在这个文件里：写进来就会改掉它所度量的这个文件**）。**没有在 `/tmp` 里另建一棵树去跑**——
理由照实写：这个脚本用 `"$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"` 定自己的根，
在 `/tmp` 副本里它需要一棵带 `.git` 的树，而**给副本造 `.git` 是 git 控制面的动作**，本 Worker 不做。
所以做法改成「原地改、当场还原」，每条证明后面都跟着还原后的退出码 **0**。

**证明 1（§0 的裁定行：删一条 → 红）**。把第 80 行那条 L3-① 的 `**裁定**：` 前缀改成别的字：

```text
$ bash tests/acceptance/v1-final-audit.sh          # 已把 :80 的 **裁定**： 改成 **裁定**（待定）：
FAIL: the report's section 0 names 5 L3 label(s) but carries 4 ruling line(s) (a line is one that starts with **裁定**：) — every named L3 needs its own ruling line
AUDIT FAILED
退出码 1
$ # 从 /tmp/t1214-report-backup.md 整份还原之后：
$ bash tests/acceptance/v1-final-audit.sh ; echo "exit=$?"
… AUDIT OK: report markers present, counts match (0 unmerged, 1 not_run, 0 failed, 0 skipped, 0 passed-without-evidence).
exit=0
```

**证明 2（门层统计：判定与统计不一致 → 红）**。把 Gate C 那一条处置行改成「未通过」，
而 §5.3 那句统计**照样写着 10 / 0**：

```text
$ bash tests/acceptance/v1-final-audit.sh          # 已把 Gate C 的处置行改成 **处置**：未通过。
FAIL: report missing required marker: Gate 层面：**9 条「通过」、1 条「未通过」……
      （省略的末段 = 脚本点名的那个门名；逐字抄进本报告，这段引文就会变成它自己的证据）
AUDIT FAILED
退出码 1
```

**这一条同时证明了「位置配对」是对的**：脚本要求点名的是 **Gate C**——报告里第 3 条处置行，
正好是 `GATE_TITLES` 的第 3 个（`Gate C`）。它没有去点名别的门。

**证明 3（统计句说 1 条未通过却不点名 → 红）**。在证明 2 的状态上，把 §5.3 那句改成
「9 条通过、1 条未通过」（**不带括号里的门名**）：

```text
$ bash tests/acceptance/v1-final-audit.sh          # 统计句改成 **9 条「通过」、1 条「未通过」**、不带门名
FAIL: report missing required marker: Gate 层面：**9 条「通过」、1 条「未通过」……
AUDIT FAILED
退出码 1
```

**证明 4（点错门名 → 红）**。同一状态上，把统计句点成 `（Gate H）`（**错误的门**）：

```text
$ bash tests/acceptance/v1-final-audit.sh          # 统计句点成 **…1 条「未通过」（Gate H）**
FAIL: report missing required marker: Gate 层面：**9 条「通过」、1 条「未通过」……
AUDIT FAILED
退出码 1
```

> **上面三段引文都在末段用了 `……`，这不是排版偷懒，是我这一版真正做错过一次的地方**：
> 第一稿把 `FAIL` 那一行**逐字**抄了进来（含脚本点名的门名），随后自己拿 `grep -cF` 一量才发现——
> **报告里那三行合起来正好构成了脚本要找的那串标记**。这意味着：将来真把某条处置改成「未通过」、
> 而 §5.3 还写着 10 / 0 时，**被要求的那串东西已经在报告里了，检查会被我自己的引文短路**。
> 所以末段一律省成 `……`，并把这条写在这里。**省掉的是那一串，不是结论**：
> 三个方向各自的退出码、`AUDIT FAILED`、以及 FAIL 那一行（除末段）都照抄。
> 下面**证明 5** 就是在**本节与本轮九处订正都写定之后**重跑的那一次——用最终交付的这一版证明它仍然会说 no。

**证明 5（本节与本轮订正都写定之后，在最终这一版上重跑）**。这一条专门回答「引文会不会把检查短路」：
报告里现在躺着上面那三段 FAIL 引文，再照**证明 2** 的手法把 Gate C 的处置行改成「未通过」：

```text
$ awk 'NR==242{print "**处置**：未通过。"; next}{print}' <报告> > <变异副本>   # 再把副本换回原位
$ sed -n '242p' tests/acceptance/v1-final-report.md
  **处置**：未通过。
$ bash tests/acceptance/v1-final-audit.sh | tail -2
FAIL: report missing required marker: Gate 层面：**9 条「通过」、1 条「未通过」……
AUDIT FAILED
退出码 1
$ /bin/cp -f /tmp/t1214-report-backup9.md tests/acceptance/v1-final-report.md   # 整份还原
$ sha256sum tests/acceptance/v1-final-report.md
# （这一行的原文 = 交付这一份的 `sha256sum`；它记在 RESULT 里，不写在这里——写进来就会改掉它所度量的这个文件）
$ bash tests/acceptance/v1-final-audit.sh | tail -1 ; echo "exit=$?"
AUDIT OK: report markers present, counts match (0 unmerged, 1 not_run, 0 failed, 0 skipped, 0 passed-without-evidence).
exit=0
```

> 两处说明。**其一**：`sed -n '242p'` 那一行在引文里**前置了两个空格**——报告里若出现一条
> **顶格**的 `**处置**：`，脚本会把它数成第 11 条处置行（`GATES_TOTAL=10`），这一节自己的引文
> 就会把门数搅乱。所以凡是抄这类行，一律缩进。**其二**：变异只改了一行，`FAIL` 那一行的末段
> 照上面同一条规矩省成 `……`；**还原是按 `sha256sum` 核对过的**——还原后与备份逐字相等、
> 且等于交付这一份（两次的原文都记在 RESULT 里），退出码 **0**。

**再补一条方向相反的**：门层统计**新写的这一句**在真实报告上必须能找到——
`10 条「通过」、0 条「未通过」`（本版实测 `AUDIT OK`、退出码 0，见证明 1 末尾那次还原后的运行）。

**清扫一：`未获裁定` / `未回答` / `至今未裁定` 这一族句子。** 改前（`HEAD` 上那份第八轮的报告）
逐字是这 5 行（`git show HEAD:tests/acceptance/v1-final-report.md | grep -n "未获裁定"`）：

```text
50:- **五条 L3 至今未获裁定**，每一条都挂在一条 Gate 判定或一条缺口处置上：L3-⓪（内容能不能出平台 → Gate E 第一条只按结构化答案判过，R8）、L3-①（登录前能不能搜索 → R9）、L3-②（V1 含不含 blob 上传入口 → R3 的处置）、L3-③（V1 含不含 MCP 工具面 → Gate G 未通过，R12）、L3-④（公开声明该在哪里被发现 → R14，`tasks/decisions.md` ㊳ 补记）。
229:**由证据背书的「结构化」答案满足；模型撰写的综述不在 V1 范围内，因为它卡在一条未获裁定的 L3（外部凭证/付费服务）上。**
578:`docs/15/17/23/27` 一起对齐。** 在裁定之前，这条的状态是「已记账的缺口 + 一条未获裁定的 L3」，
776:- **性质**：**这不是待补的工程活，是一条未获裁定的 L3。** 把 21 条工具建起来要先有产品语义
994:所以本报告**不宣告 V1 完成**：③ 不成立，而它不成立的原因是一条**未获裁定的 L3**，不是一件没做完的工程活。
```

**改后（工作树）**：先量**报告主体**（`awk` 停在 `#### (e)`，把本节整个切掉），逐字是这两行——
每一条都是**在引用旧说法以便订正它**，不是还在声称：

```text
$ awk '/^#### \(e\)/{exit} {print}' tests/acceptance/v1-final-report.md | grep -n "未获裁定"
272:（**T1214 订正这一句**：原文写「因为它卡在一条**未获裁定**的 L3（外部凭证/付费服务）上」——
673:> **这一条本版重新定性（T1214）：由「已记账的缺口 + 一条未获裁定的 L3」改成「范围外，不是欠账」。**
```

**报告主体 2 行**（改前那份 HEAD 上是 5 行，删掉三行断言、留下两行引用以便订正）。
整份文件的数字在本节末尾那段「自指」里——那里会多一些，**多出来的全是本节自己的引文**。

**同一族里的另外几处也一并扫了**（它们用的词是「未回答 / 待裁定」，不在上面那条 grep 里）：
R8 / R9 / R14 三个标题由「未回答」改成「**已裁定**」（各自带一个引用旧标题的说明块），
R12 由「（L3-③ 未裁定）」改成「（L3-③ **已裁定：V1 不含**）」，
R3 的标题行下方补了重新定性的说明块；§0 那条「几条 L3 至今未获裁定」整句换成了
**「五条 L3 已全部获得裁定」**，而这个「五」是脚本用 `L3_WORDS` 拼出来的（数出来几条就写几条，
不许手写）。**留下的两处「待裁定」都是过去时**（「㉟ 曾把这一条立为待裁定的 L3-②」「㊻ 同日记下裁定」），
它们描述的是过程，不是现状。

**清扫二：契约里那两条端点。** 上面 §3 R3 已逐条说明，这里给出原始输出——
**分两段量**，因为这一节自己的引文会被下一次 grep 数到。先量**报告主体**
（把本节整个切掉再数，`awk` 停在 `#### (e)` 这一行）：

```text
$ awk '/^#### \(e\)/{exit} {print}' tests/acceptance/v1-final-report.md | grep -n "request-upload"
685:    端点」，那描述的是**裁定之前**的树）：`grep -n "request-upload" specs/api/openapi.yaml`
688:    `grep -n "request-upload" tests/acceptance/v1-final-report.md` 在**报告主体**（不含 §6.0(e)
694:  - 产品代码里从来没有实现：`grep -Rn "request-upload\|requestUpload" cmd/api internal` 输出为空。
1107:grep -n "request-upload" specs/api/openapi.yaml || echo "zero hits in specs/api/openapi.yaml"
1109:grep -Rn "request-upload\|requestUpload" cmd/api internal || echo "zero hits in cmd/api and internal"
```

**5 行、5 处字样**——逐行读过：**三行是这一条在叙述这件事本身**（`:685` / `:688` / `:694`）、
**两行是第 4 节那条复跑命令**（`:1107` / `:1109`）。**没有一行**还在声称契约里声明着它们。

再量**整份文件**（不做任何切分，就是任务里那条命令）：

```text
$ grep -n "request-upload" tests/acceptance/v1-final-report.md | wc -l
13
$ grep -c "request-upload" tests/acceptance/v1-final-report.md
13
```

**13 行里 5 行在报告主体、8 行在本节**（本节这一段的命令行、被抄进来的那 5 行、以及下面这句自指）。
「未获裁定」同法：**整份文件 14 行，其中 2 行在报告主体**（`:272`、`:673`，两行都是在引用旧说法以便订正它）、
**12 行在本节**。所以**复核的人自己跑一遍，得到的行数一定比上面那两段多**——
多出来的行**全部在这两段引用块和这一节自己的句子里**，逐行可辨。

> **§6.1–§6.9 是上一版（第八轮 / T1213，基准 `a8850f51`）的修订记录：结构保留、原文保留，
> 本版只在三处插入了带「T1214 订正」字样的订正**（§6.2 的 Development System Gate 行、§6.8 的 B2 行
> 与 H4 行；按 `grep -n 'T1214 订正' tests/acceptance/v1-final-report.md` 逐处定位即可——该命令除这三处
> 外还会命中报告头、第 2 节、6.0(e) 的一行引文，以及这段说明自己；那几处都不在 §6.1–§6.9 里）。
> 那几节里的「本版」指的是**第八轮**，不是本版；本版的修订记录是上面的 **6.0**。
> 这样留着的理由：第八轮的逐层账（第六轮基准 `b6c1fb1a` → 第八轮基准 `a8850f51`）里有几处
> 本版直接引用的事实，删掉它们就会让 6.0(b) 的「仪器变了 / 没变」失去参照点。

### 6.1 第八轮钉在哪棵树、什么时候、为什么换

- **树**：`a8850f510033a771cdc2215c9ceb2b7059932f8d`（`git rev-parse HEAD` 实测，第 4 节第 1 条）；
  **日期**：2026-09-23。
- **基准行因此更新**：头部第 4 行与第 1.2 节那一条 `> 生成基准：`，都由上一版的 `115a4286…`
  换成 `a8850f51…`，逐字来自本轮 `bash tests/acceptance/v1-final-audit.sh --emit-tables` 的**同一次输出**
  （`<!-- 生成命令：bash tests/acceptance/v1-final-audit.sh --emit-tables -->`）。
- **三个 SHA 各是哪一版**（本文正文一律按这个说法标注，不留一个光秃秃的 SHA 指代当前基准）：
  `9b22339e` = 第五轮（收尾轮）的基准；`b6c1fb1a` = **发放时**（第六轮）的基准，也是本节「沿用」判据的参照点；
  `115a4286` = 上一版（第七轮 / 返工轮）的基准；`a8850f51` = **本版**基准。
- **为什么换**：`115a4286` 之后 `main` 又前进三次合并（T1210 / T1211 / T1212），
  其中 T1211 改的正是 **Gate H 赖以得绿的那套仪器本身**——逐字：

  ```text
  $ git diff --stat b6c1fb1a..HEAD -- tests/security ops/security
   ops/security/absent-checks.json              |  5 +-
   ops/security/license-allowlist.json          |  4 +-
   ops/security/tool-versions.sh                | 43 ++++++++++---
   tests/security/README.md                     | 59 ++++++++++++++++--
   tests/security/container-scan.sh             | 93 +++++++++++++++++++++++-----
   tests/security/install-security-tools.sh     | 36 ++++++++++-
   tests/security/license_audit.py              | 79 ++++++++++++++++++++---
   tests/security/master-gate-mutation-check.sh | 50 ++++++++++-----
   tests/security/master-security-gate.sh       | 43 ++++++++++++-
   tests/security/sast.sh                       | 21 +++----
   tests/security/sbom.sh                       | 76 ++++++++++++++++++++---
   11 files changed, 428 insertions(+), 81 deletions(-)
  ```

  **仪器变了，用旧仪器挣来的绿就不是这份交付上的绿**——这正是本版重钉的机械理由（不是审美理由）。

### 6.2 逐层的「重挣 / 沿用」

**判据先说清**：本表把「该层的驱动路径」定义为**第 2 节该节每一行的证据列里点名过的测试/代码路径**
（台账文件不算：它在每一层都被读过，且它自己随本轮合并而前进，拿它当判据等于什么都没说）。
驱动路径在 `b6c1fb1a..HEAD` 之间没变 ⇒ **沿用**（沿用 = 判定与证据照上一版，理由是该层的输入没动）；
变了 ⇒ 写清变的是什么、以及沿用/重挣为什么仍然成立。

| 层 | 本轮 | 驱动路径的 `git diff --stat b6c1fb1a..HEAD -- <路径>` 原文与理由 |
|----|------|--------------------------------------------------------------------|
| **Gate A** Domain Integrity | **沿用**（自发放时 `b6c1fb1a`） | 路径未变：<br>`$ git diff --stat b6c1fb1a..HEAD -- internal/rsg internal/domain internal/assets infra/migrations tests/integration`<br>（无输出） |
| **Gate B** Collaboration | **沿用** | 单元侧未变：`… -- internal/application/resolutions internal/rsg/conflict tests/integration`（无输出）。**有一处驱动文件变了**：<br>`$ git diff --stat b6c1fb1a..HEAD -- tests/e2e/conflict_e2e_test.go`<br>` tests/e2e/conflict_e2e_test.go \| 30 ++++++++++--------------------`<br>` 1 file changed, 10 insertions(+), 20 deletions(-)`<br>变的是文件顶部那段**本地探针**（`pgReachable` 换成 `testdb.RequireDB`）与它的头注释；**被引用的那条断言一个字节没动**：`git diff b6c1fb1a..HEAD -- tests/e2e/conflict_e2e_test.go \| grep -c 'human-governed'` = **0**，该字符串在 `b6c1fb1a` / `115a4286` / `HEAD` 三处都恰好出现 **1** 次，函数行号 447 → **437**（B2 行的引用本版已按新树更正） |
| **Gate C** Release/Asset | **沿用** | 路径未变：`… -- internal/assets tests/integration tests/e2e-release`（无输出） |
| **Gate D** Network | **沿用** | 路径未变：`… -- internal/domain tests/e2e-anonymous` 与 `… -- tests/e2e/explore_e2e_test.go`（都无输出） |
| **Gate E** Search | **沿用** | 路径未变：`… -- internal/search tests/answer tests/integration`（无输出） |
| **Gate F** Web UX | **沿用** | 路径未变：`… -- tests/web-smoke tests/e2e-files tests/e2e-pulls`（无输出） |
| **Gate G** Agent/API/Git | **沿用** | 路径未变：`… -- cmd/mcp-server specs/mcp specs/api tests/cmd/contractgate`（无输出） |
| **Development System Gate** | **沿用**（读的是台账事实，本轮**没有**重跑任何 `rddev` 门） | `cmd/rddev`、`internal/devorchestrator` 未变（无输出）；这一层的每条 S1–S7 都指 `T0000`–`T0012` 的账本条目。**但这一层的面里有三处 CI 管道文件确实变了**（**T1214 订正：原文写「两处」，下面却列了三处——这句话自己数错了**），逐字如下，且它们是 T1210 / T1211 / T1212 自己落地的改动、不是本版改的：<br>`$ git diff --stat b6c1fb1a..HEAD -- .github/workflows/ci.yml` → `1 file changed, 20 insertions(+), 7 deletions(-)`。**T1214 订正这一格：`20+/7−` 不是一笔任务的改动，是三笔**——实测读法 `git log --format='%h %s' b6c1fb1a..HEAD -- .github/workflows/ci.yml` 给三笔，逐笔 `git show --stat <c> -- .github/workflows/ci.yml` 给 `115a428`（T1210，`10 insertions(+), 2 deletions(-)`：把那条无条件 `sudo apt-get` 改成「先断言 `command -v redis-cli`，缺了才装」）、`d98022a`（T1211，`3 insertions(+), 3 deletions(-)`：装钉版的 uv）、`a8850f5`（T1212 的 CI 半边，`7 insertions(+), 2 deletions(-)`：`POST_REQUIRE_E2E_DB=1` 那一步），**10+3+7 = 20、2+3+2 = 7，与总数逐字吻合**<br>`-- Makefile` → `1 file changed, 7 insertions(+), 4 deletions(-)`（T1212 改了 `GO_UNIT_PKGS`；**注意这 7 行里含 4 行删除，所以它对 `dev:` 位置只贡献净 +3，见 6.8 那一格**）<br>`-- scripts/ci.sh` → `1 file changed, 6 insertions(+), 1 deletion(-)`。这三处不承载 S1–S7 的判定（那七条读的是 `tasks/tests.json` 里的 `passed` 与本节最后一条「状态外部化」的文件清单），本版照实点名，不把它说成「路径全未变」 |
| **Gate H** Security/Quality | **重挣**（本轮实跑，见 6.1 的原文） | 仪器本身变了（11 个文件、428 插入）⇒ 旧绿不算数：`2026-09-23 11:00:36 → 11:01:22`，退出码 **0**、`MIN_CHECKS=17`、注册表 17 行、17 行 `ok`、`NOT ASKED` **0** 行、`absence-manifest` 三条 witness 逐字引在 Gate H 第 3 段。**走查数那一次打印 2112**（当时装齐了前置工具与构建产物）；把这些产物清掉之后同一行打印 **2110**——**这个数只跟着被忽略的文件走**，判定看的是它自己打印的那条 tree claim（Gate H 第 5 段的限定仍照原样成立），两次都 `ok` |
| **Gate I** Canonical Workflow | **重挣**（本轮实跑） | **重挣的理由不是它的脚本变了**——`$ git diff --stat b6c1fb1a..HEAD -- tests/acceptance/mof-canonical-workflow.sh`（无输出）；理由与整份报告同一条：**证书是某一棵树上的快照**，这次实跑发生在 `a8850f51` 上（`11:01:40 → 11:02:15`，退出码 0、50 步全绿、`FAIL` 0 条，pid `za4ezyp4wfzbm6jzjmzrzych1c`） |
| **机械块**（基准行、§1.2 / §1.3 两张表、计数块） | **重挣** | 逐字来自本轮 `--emit-tables` 的同一次输出；逐行核对方式：把那次输出的每一行拿去 `grep -qF` 报告，**零缺失**（本节的命令见第 4 节第 2 条那一段） |

### 6.3 `EXCLUDE_TASK=T1207`：**今天是空操作，本版原样保留**

脚本第 1 节那句 `EXCLUDE_TASK="T1207"` 是 **T1207 那个年代的产物**：那一版证书就是 T1207 自己写的，
不排除自己的话，「尚未合并的 v1_required 任务」那张表里会多出正在写它的这一笔。
**今天 T1207 早已合并**（`392e6d6`）、`T1207-TEST-01` 也已落账，所以这条排除**现在是空操作**——
即使不排除，第 1.2 节那张表也是空的（脚本实测打印 `EXCLUDED=T1207`、`V1_REQUIRED_UNMERGED_COUNT=0`）。
**本版不删它**：删掉就是在改判据、也在改被复核的那个脚本（这一版的复核对象正是它）；
只在这里写清它现在是空的、以及它为什么还在。

### 6.4 逐字引文里的绝对路径：本版的唯一处置 =「保留 + 在那一节的标题处说明」

复核 risk③ 要的是**把这件事判一次**，不是两处各说各话。本版的处置是**保留**，并把理由写在**引用它的那一节**：

- **保留**：那些路径属于**被转录的输出**（Gate H 第 3 段的 `absence-manifest` 第三行里那句
  `python3 tests/security/check-absent-manifest.py --selftest` 打印的
  `/home/shibo/code/post/.rddev/worktrees/T1214/ops/security/absent-checks.json`），
  报告只是把它抄了下来。**改掉它，那段引文就不再是逐字的**——而这份报告的每一处引文的全部价值就在于逐字。
- **在那一节的标题处说明**（第 2 节 Gate H 第 3 段那一块引用）：路径属于**被转录的输出**，
  **不是报告在给读者指路**；本版把它重钉到 `…/worktrees/T1213/…`（本基准的实际路径），
  而不是留着上一版的 `…/worktrees/T1210/…`——**一份 CURRENT 的证书里指着别的 worktree 的绝对路径才是错的**。
- **另一条处置（脱敏）没有采用**，代价写在上面那句里：一脱敏，那一段就不再是逐字引文，
  标题里也不能再写「逐字」二字。**两处都按这一条办**（Gate H 的 witness 行、Gate I 的 `[50]` 行——
  后者本次运行在干净的树上跑，`got` 后面是空的，本来就没有路径）。

### 6.5 本版复核用的探针（**都在 `/tmp` 的一份副本上跑，树里不留痕**）

复核意见说「能改的要给自证」。自证不能只是「我改了」，得是**构造出来的样本 + 改前改后的两次实跑**。
副本的做法：`git archive HEAD` 解出一棵树到 `/tmp/t1213-probe`，往里写一个纯文本 `.git` 文件
（`gitdir: /home/shibo/code/post/.git/worktrees/T1213`，只读视角，**没有**在 `/tmp` 里 `git init`），
再把本版的 `tests/acceptance/**` 覆盖进去；脚本的 `ROOT` 由它自己的路径推出，所以在副本里跑的就是副本里的报告。

| 探针 | 构造样本 | 旧脚本（`HEAD` 那一版） | 新脚本（本版） |
|------|----------|--------------------------|----------------|
| (i) 对照 | 报告**未改** | 退出码 **0**，末行 `AUDIT OK: report markers present, counts match (0 unmerged, 1 not_run, 0 failed, 0 skipped, 0 passed-without-evidence).` | 退出码 **0**，同一行——**新断言不咬诚实的报告** |
| (ii) nit②（门层统计只认一种排版） | Gate H 的处置行从 `**处置**：**通过——…` 改成 `**处置**：未通过——构造样本（本轮探针，不进版本库）：…`（去掉紧跟冒号的那对星号） | 退出码 **0**、`AUDIT OK: …`——**没数到那个「未通过」**；把两版推导单独跑：旧推导 `grep -cE '^\*\*处置\*\*：\*\*未通过'` = **1** | 退出码 **1**，`FAIL: report missing required marker: Gate 层面：**8 条「通过」、2 条「未通过」…**`（末段按表下那句说明省成 `…`）；新推导 = **2**（先抹掉 `*`/`_`/空白再数判定），于是报告里那句 9 / 1 与数出来的 8 / 2 不一致 ⇒ **红** |
| (iii) nit④（ADR 路径只在报告里被提到） | 把 `docs/adr/ADR-028-v1-ships-no-container-image.md` 移出树（报告未改） | 退出码 **0**、`AUDIT OK: …`——`require_marker` 只看报告里有没有那串字符 | 退出码 **1**，`FAIL: the written risk acceptance the report cites is not in the tree: docs/adr/ADR-028-v1-ships-no-container-image.md`（放回去后同一脚本再次退出码 0） |
| (iv) nit③（`collect_all` 的边界） | 把第 1.3 节两处分布句**删掉一处**（只剩一处正确的） | —— | 退出码 **0**（边界确认：它保证「已出现的每一处都说对」，不保证「该出现的都还在」；全部删光才红）。**这一条不修**，理由已逐字写在脚本 `collect_all` 上方的注释里 |

> **为什么 (ii) 的引文把 marker 串的末段省成 `…`——本版定稿时重跑抓到的一处「自我缴械」**：
> 那一格第一次写的时候是把脚本打印的整句 `FAIL: …` **完整**抄进去的。在定稿的这棵树（而不是写这一格时的那次中间状态）上重跑同一条探针时，实测到的是
> **退出码 0、`AUDIT OK`**——与那一格写的「**1**」不符。原因是 `require_marker` 用 `grep -qF` 在整份报告里找那串字符，
> 而**这段引文自己**就含有它：探针的引文满足了它所演示的那条断言，构造样本于是不再变红。
> 省掉的只是 `（Gate G）**` 这几个字（`FAIL:` 那半句、`8 条「通过」、2 条「未通过」`照旧逐字），
> 省完在同一条样本上重跑：旧脚本 **0**、新脚本 **1** 并打印那句 `FAIL`——与本格一致。
> 这不是措辞洁癖：**把失败信息逐字抄进被校验的报告，会让那条断言再也说不**；脚本 `require_marker` 上方已把这条写进注释，供下一位改写者参考。

### 6.6 第七轮复核（`verdict=approve`：1 minor + 5 nit + 5 risks）逐条处置

**判据是「报告 ↔ 校验器的一致性与可复现性」，不是好看。**

| # | 复核说什么 | 本版处置 | 自证 / 落点 |
|---|------------|----------|-------------|
| minor | `.rddev/workers/T1210/RESULT.json` 里「五条复核意见」的说法指的是上一轮，第五轮原文（`T1207-review` 的 a–e）没被逐条点名 | **不改，理由：面外**——那个文件在 `.rddev/workers/T1210/**`，本任务的写入面只有 `tests/acceptance/**`；写它就等于改上一轮的交付记录 | 面外事实：本任务包的硬线逐字列出可写范围只有 `tests/acceptance/**`；本版把这个处置写在 RESULT 里交给 Supervisor |
| nit① | 脚本头注释说「做五件事」，下面的清单是六条 | **改了** | `tests/acceptance/v1-final-audit.sh:5` 现在是「这个脚本做**六件事**」，并逐字说明「这里原写『五件事』，而清单在 T1210 加进第 5、6 条之后已经是六条——数一遍清单就知道，改的是这句注释」 |
| nit② | `gates_failed` 只认一种排版（`**处置**：**未通过`），换个加粗就能绕过判定/统计一致性检查 | **改了**（按复核给的两个方向里的第一个，并比它更进一步：不是 `\*{0,2}`，而是**先抹掉排版再数判定**） | 探针 (ii)：构造样本上旧脚本退出码 0、旧推导数到 1；新脚本退出码 1、新推导数到 2，红的理由正是「报告那句 9 / 1 ≠ 数出来的 8 / 2」。脚本 `:413` 起是逐字注释、`:420-422` 是数法本身（**T1214 重写脚本后按本树重数**；第八轮脚本里这两处是 `:375` / `:382-384`——那一轮在 `require_marker` 上方补注释后自引用推移的两处之一，见 6.8 末句） |
| nit③ | `collect_all` 保证的是「已出现的每一处都说对」，不是「该出现的地方都还在」 | **文档化了，但不加位置断言**——复核自己说这一条「不是本轮的偏差」，本版的选择与理由逐字写在脚本 `collect_all` 上方的注释里（加位置/条数断言 = 把值检查升级成排版检查，报告每增删一句就红一次，红的原因与它要防的东西无关） | 探针 (iv)：删掉一处分布句，脚本仍退出码 0——**注释里写的边界与实跑一致**；复核自己给的样本（把 `:31` 换成 `**分布见台账**`）与这条同理 |
| nit④ | `require_marker` 只断言报告**提到** ADR 路径，不断言文件在树里 | **改了**（**只加不减**：原来那条 `require_marker` 一个字没动，另加一条 `[ ! -f "$ROOT/docs/adr/ADR-028-v1-ships-no-container-image.md" ]` 即红） | 探针 (iii)：把 ADR 移出树，旧脚本退出码 0、新脚本退出码 1 并打印 `FAIL: the written risk acceptance the report cites is not in the tree: …`；放回去后退出码 0 |
| nit⑤ | 报告 §1.3 **与 §4 第 7 条**给读者复跑的 jq 与脚本钉的那条**逐字不同**（少了 `gsub("[[:space:]]";"")`） | **改了报告，脚本一个字节没动**（脚本那条断言本来就是对的：全空白的 `evidence` 也算没有证据） | 报告 §1.3、§4 第 7 条，以及本版另外补进 §4 第 3 条的那一条，现在印的都是**脚本那一行的逐字形式** `select(.status=="passed") \| select((((.evidence // "") \| gsub("[[:space:]]"; "")) \| length) == 0)`；本基准三种写法都返回 `0`（第 4 节第 3 条的输出），**判定不受影响** |
| risk① | 没有任何 CI 作业 / Makefile 目标引用 `v1-final-audit.sh`：下一笔 state commit 就会让它在 `main` 上非 0，且没人因此变红 | **不改，理由：面外 + 已立账**——那是 `tests/` 之外的 CI/编排决定，`tasks/decisions.md` ㊷ 已把「这条脚本故意没有 CI 消费者」记为决定；本版把它原样带进 RESULT 的 risks | ㊷ 的原文；本版的 risks 第 1 条 |
| risk② | Gate H / Gate I 的绿建立在对**冻结树**的实跑上；合并门的 G2 会在真 CI 上再跑一次 `security-master` | **不改**（本版做的就是这件事本身，且把它拆到「逐层重挣/沿用」上，见 6.2）；本轮两次运行的产物与时间戳逐字写进 Gate H 第 1 段、Gate I 的 I1 行与第 4 节 | 第 4 节第 4 / 5 条；`/tmp/t1213-mof-out.txt`、`/tmp/t1213-master-gate-report.json` |
| risk③ | 逐字引文带来绝对路径（`…/worktrees/T1210/…`），保留 / 脱敏 / 加说明「需要一并决定」 | **改了**：判成**保留 + 在那一节的标题处说明**，并把路径重钉到本基准的实际路径 | 6.4 一节；Gate H 第 3 段标题处那一块引用 |
| risk④ | R10 的残余：`docker-compose` 拉的基础设施镜像按 tag 钉住、无人扫描（ADR-028 只覆盖「V1 交付不含镜像」） | **不改**（它是被点名保留的残余风险，不是待办的改动）；本版照原样把它带进 RESULT 的 risks，并在 R10 里保持原文 | R10 末段与 Gate H 第 5 段 |
| risk⑤ | 统计句把「哪一条未通过」写死成「（Gate G）」：Gate G 一旦改判，那句与那条标记必须**重新生成**而不是手改 | **不改，理由：那是「重生成」而不是「放松」**——标记钉的正是当下事实（`任务书` 也要求判定与统计一致），而要它随判定自动改写就得让脚本去认各节的判定格，属于**新增断言**、不在本轮复核要求的范围内；本版把这条风险照原样交回 Supervisor | 脚本 `:452-457` 那一带（**T1214 重写脚本后按本树重数**；第八轮脚本里是 `:391`——那一轮在 `require_marker` 上方补注释后自引用推移的两处之一，见 6.8 末句）；本报告第 4 节第 2b 条已写明「改判后必须重新生成」的流程 |

### 6.7 上一轮返工单（第七轮的 1 minor + 4 nit）那四条：**只核对，不重做**

它们是**上一轮复核的对象**，改掉就等于把被复核的东西换掉。本版只做「我读了哪一行 / 我跑了哪条命令 /
我看到什么」的核对：

| 返工单条目 | 本版核对方式 | 看到什么 |
|------------|--------------|----------|
| ① 出路 (a) 要打印**报告自己钉的** SHA | 在副本里把报告的三处基准行改回 `115a4286…`，跑本版脚本（副本 HEAD = `a8850f51`） | 打印 `(a) 在报告自己的基准上跑这条命令：git checkout 115a4286bf8ccea8ddc0ff4f56747c4082ba483d（或那个基准的 worktree）`——**是报告自己的 SHA，不是当前 HEAD**；本次实跑退出码 1 |
| ② 走查数 2106 → 2108，并写明这个数随被忽略的构建产物浮动 | 读 Gate H 第 5 段；在本树上跑 `--only container-scan` | 段落仍在（本版把当下实测数更新为 **2112**，并把 2106 / 2108 标成冷树 / 上一版基准）；本树那一行逐字：`ok   container-scan: no container build file in the tree (2112 file(s) walked, tree claim 'no-dockerfile' holds)`——**数确实在浮动，判定只看那条 tree claim** |
| ③ R14 的「全部调用者只有那一页自己」改成「**产品侧**的…」并点名三个单测调用点 | 读 R14；`grep -rn attestationHref apps/web/lib/attestations.test.mjs "apps/web/app/(main)/attestations/[pid]/page.tsx" apps/web/lib/attestations.ts` | R14 现在的措辞是「**产品侧的**调用者只有那一页自己（`page.tsx:72` 的 canonical 链接、`:131` 的自链；`attestations.test.mjs:84/88/89` 也调它，那是单测，不是入口）」；实测命中 `attestations.ts:237`（定义）、`page.tsx:9`（import）/`:72`/`:131`、`attestations.test.mjs:32`（import）/`:84`/`:88`/`:89`——**与报告逐条对得上** |
| ④ nit③（「引文缺结尾 OK 行」）**不采纳** | 读 `tests/security/check-absent-manifest.py` 的 `:357` / `:364` / `:371`；两种调用方式各跑一次 | 那一行只在**非 `--selftest`** 分支打印（那个 checker 的 `:371`），总门注册的是 `--selftest`；两次实跑的末尾形态与报告第 3b 段逐字一致（本次重跑记录见 Gate H 第 3b 段的「本版复核」块）。**一处行号不精确在此点名、不就地改**：报告写 `:358-363`，`grep -n` 实测 `if args.selftest:` 在 **`:357`**、`return 1 if FAILS else 0` 在 **`:364`**，正确区间是 `:357-364`——**这不影响该条的结论，但引文里的行号应更正**，本版按「不许重做」只点名不改 |

### 6.8 本版更正的三处过期行号（附一处**不**更正的）

逐条核对报告里 68 处 `文件:行号` 引用（其中 17 处带符号名）之后，只有三处因 T1211 / T1212 的落地而漂了，
本版按新树更正（**更正的是行号，断言的文字一个字没动**）：

| 位置 | 上一版 | 本版 | 为什么漂 |
|------|--------|------|----------|
| Gate B 的 B2 行 | `tests/e2e/conflict_e2e_test.go:447` | **`:437`** | T1212（`79aaa8d`）把该文件顶部那段本地 `pgReachable` 探针换成 `testdb.RequireDB`。**T1214 订正这行的数值**：本版原文写「文件短了 **13 行**」，那是被删掉的 `pgReachable` 块的块大小，不是文件长度的变化。实测（`git diff --stat 115a4286..HEAD -- tests/e2e/conflict_e2e_test.go`）= `1 file changed, 10 insertions(+), 20 deletions(-)`，**净 −10**；文件行数 633（`115a4286` 与 `b6c1fb1a` 同）→ 623（`a8850f51` 与 `91ed2d5` 同），函数行号正好移了 10（447→437）。原文那句「13 行」在 B2 行与本节是**同一处**（两处同源），一处不漏地改成真数字；`:447` 今天是 `if view.Report.AutoMergeable {` |
| Gate H 第 2 段 | `tests/security/master-security-gate.sh:120` | **`:140`** | T1211 在 `MIN_CHECKS=` 之前加了 20 行（本树实测 `MIN_CHECKS=17`、注册表 17 行） |
| Gate H 的 H4 行 | `Makefile:324` | **`:337`** | **T1214 订正这一格的归因**（上一版这一格**逐字**写的是「T1212 改了 `Makefile` 顶部的 `GO_UNIT_PKGS`，`make a11y` 那一行整体下移 13 行」）。**先把这句引文读准**：它点名的是 **`make a11y`**，而**本节这一格与第 2 节的 H4 行说的是 `make dev`**——两者不是同一条目标。**「下移 13 行」这个数在它自己的区间里是对的**：`make a11y` 实测（`git show <rev>:Makefile \| grep -n '^a11y:'`）在 `9b22339e` / `b6c1fb1a` / `115a4286` / `a8850f51` / `91ed2d5` 五棵树上给 **431 / 441 / 441 / 444 / 444**（从头到尾正好 13 行）；`make dev` 走同一批树（**324 / 334 / 334 / 337 / 337**），也是 13 行（`security-tools:` 那一行在 `9b22339e` 上还不存在，随后四棵树同步 **316 / 316 / 319 / 319**）。**错的不是「13」，是「全归 T1212」**：这 13 行是**两笔任务的插入叠加**——前 **+10** 是 **T1209（`034a341`，`git show --stat 034a341 -- Makefile` = `10 insertions(+)`：`security-tools` / `security-gate` 两个目标与 `dev:` 上方那段注释）**，后 **+3** 才是 **T1212（`a8850f5` 的 Part 3，`GO_UNIT_PKGS` 那段）**，10 + 3 = 13。结论不变：**移了 13 行是对的，13 行全归 T1212 是错的**；`:324` 今天是一个空行，目标本身没动 |

**一处不更正**：Gate H 第 3b 段里 `check-absent-manifest.py` 的 `:358-363`（正确是 `:357-364`）——
那是上一轮复核对象的原文，本版只点名不改（6.7 ④）。**同一节里两处相关数字也一并交代**：
`sast-go` 本树实测 `798 file(s) scanned`（上一版基准是 797；本轮没有去追这个数与 `.go` 文件数
（1236 → 1239）不同口径的原因，判定只看「有这一行、它自己打印 OK、`unbaselined` 为 0」），
`sbom-node` 逐字相同（364 components）。

**自引用推移的两处（第八轮自己造的，同笔更正；T1214 按当前脚本重数）**：6.6 里两条指向**本脚本自己**的行号（nit② 的 `:348`、risk⑤ 的 `:381`）
在第八轮给 `require_marker` 上方补那段注释（见 6.5 表下引文缴械那一节）时随之推移，那一轮改成了它自己树上的 `:375` / `:391`（**第八轮脚本的行号**）；
T1214 重写脚本后按当前脚本重数是 `:413` / `:420-422`（数法）与 `:452-457`（统计句与那条 `require_marker`）。教训与 6.8 开头那三处同一个：**行号引用会漂，改注释也会漂**——补完注释要把脚本里被引用的行号重数一遍。

### 6.9 一句话总结这一版

**重挣的三样**：Gate H（新仪器、本树实跑）、Gate I（本树实跑）、机械块（脚本生成）；
**沿用的**：Gate A–G 与 Development System Gate（驱动路径未变，逐条给了 `git diff --stat` 原文，
唯一两处例外——Gate B 的引用文件与 Development System Gate 面的 CI 管道——都已写清变的是什么）；
**没动的**：任何一条既有断言、任何一处台账、任何一份产品规格。
