# ADR-024：证据断言的读必须显式带上读者；非公开行只给当事项目
Status: Accepted

`evidence_assertions.visibility` 这一列的语义，迁移头部已经写死
（`infra/migrations/00091_external_evidence_network.sql:42-66`）：
它是 **"whether the assertion may be rendered by the PUBLIC network read
(GET /knowledge/{knowledgeId}, which is `security: []`)"**，DEFAULT `'private'`，
理由是 **"an assertion nothing explicitly made public is not rendered anywhere (docs/12 §5)"**。
写路径按同一语义推导（`internal/application/rsg/evidence.go:257-268`）：断言方项目公开、
无可见性策略、且**提交所在分支**公开，三条同时成立才是 `public`，"Anything unclear stays private"。

**本 ADR 定的是一条读的规则：这一列既然是"能不能被公开读渲染"，那么"谁在读"就必须进到读本身里去。**

## 问题：门只问项目，读不问读者

落地这条列的读有两条（`internal/persistence/queries/evidence.sql`）：

- `ListEvidenceAssertionsForTarget`（`:67-73`）——头部逐字写着
  **"Rows are returned unfiltered by visibility: this is the owning project's read."**，
  查询里**没有任何可见性谓词**；
- `ListPublishedEvidenceForTarget`（`:74-109`）——带 `AND ea.visibility = 'public'`，
  是公开网络读（`GET /knowledge/{knowledgeId}`，`internal/persistence/evidence_store.go:186`）。

而 T0506 接上、T0507 又铺到人可读页面的，是**前一条**：

- JSON 路由 `GET /api/v1/projects/{projectId}/objects/{objectId}/evidence`
  （`cmd/api/evidencehttp/wiring.go:63`）：门先跑，**门只决定"这个项目对这个读者可不可见"**，
  之后 `h.service.ObjectEvidence(ctx, projectID, objectID, versionNo)`
  （`cmd/api/evidencehttp/handlers.go:53-60`）——**读者根本没有被传下去**；
- 对象详情页的证据页签（T0507）：`evidencePanelFor` 同样**不接读者**
  （`cmd/api/rsghttp/graph.go:398-404`，读在 `:418`）。页面与 JSON 走同一条
  `handleGetObject` 路由，HTML 分支（`cmd/api/rsghttp/handlers.go:395`）在
  "与项目同可见"的 T0106 读通过之后才渲染。

于是对**公开项目**：匿名读者过门（这一条被测试正面钉死——
`tests/integration/evidence_graph_test.go:644-650` 要求匿名读公开项目 = 200），
拿到的是**不带可见性谓词的行**。T0507 的独立评审在真实 PostgreSQL 上探过：
把一行改成 `visibility='private'`，**匿名的证据页签照样渲染那一行**。

可达数据不缺：断言方是私有项目、或断言的提交分支不公开，写路径就存 `private`
（`internal/application/rsg/evidence.go:257-268`）——这正是"私有组织的内部判断"与
"尚未合并的草稿证据"两种本该只在项目内可见的东西。**公开面渲染它们，等于把 private 读成了 public。**

## 决定

1. **读者是证据读的显式输入。** 读的签名带上读者（或其解析出的身份），
   由读自己决定渲染哪些行——**不是**取回全量再在传输层筛。
2. **渲染谓词（fail-closed）：**一行可以渲染给某读者，当且仅当满足其一：
   - `ea.visibility = 'public'`；或
   - 该读者是**断言方项目**的成员；或
   - 该读者是**目标对象所属项目**的成员。
3. **读者解析不出来、或成员关系查不到时，只给 `public` 行**——与这一列自己的
   DEFAULT `'private'` 同向。判断不了就不渲染，和 `rsg/evidence.go` 的
   "Anything unclear stays private" 是同一条规矩。
4. **两个读出口一次性对齐**：JSON 路由与页面走同一条判据（"一个图一个答案"这条性质
   本来就要求如此，`tests/integration/graph_page_e2e_test.go` 是它的证词）。

成员判据的现成读：`internal/persistence/project_store.go:177`
（`GetMembership(ctx, projectID, userID)`，实现在 `projects.ProjectStore` 上）。

## 为什么是"两方成员的并集"，而不是只给断言方

这条规则**只移走与这一行没有任何已记录关系的读者**（匿名者、以及既不属于断言方也不属于
目标方的登录用户），当事两方的成员一个都不减——既不新增任何可见者，也不从任何人手里
拿走今天已有的东西。目标方维护者看得见，也是 `docs/24 §2` 的既有立场：
**"external evidence 不可被 origin maintainer 静默删除"**——删不掉的前提是看得见。

## 后果

- 证据图服务的读接口要变（`internal/application/evidencegraph/**`），
  两个传输层各自把已经解析好的读者传下去（JSON 侧在 `evidencehttp` 已有 `reader(r)`，
  页面侧 `graph.go` 的面板要接读者）。
- **测试必须能红**：夹具已经能造出这条形状
  （`tests/integration/evidence_graph_test.go:264-270` 的 `mustAssertEvidence(t, projectID, branchID, body)`
  可指定断言方项目与分支；`:153` 已有私有项目 `eg-gamma`），
  所以修复必须附一条"私有项目断言公开对象 → 匿名读看不到那一行、当事方成员看得到"的用例，
  并且这条用例在改动前**必须红**。
- 落地的任务书与调度见 `tasks/decisions.md`（2026-09-19 的 T0507 一节）。

## 尚未决定（留给 owner，本 ADR 不作规定）

1. **一个既能读公开项目、又与该行无关的登录用户**，将来是否该看到非公开行。
   本 ADR 判**不**（fail-closed）；若产品要放开，那是放宽，必须显式记录。
2. 目标方成员看到的是**别人（可能是它并不信任的私有项目）尚未公开的草稿**这一情形，
   是否要收窄到"只有断言方可见"。本 ADR 保持今天的答案（两方成员都可见），
   因为收窄它属于科研语义/隐私决策，不由本 ADR 代 owner 决定。
3. `contribution_events` 是否需要自己的可见性轴（见 `tasks/progress.md` 的 owner 清单）。
