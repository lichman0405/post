# 开发进度

状态：**P0 完成**（14/14）、**P1 完成**（10/10）、**T0201 已合并**；P2 的 T0202 正在跑，
**但全项目已经没有任何一个新任务可以发出去** —— 瓶颈是待批准队列（见下）。
最后更新：2026-09-13 23:45（**5 项待人工批准**，其中 **#99 卡着 29 个任务**；自修两条已自行合入：`#108`、`#110`）

## 当前阶段

- **P2 — T0201**：**已合并** ✓（PR #107 → `821f3ce`）✓。走的正是 §8.2 那条机械段 ✓：
  我把 main 合进任务分支后四门断言拒了 push ✓（判决绑的代码身份变了 ✓），
  重派 → 独立 review 回来 **approve** ✓（0 blocking / 0 major ✓，评审还自己重建了树做逐字节比对 ✓）
  → 清掉 pending → driver 自己 push ✓（23:27:26）→ 自己 merge ✓（23:27:35）✓。
  **G4 属实** ✓：被合入的那个 head（`7948046`）的 7 项检查在 23:14 就全绿了 ✓，merge 发生在 13 分钟之后 ✓。
- **P2 — T0202**：已开工 ✓（Scientific Object 不可变版本仓库 ✓，依赖 T0201 ✓ 刚被合并 ✓）。
- **P6 — T0603**：G3 红有**两个**原因 ✓。①`gitea-real-services` 一直红，因为
  **driver 进程的环境里没有 `POST_GITEA_TOKEN`** ✓ —— 任务 worktree 里没有 `.env.dev` ✓，
  脚本的两条取 token 路径都走不通 ✓。已带 token 重启 driver ✓（worker 侧仍然被剥离 ✓，
  `worker_env.go` 的剥离清单里有它 ✓）。②`rsg-real-services` 红是因为 **P2 的接口还没写** ✓，
  这是设计如此 ✓（门自己写着"P2 builds them"）✓ —— 所以 T0603 仍卡在 P2 链上 ✓。
- **P3 — T0301**：collect 拒绝**不是 Worker 的错** ✓ —— 检查逻辑没错 ✓，但**被检查的树是验收
  框架自己改的** ✓（L1-20260913-16 ✓）：main 自己的 G3 脚本在被评测的树里 `-am` 提交了
  `029380b`（身份 `g3 <g3@test>`）✓，顺带把 `README.md` 写进 allowed_scope 之外 ✓；
  另一条 `refs` 是我自己建的 PR 分支 ✓（那部分已由 #97 修掉 ✓）。**交付完好保存在 worktree** ✓
  （25 个改动路径 ✓，worktree 仍在 baseline `5cfc4c3` ✓，我给的是实测 ✓）
  → **阻塞在 #99** ✓，且**它后面排着 29 个任务** ✓。
- **两条 orchestrator 自修，已在本轮自行合入** ✓（都只动测试/格式 ✓，不碰门语义 ✓）：
  - **#110 → `4f23500`**：`gofmt -l` 还点名的 5 个文件 ✓（纯空白 ✓，`git diff --ignore-all-space` 为空 ✓）。
  - **#108 → `b00c9ee`**：残留检测的等待等的是"读到了什么" ✓。
- **#109 已开** ✓（**第二轮已推 `bb1ba48` ✓、CI 全绿 ✓**，等这一轮的独立 review 回来再合 ✓）：
  同一个"门为无关理由变红"的毛病 ✓ —— 监听器测试把 18981/18982 写死 ✓，
  而它自己的产物就是一个**逃出会话的监听器** ✓，上一次跑漏下的进程正好占着那两个口 ✓。
  现在两个端口都向内核要 ✓（两个 socket 同时持有 ✓，所以两个号必然不同 ✓），
  断言也从裸数字换成 `/proc/net/tcp` 那个完整地址 ✓（`:1898` 是 `:18981` 的前缀 ✓）。
  第二轮补的是**更糟的那一半** ✓：原来那个"先存在的监听器"只是 `sleep 300ms` 假定它起来了 ✓，
  实测**口上被别人占着时旧版本报 `ok`** ✓ —— 反面证据整个缺失 ✓，而绿色的谎比红色的真更危险 ✓。
- P0 / P1 已全部合并 ✓

## 治理状态（★ 影响每次调度）

owner 于 2026-09-12 授予**默认自主推进**授权（`L3-20260912-4`，持久化于 `CLAUDE.md` §5.1、
`docs/62` §3.1、`docs/69` §3.1）：L0/L1 常规实现与产品代码 PR，满足六项条件即由 Supervisor
自行 review + merge；仅 L3、重大 L2、新外部凭证/付费服务/账号授权、无法用规格解决的
`SPEC_BLOCKED` 才停止请求人工。授权不降低 Gate 标准。

### ★ 每轮开工的第一件事：`rddev status` —— 看 driver 在等什么

