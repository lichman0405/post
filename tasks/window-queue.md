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

## A. 落 T0511 的任务书，然后解冻 T0812 —— 唯一的"真待落地"

**为什么**：`tasks/packages/T0511.json` 是**唯一**还真正待落地的任务书
（2026-09-19 用 `.rddev/tools/packages-vs-dag.py` 逐份比对过：26 份包里其余 20 份 DIFFERS
是**过期草稿**，live 那份更新，落了会把更正改回去；详见 `tasks/packages/README.md`）。

**怎么确认还需要做**：`python3 .rddev/tools/packages-vs-dag.py | grep T0511` → 仍是 `NOT IN DAG`。
再看一眼 T0511 是否已经被合并/已经存在（`./bin/rddev task inspect T0511`）。

**步骤**（顺序不能乱，每一步都在 `land-T0511.py` 的头注释里）：

```sh
python3 .rddev/tools/packages-vs-dag.py                       # 先看：只有 T0511 该落
python3 .rddev/tools/land-T0511.py --dry-run                   # 干跑
python3 .rddev/tools/land-T0511.py                             # 写 tasks/tasks.json
python3 scripts/spec_version.py --write                        # 重生成指纹，不跑这步 main 红
python3 scripts/validate_task_state.py                         # 9 项检查
git add -A && git commit && git push                           # 一笔提交
./bin/rddev task ready T0511                                   # 之后才能派工
```

**T0511 合并之后**（不是同一时刻）才有的事：`./bin/rddev task ready T0812`。
T0812 现在被冻成 `blocked`，因为它的那半条依赖（"读规则 ADR-024 得先在树上"）
DAG 里表达不出来，而它要改的两个文件正被 T0511 重写。

---

## B. 两处 baseline 的"只许变短"守卫（**新，2026-09-19 发现**）

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
**一句话：这条拆不开，一起落。**

**优先级**：**排在 A 之后。** 空窗是稀缺资源，落任务书能解锁工人（吞吐），
这条是防护（正确性）——若这次空窗只够做一件，做 A。

---

## C. 记着但**不属于空窗**的事（写在这是为了不忘，别在空窗里顺手做）

- **T0602 一合并，立刻清两条判断点**：`./bin/rddev drive --clear-decision T0808` 与
  `--clear-decision T0809`。**驱动会跳过带判断点的任务**——不清，那两个 PR 永远不会被重试合并
  （两条都是"等迁移顺序"，00100 一落就成立）。**这不是空窗工作，是合并那一刻的动作。**
- **T0808 合并后改两处注释**（逐字文本已核过，在 `tasks/decisions.md` 里）：
  `internal/application/researchprofile/doc.go` 的第 2 行与第 8-13 行、
  `apps/web/app/components/research-profile-sections.tsx:39-43`（加 `ReuseList` 例外，
  依据是 `lib/research-profile.ts:119-124` 的 `ProfileReuse.project` 非空）。
  改完**要把修正记在那两条原始发现上**（记账要闭环）。
- **T0811 合并后改一句注释**：`infra/migrations/00104_discussions.sql` 里
  `discussion_promotions` 的 CHECK 注释说 ref「cannot name nothing」——**实测 `'issue:'` 能过**
  （三条 kind 都测过），它钉的是"kind 前缀与 ref 一致"，不钉"非空"。产品路径上 ref 由 Go 用真
  uuid 拼出、够不到空值，**保护完整 → 按判据记账后合并**，合并后把话说准。
- **`make -n <target>` 之类的 CI 步骤自检**：**我倾向于不做**。它要防的失败是**大声**的
  （目标不存在 → make 自己报错），不是静默的，性价比不够。列在这里只为说明**我考虑过并否掉了**，
  免得以后又当成新点子重新捡起来。