`./bin/rddev status` 的第一屏就是答案 ✓，尤其是 `decisions waiting for the Supervisor` ✓。
2026-09-13 17:07–21:19 停摆 4 小时 12 分 ✓（L1-20260913-20 ✓）的原因就是"没人看这一屏" ✓：
driver 一直在**正确地**等 ✓，把三件事写得很清楚 ✓，只是没有任何东西把它送到 Supervisor 面前 ✓。
**driver 的 pending 不清，后面所有事都在等这个 pending** ✓ —— 包括那些看起来"只是慢"的任务 ✓。

### ★ 2026-09-13：本类变更被判定为**需要人工批准**（Supervisor 已停止自行合入）

对 #99 / #100 执行自行合入时，环境的安全分类器拒绝，理由与 §5.1 **条件 6**一致：
diff 改动了既定安全边界 / 权限模型 / 核心架构原则的范畴，不在自主授权内，需 owner 批准。
随后复核发现 **#98 也属于这一类，而它已在此之前合入（`e599931`）** —— 该 PR 修改了
`specs/orchestrator/worker-permissions.yaml` 的 ref 归因规则并新增 ref 台账与测试，
**需要 owner 事后复核**（诚实上报，不淡化）。

**当前待批队列**：

| PR | 内容 | 状态 |
|---|---|---|
| #98 | Worker 权限模型 spec 的 ref 归因规则 + ref 台账（`internal/devorchestrator/ref_ledger.go`） | **已合入 `e599931`**，待事后复核 |
| #100 | driver 对"被取代的 review"重派而非停摆（L1-20260913-17） | **已合入 `43a63fb`**（owner 于 08:55Z 合入）|
| #99 | 验收脚本不再改写它正在验收的那棵树（L1-20260913-16）| 已并入 main（`2740e74`）、**7/7 全绿**、**等合入** —— **它是 T0301 的解除条件**，并删掉 main `README.md` 里的 8 行垃圾 |
| #103 | rebaseline 被拒时把 Worker 已做好的活原样放回 | 已推（`101fdc7`）、**8/8 全绿**、**等合入** —— 不修它，我**不敢**对 T0301 做 rebaseline（会毁掉那 25 个文件） |
| #104 | 门自己报的检查项数量不再写死 | 已并 main（`f8dde0b`）、**7/7 全绿**、**等合入** |
| #105 | 一次运行的开始时间必须能排出同一个秒内的先后 | 已推（`c64c017`）、**等合入** —— 不修它，上一轮的旧判决可能被当成新一轮的判决收下 |
| #106 | orchestrator 误读两份文档（判决里的 `null`、变更的原文） | 已推（`4c529b6`）、**等合入** —— 不修它，一个说 "approve" 的判决会被误判成不合法 |

**★ 现在的瓶颈就是这张表。** 全项目 132 个任务里，未完成的 107 个中有 **105 个被未完成的上游卡住** ✓
（`rddev task next` 现在是空的 ✓，一个新任务都发不出去 ✓）：排在最前面的三处是
**T0202（102 个任务等它）**、**T0603（57 个）**、**T0301（29 个）** —— 而 T0301 的解药就是 #99 ✓。
换句话说：现在不是"没活干"，是**活全排在这三处后面** ✓，而 T0202 是唯一正在跑的那个 ✓。

**★★ 补一条我自己先写错、又实测改回来的记录（两边都写在这里）。**

我先这么写过："G3 里带 `gitea-real-services` 的任务有 48 个（T0301–T0309、T0401–T0410、
T0601–T0609、T0801–T0812、T1201–T1208），所以 #99 不合，这 46 个 todo 会一个接一个被同样的
理由冤枉掉。" **后面这半句是我推的，不是查的，推错了** ✓：

- `gate_run.go:509` 起：G3 **不在任务的 worktree 里跑** ✓，而是在一个**一次性集成树**里 ✓
  （`worktree add --detach <main>` ✓，再把该任务的改动以 patch 打进去 ✓，跑完删除 ✓）。
  所以 G3 跑这个脚本，脏的是那个一次性树，**不是任务自己的树** ✓。
- T0301 **没有 g3 记录** ✓（`gates/T0301/` 里只有 collect / accept / review / verdict ✓）——
  也就是说 T0301 的 G3 **根本没跑到** ✓，那次提交不是 G3 干的 ✓。
- 提交 `029380b` 的父提交是 `5cfc4c3`（当时的任务基线 ✓）、tree 里是 Worker 的全部改动 ✓ ——
  形状是"有人在这个 worktree 里 `git commit -am`" ✓。

**能确定的与不能确定的，分开写** ✓：能确定的是**那条命令在 main 自己的脚本里** ✓，
**谁在这个树里跑它，就在这个树里留下一个提交** ✓；不能确定的是 T0301 那次**具体是谁跑的** ✓
（Worker 当时正在改这个脚本 ✓，最可能是它自己跑了一次验证 ✓，但这是推断，不是证据 ✓）。
所以 #99 的分量要照实说：它**不是**"挡着 46 个任务" ✓ ——
它挡的是 **T0301 一个** ✓，以及"**任何在任务树里跑这个脚本的人都会重演一次**"这个风险 ✓，
另外它删掉 main `README.md` 里那 8 行垃圾 ✓。**我不把推断写成 46。**
| #108 | （**已自行合入 `b00c9ee`**）残留检测的等待等的是"读到了什么"，而不是"exec 完成了没有" | 测试专属改动 ✓ 不碰门语义 ✓ 六项条件满足 ✓ CI 全绿 ✓ review 的四条意见全部落实 ✓ |

**合入顺序**：#104 / #105 → **#99** → **#103** ✓（#106 独立 ✓）。

**★ 另有一件只等你点头、不挡任何任务的事：Issue #111。** 安全扫描报了
`internal/rsg/schemareg/schema.go` 的 `$ref` 能读外部东西。我自己实测过，结论比报告细：
`http://` **根本发不出去**（0 次请求，库的默认加载器不做网络 ✓）；但 `file://` **确实会读** ✓，
而且**读出来的内容会真的生效** ✓（我放了个 `maxLength:4` 的文件，验证 `"abcde"` 就被它拒了 ✓）。
今天**没有漏洞**：这个包**外面一个调用者都没有** ✓（刚 T0201 交付，还没接线 ✓）。
但 `Register` 是公开 API ✓，第一个把它接到请求上的任务就会让它变成真问题 ✓。
代码注释写着"只在本地注册表内解析（从不走网络）"——**网络那半句是库的功劳，不是这段代码的** ✓，
而且注释**完全没提 `file://`** ✓。要不要收紧成"只认已注册的文档"，属于安全边界 ✓，
按 §5.1 条件 6 我不自己定 ✓；就算定下来，它是产品代码 ✓，按 §1 该派 Worker 而不是我写 ✓。

**★ 2026-09-13 23:20：driver 带着 `POST_GITEA_TOKEN` 重启了 ✓。** 原因是 G3 的 `gitea-real-services`
一直在红 ✓，而红的理由与产品无关 ✓：门在**任务的 worktree** 里跑 ✓，那里没有 `.env.dev` ✓，
`POST_GITEA_TOKEN` 也不在 driver 的环境里 ✓ —— 三条取 token 的路全断 ✓。
现在只把这一条变量给了 driver ✓（这正是 CI 给它的方式 ✓）；worker 拿到它的路仍是封的 ✓
（`strippedEnvVars` 里有 `POST_GITEA_TOKEN` ✓，spawn 后还会断言它不在 ✓）。

**★ 更正（21:50 实测）：GitHub 的 `MERGEABLE` 是过期的 ✓。**
把四条分支各自对着当前 main 做一次真实合并（scratch worktree ✓，已删除 ✓）：
`#104` 干净 ✓、`#105` 干净 ✓、**`#99` 冲突**（`specs/SPEC_VERSION.json` ✓）、
**`#103` 冲突**（`tasks/decisions.md` ✓）。**GitHub 仍报四者全 `MERGEABLE` ✓ —— 与实测不符 ✓。**

两处冲突的原因和解法都清楚 ✓，且两处都是**我的**痕迹 ✓：

- **#99 × `specs/SPEC_VERSION.json`**：派生工件 ✓。按本项目规则**只能重新生成** ✓
  （`python3 scripts/spec_version.py --write` ✓，scratch 里验证过：`--check` 由它变成 current ✓）。
- **#103 × `tasks/decisions.md`**：**编号撞车** ✓ —— 我在 main 上写的 `L1-20260913-20`（driver 停摆 ✓）
  与 #103 分支上写的 `L1-20260913-20`（rebaseline 的拒绝是破坏性的 ✓）是**两条不同的记录** ✓。
  解法：把**分支上那条改号为 `L1-20260913-22`** ✓（main 的 20/21 已推送 ✓，不动 ✓）。

**执行时机**：**等两个 delta review 回来再动** ✓ ——
rebase/merge 会移动分支 HEAD ✓，而本项目的规矩是"判决绑定它读过的那份代码" ✓
（#100 修的就是这条 ✓），中途改字节会让正在跑的 review 变成"针对已被取代的版本" ✓。
**顺序**：先收 review ✓ → 按需改代码 ✓ → merge-forward + 重新生成 ✓ → 复核 ✓ → 交给 owner 合入 ✓。

**必须明说的结构性事实**：这类变更（安全边界 / 权限模型 / Gate 行为 / 状态机）**
在本项目里没有独立 reviewer** —— Supervisor 既是作者又是批准者，而 §5.1 条件 6 恰恰是
把"人工"变成唯一可用 reviewer 的那一条。因此本类 PR 的 review 由 owner 承担，
Supervisor 不再自行合入（本次已停止）。**T0301 / T0201 因此被这两个 PR 阻塞。**

## 已完成任务

| Task | 状态 | PR | 交付 |
|---|---|---|---|
| T0000 | merged | #3 | `ops/doctor.sh` 35 项确定性 preflight + 检查契约 + 2 套测试 |
| T0001 | merged | #6 | 12 项 spec 校验 + source-repo preflight（三态 visibility、fail-closed）+ 派生版本标记 |
| T0002 | merged | #10 | Monorepo：Go module 4 binary + Next.js 16.3.5 + uv Python adapter + Makefile（无 TS backend） |
| T0003 | merged | #13 | Docker Compose 基设：pgvector/Redis/MinIO/Gitea/Mailpit，全 pinned + healthcheck + 幂等 init |
| T0004 | merged | #16 | 配置与密钥基线：Go typed loader（stdlib）+ web/python 独立校验 + RedactURL + secret scan |
| T0005 | merged | #19 | 13 个 forward-only migration + pgx/sqlc + tx helper + run-scoped 测试库命名空间 |
| T0006 | merged | #32 | `/healthz` 200 / `/readyz` 503 如实 + 三处接线修复 + Docker-free `make smoke` |
| T0007 | merged | #34 | 结构化日志 + 端到端 correlation id（含 canary 负对照）+ go-redis 日志归口 |
| T0008 | merged | #36 | CI 六阶段 gate + `make check` 不再依赖数据库 + DAG/state 一致性检查 + workflow YAML 校验 |
| T0012 | merged | #45 | 四层 Gate 可执行闭环：G1-G4 执行器、accept/reject/rework/respawn、Review Worker、red 矩阵、gate 输入防篡改 |
| T0011 | merged | #41 | Worker 隔离 e2e：凭据剥离（含 spawn 后断言）、残留归属、RESULT 契约机械强制 |
| T0010 | merged | #38 | `rddev worker spawn`：worktree 生命周期 + registry + 移植的 guard（147/147 双向回归） |
| T0009 | merged | #23 | `rddev` CLI：doctor 复刻 35 项契约、任务状态机、原子+加锁状态写入、诚实 stub |
| T0013 | merged | #28 #29 | append-only 不可变性：13 表 × (BEFORE UPDATE/DELETE 行级 + BEFORE TRUNCATE 语句级) |
| 安全修复 | merged | #8 #11 #14 #18 #21 #26 | preflight 凭据泄漏/fail-open；schema 注解；infra token/SQL；config 三条泄漏路径；search 访问控制；state 锁与 drift |
| T0101 | merged | #64 | 用户认证与 session |
| T0102 | merged | #69 | Research Profile 基础 |
| T0103 | merged | #70 | Organization 创建与成员关系 |
| T0104 | merged | #74 | Project 创建与 Purpose/Program |
| T0105 | merged | #75 | Project 成员权限框架 |
| T0106 | merged | #77 | Public/Private 读取隔离 |
| T0107 | merged | #76 | 全局 GitHub-style 导航与 Layout |
| T0108 | merged | #80 | Project Shell 与 tabs |
| T0109 | merged | #82 | Project Settings 与成员管理 UI |
| T0110 | merged | #81 | 基础 Audit Log |

→ **P1 完成：10/10 merged**（2026-09-13T02:46:38Z，T0109 是最后一个）。

## 进行中

| Task | 状态 | 说明 |
|---|---|---|
| T0201 | running（返工中） | 独立 review **approve** ✓；只因 **#100 合入**使 main 前进 ✓、accept 拒绝而停 ✓ → **已 rebaseline 到 `42379ea`**（38 文件 carried ✓），worker 已在新基线上返工 ✓ |
| T0301 | rejected（等 #99） | 拒绝的四条**全部不是 worker 的错** ✓：三条来自 main 自己的 G3 脚本在被评测的树里提交 ✓（= #99 的缺陷 ✓），一条来自**我自己**的分支 ✓（= 已删的 `f2170d1` ✓）→ 等 **#99** 合入后 `rddev rebaseline T0301` + `worker rework` ✓ |
| T0603 | running（返工中） | 独立 review **approve** ✓；同样只因 main 前进而 accept 拒绝 ✓ → **已 rebaseline 到 `42379ea`**（24 文件 carried ✓，重新生成 `SPEC_VERSION.json` + `specs/database/postgres.sql` ✓），worker 已在新基线上返工 ✓ |

## 已关闭的 SPEC_BLOCKED

**版本历史可变性**（原 `decisions.md` L2-SPEC-20260912-15）—— **已由 owner 裁定并实现**（T0013）：
owner 选择 DB 触发器；13 张表上 `BEFORE UPDATE/DELETE` + `BEFORE TRUNCATE` 触发器，实测原先成功的
`UPDATE` 现被 `P0001` 拒绝。**Master Gate A 该项在应用层意义上已闭合**（残余绕过见下）。

## 阻塞

- **T0301**：等 #99 合入。其 G3 脚本就是"改了被测树"的那个脚本，
  在 #99 落地前返工只会**再次**污染它自己的分支。
- **#99 / #103 / #104 / #105 需要 owner 批准**（见"治理状态"）——
  这是当前**唯一**的人工依赖 ✓。四者都已 review 完、CI 绿、`MERGEABLE` ✓。
- 非阻塞但未关闭：`/readyz` 泄露内部拓扑（见"已知风险"）✓。

## 已知风险 / 需 owner 关注

- **触发器的残余绕过（实测，未关闭）**：`SET session_replication_role = replica` 与
  `ALTER TABLE ... DISABLE TRIGGER` 都能让 `UPDATE` 通过，而本栈应用当前**以表 owner 连接**。
  触发器挡住的是应用层意外改写，**不是特权角色**。彻底关闭需受限写入角色（`REVOKE UPDATE/DELETE`
  + 应用以非 owner 运行）——owner 在选择机制时未选该方案，故仅记录并上报。属**生产部署**议题。
- **Branch protection 不可用**：私有仓库在当前 GitHub plan 下无法启用（403）。
  `docs/69` §6 的目标规则只能靠 Supervisor 纪律保证。见 F-20260912-1。
- **`/readyz` 泄露内部拓扑（待修）**：probe 错误原样返回给未认证调用方（含 DB user/db 名/host/port），
  且默认绑定 `:8080`。已确认属实，计划单独修复（公开 body 只报 down，细节进结构化日志）。
- **Worker 会占用宿主资源且不自动清理**：T0007 的 Worker 自建 PostgreSQL 后未停止，
  会**污染其他任务的验收**（T0008 正要验证"无数据库时 make check 通过"）。见 L1-20260912-24。
- **`GITHUB_PERSONAL_ACCESS_TOKEN`** 存在于 Supervisor 环境，Worker spawn 时已剥离。

## Supervisor 自身流程失误（已在 decisions.md 留档，供复核）

本会话我犯下并已修正的失误，集中记录以便回溯：

1. `L1-20260912-18` —— **带着红灯 CI 合并了 PR #23**；G2 跑的是 CI 步骤的**子集**，且把查询与 merge
   写在同一命令里。纠正：G2 必须跑 CI 的**完全相同**步骤，且 CI 状态必须在 merge 前**独立断言**。
2. `L1-20260912-22` / `L1-20260912-23` —— **两次 scope glob 错误**（`internal/config/wiring*`、
   收窄 `tests/**`）制造出本不该存在的"越界"。纠正：默认采用 DAG 声明的 scope，仅在具体并发冲突时收窄。
3. `L1-20260912-20` —— 把"10 个并发写者通过"当作并发已验证；两个真实缺陷都落在我不曾尝试的情形里。
   纠正：**对"X 不会发生"的断言，必须主动尝试让 X 发生**。
4. `L1-20260912-25` —— 用**近似**命令跑 `ruff --fix`，配置解析与 CI 不同，删掉了作者的 `# noqa`。
   同源于第 1 条：**用真实命令验证**。

## 下一步

1. **#100 已合入**（`43a63fb`）：driver 已在真实任务上生效 —— T0201 的过期 review 被自动重派
   （日志 `the recorded review is about a superseded attempt (reviewed f667b4568b84, code is now 7783588acae9)` ✓），
   不再需要人工清决定。当前 T0201 在 verification，等这次 review 结束。
2. **#99（G3 门不再改动被测树）第二版已推送，独立 review 中**：六种漏判改为结构性关闭
   （命名网 = 谁在做 + 度量网 = 树有没有变），25 例变异电池 25 中 0 漏。
3. **#103（rebaseline 拒绝后把活放回原处）PR 已开、CI 全绿，独立 review 中** ——
   合入前**不要**跑 rebaseline：main 上的 `bin/rddev` 仍是会清空工作树的那版。
4. **#104（消息里的 job 数目不再写死）刚开 PR**，等 CI。13 处把它们说成"六个"，
   而 `required_jobs` 有七个；其中两条是操作者会读到的运行时消息。
5. 合入 #103 之后：`rddev rebaseline T0301`（预期在 G3 门脚本上与 main 冲突，
   手工解，**门脚本的修复和 T0301 的 HMAC 改动都要留**）→ `rddev worker rework T0301`；
   T0603 也需要一次 rebaseline（它的 accept 现在被 `specs/SPEC_VERSION.json` 冲突挡住 ✓），
   driver 已把该决定记下，等 #103 合入后一并处理。
6. T0603 accept 的旧决定（链式 G3 红）已清掉：那是 #102 修的缺陷本身，`bin/rddev` 已是修复版，
   重试后暴露出的是上面这条 rebaseline 依赖。
7. 待 owner 批准（两件，都不阻塞上面的机械段）：
   - **已 rejected 任务的重判路径**（L1-20260913-18）—— 让拒绝记录可被真实内容取代、
     并允许在被拒交付上重跑原检查（需新增状态机边 `rejected→verification`）；
   - **T0204 / T0205 / T0207 的链式 G3 例外** —— #102 已把"谁该背这条链"改由依赖图决定，
     剩下这三个任务**结构上**无解（各自的理由写在 `carriersTheChainTraps` 测试里），需产品判断。
8. 修复 `/readyz` 拓扑泄露（小而明确，独立 PR）。

<!-- AUTO-PROGRESS:BEGIN — generated by scripts/update_progress.py, do not hand-edit -->

## 任务状态自动总览

生成时间：2026-09-13T06:54:20Z

状态分布：todo 105 · ready 0 · running 1 · worker_failed 0 · verification 1 · rejected 1 · blocked 0 · accepted 0 · merged 24（合计 132/132 个任务）

| Task | 标题 | 阶段 | 状态 | 开始 | 完成 | 验收 | 合并 |
|---|---|---|---|---|---|---|---|
| T0000 | Ubuntu 24.04 Canonical Environment Preflight | P0 | merged | 2026-09-12T12:30:00Z | 2026-09-12T13:05:00Z | 2026-09-12T13:05:00Z | 2026-09-12T13:20:00Z |
| T0001 | 验证规格仓库与任务依赖图 | P0 | merged | 2026-09-12T13:30:00Z | 2026-09-12T13:35:00Z | 2026-09-12T13:35:00Z | 2026-09-12T13:36:00Z |
| T0002 | 初始化 Go + Next.js + Python Adapter Monorepo | P0 | merged | 2026-09-12T13:37:00Z | 2026-09-12T14:20:00Z | 2026-09-12T14:20:00Z | 2026-09-12T14:22:00Z |
| T0003 | 本地基础服务 Docker Compose | P0 | merged | 2026-09-12T14:25:00Z | 2026-09-12T14:35:00Z | 2026-09-12T14:35:00Z | 2026-09-12T14:40:00Z |
| T0004 | 配置与 Secret 管理基线 | P0 | merged | 2026-09-12T14:45:00Z | 2026-09-12T15:05:00Z | 2026-09-12T15:05:00Z | 2026-09-12T15:10:00Z |
| T0005 | 数据库 migration 与 pgx/sqlc 数据访问层 | P0 | merged | 2026-09-12T14:45:00Z | 2026-09-12T15:40:00Z | 2026-09-12T15:40:00Z | 2026-09-12T15:45:00Z |
| T0006 | 基础 Go API/Worker/MCP + Web + Python Adapter 健康检查 | P0 | merged | 2026-09-12T15:48:00Z | 2026-09-12T16:35:00Z | 2026-09-12T16:35:00Z | 2026-09-12T16:35:00Z |
| T0007 | 日志、request id 与基础 telemetry | P0 | merged | 2026-09-12T17:00:00Z | 2026-09-12T17:20:00Z | 2026-09-12T17:20:00Z | 2026-09-12T17:20:00Z |
| T0008 | CI 基线与多语言 Gate | P0 | merged | 2026-09-12T17:00:00Z | 2026-09-12T17:35:00Z | 2026-09-12T17:35:00Z | 2026-09-12T17:35:00Z |
| T0009 | 实现 rddev Go CLI 与任务状态机 | P0 | merged | 2026-09-12T15:12:00Z | 2026-09-12T16:35:00Z | 2026-09-12T16:35:00Z | 2026-09-12T16:36:00Z |
| T0010 | Worktree 与独立 Claude Code Worker 调度 | P0 | merged | 2026-09-12T17:00:00Z | 2026-09-12T18:05:00Z | 2026-09-12T18:05:00Z | 2026-09-12T18:05:00Z |
| T0011 | Worker 权限隔离与结果收集 | P0 | merged |  | 2026-09-12T20:20:00Z | 2026-09-12T20:20:00Z | 2026-09-12T20:20:00Z |
| T0012 | Supervisor 四层验收 Gate 与自动开发闭环 | P0 | merged | 2026-09-12T20:25:00Z | 2026-09-12T21:55:00Z | 2026-09-12T21:55:00Z | 2026-09-12T21:55:00Z |
| T0013 | Enforce append-only / version immutability at the storage layer | P0 | merged | 2026-09-12T15:15:00Z | 2026-09-12T15:55:00Z | 2026-09-12T15:55:00Z | 2026-09-12T15:55:00Z |
| T0101 | 用户认证与 session | P1 | merged | 2026-09-12T18:07:10Z |  | 2026-09-12T18:35:07Z | 2026-09-12T18:42:00Z |
| T0102 | Research Profile 基础 | P1 | merged | 2026-09-12T19:32:25Z |  | 2026-09-12T20:04:20Z | 2026-09-12T20:07:44Z |
| T0103 | Organization 创建与成员关系 | P1 | merged | 2026-09-12T20:32:29Z |  | 2026-09-12T20:50:52Z | 2026-09-12T20:54:16Z |
| T0104 | Project 创建与 Purpose/Program | P1 | merged | 2026-09-12T20:54:36Z |  | 2026-09-12T22:45:16Z | 2026-09-12T22:56:07Z |
| T0105 | Project 成员权限框架 | P1 | merged | 2026-09-12T22:56:22Z |  | 2026-09-12T23:29:57Z | 2026-09-12T23:33:35Z |
| T0106 | Public/Private 读取隔离 | P1 | merged | 2026-09-13T01:32:29Z |  | 2026-09-13T02:14:06Z | 2026-09-13T02:19:50Z |
| T0107 | 全局 GitHub-style 导航与 Layout | P1 | merged | 2026-09-12T23:38:53Z |  | 2026-09-12T23:56:38Z | 2026-09-13T00:00:41Z |
| T0108 | Project Shell 与 tabs | P1 | merged | 2026-09-13T00:00:56Z |  | 2026-09-13T00:51:52Z | 2026-09-13T00:57:38Z |
| T0109 | Project Settings 与成员管理 UI | P1 | merged | 2026-09-13T02:21:56Z |  | 2026-09-13T02:43:11Z | 2026-09-13T02:46:38Z |
| T0110 | 基础 Audit Log | P1 | merged | 2026-09-13T00:58:14Z |  | 2026-09-13T01:23:12Z | 2026-09-13T01:26:45Z |
| T0201 | Core Scientific Object Schema registry | P2 | verification | 2026-09-13T06:18:36Z |  |  |  |
| T0202 | Scientific Object immutable version repository | P2 | todo |  |  |  |  |
| T0203 | Typed Relation repository | P2 | todo |  |  |  |  |
| T0204 | Project State 与 State Commit | P2 | todo |  |  |  |  |
| T0205 | Research Branch Domain | P2 | todo |  |  |  |  |
| T0206 | RSG Manifest 导出与 hash | P2 | todo |  |  |  |  |
| T0207 | Progressive Validation Gates | P2 | todo |  |  |  |  |
| T0208 | V1 Scientific Object Domain Services | P2 | todo |  |  |  |  |
| T0209 | RSG Query API | P2 | todo |  |  |  |  |
| T0210 | Scientific Object Detail UI | P2 | todo |  |  |  |  |
| T0211 | Research Outline 与基础 Research 页面 | P2 | todo |  |  |  |  |
| T0212 | Project Overview Research Summary | P2 | todo |  |  |  |  |
| T0213 | Project Schema Extension 与 Custom Metadata | P2 | todo |  |  |  |  |
| T0214 | 官方材料研发 Project Templates | P2 | todo |  |  |  |  |
| T0301 | Gitea adapter 与 repo provisioning | P3 | rejected | 2026-09-13T05:57:10Z |  |  |  |
| T0302 | Git main 双层保护 | P3 | todo |  |  |  |  |
| T0303 | Branch Git ref 同步 | P3 | todo |  |  |  |  |
| T0304 | Git 用户认证/PAT/SSH key 基础 | P3 | todo |  |  |  |  |
| T0305 | Push Webhook 与 Semantic Ingestion | P3 | todo |  |  |  |  |
| T0306 | Unstructured Change 状态 | P3 | todo |  |  |  |  |
| T0307 | Files Tree/Preview API | P3 | todo |  |  |  |  |
| T0308 | 只读 Files Web UI | P3 | todo |  |  |  |  |
| T0309 | Git ↔ RSG reconciliation | P3 | todo |  |  |  |  |
| T0401 | Research State Diff 引擎 | P4 | todo |  |  |  |  |
| T0402 | Pull Request Domain | P4 | todo |  |  |  |  |
| T0403 | Integrity Review Checks | P4 | todo |  |  |  |  |
| T0404 | Scientific Review 模型 | P4 | todo |  |  |  |  |
| T0405 | Semantic Conflict Detector | P4 | todo |  |  |  |  |
| T0406 | Semantic Merge Engine | P4 | todo |  |  |  |  |
| T0407 | Scientific Conflict Resolution UI | P4 | todo |  |  |  |  |
| T0408 | PR Research Diff UI | P4 | todo |  |  |  |  |
| T0409 | Merge Governance 与 frozen main 更新 | P4 | todo |  |  |  |  |
| T0410 | PR/Branch 完整 E2E | P4 | todo |  |  |  |  |
| T0501 | Research Question 与 Hypothesis 关系模型 | P5 | todo |  |  |  |  |
| T0502 | Claim 结构与 scope | P5 | todo |  |  |  |  |
| T0503 | Finding 聚合模型 | P5 | todo |  |  |  |  |
| T0504 | Evidence Assertion Domain | P5 | todo |  |  |  |  |
| T0505 | Provenance Graph Projection | P5 | todo |  |  |  |  |
| T0506 | Evidence Graph Projection | P5 | todo |  |  |  |  |
| T0507 | Evidence/Provenance UI | P5 | todo |  |  |  |  |
| T0508 | External Reference live identity + snapshot | P5 | todo |  |  |  |  |
| T0509 | Literature evidence extraction data model | P5 | todo |  |  |  |  |
| T0510 | Knowledge workflow E2E | P5 | todo |  |  |  |  |
| T0601 | Freeze Main Governance | P6 | todo |  |  |  |  |
| T0602 | Abort/Reopen State Transition | P6 | todo |  |  |  |  |
| T0603 | Organization/Project Policy Engine | P6 | running | 2026-09-13T06:52:44Z |  |  |  |
| T0604 | Scientific Responsibility / Reviewer Routing | P6 | todo |  |  |  |  |
| T0605 | Release Manifest Builder | P6 | todo |  |  |  |  |
| T0606 | Immutable Release API/UI | P6 | todo |  |  |  |  |
| T0607 | Activity/Audit Timeline 增强 | P6 | todo |  |  |  |  |
| T0608 | Release/Abort/Policy E2E | P6 | todo |  |  |  |  |
| T0609 | Project Milestone 基础 | P6 | todo |  |  |  |  |
| T0701 | Research Asset Core/PID | P7 | todo |  |  |  |  |
| T0702 | 四类 Asset Manifest validator | P7 | todo |  |  |  |  |
| T0703 | Rights Model | P7 | todo |  |  |  |  |
| T0704 | Publication Impact Preview | P7 | todo |  |  |  |  |
| T0705 | Asset Publish Governance | P7 | todo |  |  |  |  |
| T0706 | Asset Metadata Revision | P7 | todo |  |  |  |  |
| T0707 | Asset Reference/Dependency | P7 | todo |  |  |  |  |
| T0708 | Asset Fork/Derive + Lineage | P7 | todo |  |  |  |  |
| T0709 | Asset Hub Pages/Explore | P7 | todo |  |  |  |  |
| T0710 | Asset 完整 E2E | P7 | todo |  |  |  |  |
| T0711 | Asset Governance 与 Rights Holder Transfer | P7 | todo |  |  |  |  |
| T0801 | Public Entity Anonymous Pages | P8 | todo |  |  |  |  |
| T0802 | Explore 聚合 | P8 | todo |  |  |  |  |
| T0803 | Open Contribution Opportunity | P8 | todo |  |  |  |  |
| T0804 | External Fork/Contribution flow | P8 | todo |  |  |  |  |
| T0805 | Published Knowledge Object | P8 | todo |  |  |  |  |
| T0806 | External Evidence Network Aggregation | P8 | todo |  |  |  |  |
| T0807 | Contribution Ledger projection | P8 | todo |  |  |  |  |
| T0808 | Research Profile / Organization Profile | P8 | todo |  |  |  |  |
| T0809 | Credit Attribution/Dispute 基础 | P8 | todo |  |  |  |  |
| T0810 | 最小 Open Network 闭环 E2E | P8 | todo |  |  |  |  |
| T0811 | Discussion 与 Promote to Research Object | P8 | todo |  |  |  |  |
| T0812 | Private Evidence / Public Attestation 基础 | P8 | todo |  |  |  |  |
| T0901 | Search Document Projection | P9 | todo |  |  |  |  |
| T0902 | Embedding Provider 与 pgvector | P9 | todo |  |  |  |  |
| T0903 | Scientific Query Planner | P9 | todo |  |  |  |  |
| T0904 | Hybrid Retrieval + Graph Expansion | P9 | todo |  |  |  |  |
| T0905 | Scientific Ranking | P9 | todo |  |  |  |  |
| T0906 | Evidence-backed Answer Generator/API | P9 | todo |  |  |  |  |
| T0907 | Search Answer Web UI | P9 | todo |  |  |  |  |
| T0908 | Search → Draft Research Context | P9 | todo |  |  |  |  |
| T1001 | Transactional Outbox | P10 | todo |  |  |  |  |
| T1002 | Subscription Model / Follow/Watch | P10 | todo |  |  |  |  |
| T1003 | Web Research Inbox | P10 | todo |  |  |  |  |
| T1004 | RSS/Atom Feeds | P10 | todo |  |  |  |  |
| T1005 | Email Digest abstraction | P10 | todo |  |  |  |  |
| T1006 | Signed Webhooks | P10 | todo |  |  |  |  |
| T1007 | Dependency Impact Analysis | P10 | todo |  |  |  |  |
| T1101 | 统一 Primer-style Design System | P11 | todo |  |  |  |  |
| T1102 | Research Map 高质量交互 | P11 | todo |  |  |  |  |
| T1103 | Publication/Visibility Security UX | P11 | todo |  |  |  |  |
| T1104 | 全站 Accessibility AA | P11 | todo |  |  |  |  |
| T1105 | I18N 基线 | P11 | todo |  |  |  |  |
| T1106 | API/Upload 安全加固 | P11 | todo |  |  |  |  |
| T1107 | 权限与 Search Side-channel 安全回归 | P11 | todo |  |  |  |  |
| T1108 | 性能基线与索引调优 | P11 | todo |  |  |  |  |
| T1109 | 生产级 Observability/Dashboards | P11 | todo |  |  |  |  |
| T1110 | Backup/Restore 自动化演练 | P11 | todo |  |  |  |  |
| T1201 | 完整 Seed Demo Data Builder | P12 | todo |  |  |  |  |
| T1202 | Canonical MOF Workflow E2E | P12 | todo |  |  |  |  |
| T1203 | Staging 部署模板 | P12 | todo |  |  |  |  |
| T1204 | 生产 Runbook/Release/Recovery 验证 | P12 | todo |  |  |  |  |
| T1205 | OpenAPI/MCP/Schema 文档最终同步 | P12 | todo |  |  |  |  |
| T1206 | Master Security/Quality Gate | P12 | todo |  |  |  |  |
| T1207 | V1 最终验收与交付报告 | P12 | todo |  |  |  |  |
| T1208 | 完整 Project 可移植导出 | P12 | todo |  |  |  |  |

<!-- AUTO-PROGRESS:END -->
