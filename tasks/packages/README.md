# tasks/packages/ —— 任务书草稿（**不是活的配置**）

这个目录是 **Supervisor 的暂存区**，不是任何程序读取的输入。

**没有任何东西读它。** 全仓库搜不到 `tasks/packages` 的消费者
（`scripts/`、`cmd/`、`internal/`、`Makefile` 里都没有）。
任务规格的**唯一真相源**是 `tasks/tasks.json`。

## 为什么会有这个目录

写一份任务书要读规格树、逐条回树核对引用、再决定范围 —— 这活儿是在**派工之前**做的，
而 `tasks/tasks.json` 是**规格指纹的输入**（`scripts/spec_version.py:5`），
**在还有任务在飞的时候改它，会让 main 变红、并让所有在飞任务的 G2 挂掉**。

所以流程是：**先在 `tasks/packages/<TASK_ID>.json` 里把任务书写好并提交留档，
等到"没有任务在飞"的那个窗口，再一次性写进 `tasks/tasks.json` 。**

## 落地顺序（顺序不能乱）

```sh
python3 .rddev/tools/apply-packages.py --dry-run T0806 T0811 T0901   # 先干跑
python3 .rddev/tools/apply-packages.py T0806 T0811 T0901             # 写进 tasks/tasks.json
python3 scripts/spec_version.py --write   # 重新生成指纹 —— 不跑这步 main 会红
python3 scripts/validate_task_state.py    # 9 项检查
./bin/rddev task ready <TASK_ID>          # 然后才能派工
```

**注意**：`apply-packages.py` 住在 `.rddev/tools/`，**不在仓库里**（`.rddev/` 被 gitignore）——
它是过程工具，不是产品的一部分，所以没提交。它**活得比 /tmp 长**（2026-09-18 从 `/tmp` 挪过来，
因为一次重启把它清掉了，害得落地流程中断）。真丢了的重写要点：
先证明 JSON 往返字节一致、拒绝覆盖非 `todo` 的任务、拒绝丢掉 `tasks/tests.json` 里已登记的测试、
拒绝改动 `id`/`dependencies`/`tests`（除非显式 `--allow-structural`）。

## 字段形状

每份任务书与 `tasks/tasks.json` 里那条任务的字段**同名同义**，
另外多一个 `supervisor_scope_narrowing`：**为什么这么划范围**。
它会被原样带进 `tasks/tasks.json`，也就是会被 Worker 读到 —— 这是有意的，
让干活的人看见每一条范围决定的依据，而不是自己猜。

**但不要照抄整条任务。** 最自然的写法是"打开 `tasks/tasks.json` 里那条、改掉要改的字段"，
而那样写出来的任务书会顺手带上四个**落地工具一律拒收**的字段（`apply-packages.py:131`）：

    baseline_sha, max_budget_usd, max_turns, migration_number

它们住在任务表里，但**不归任务书管**：`migration_number` 是派工时按账本现发的
（`worker_spawn.go:207` → `AllocateMigrationNumber`，账本 `.rddev/runtime/migration-numbers.json`），
任务表里那个值**会被覆盖**，所以它不是真相源；另外三个同理。
落地时表里已有的值会被原样保留，任务书根本不需要提它们。

**照抄的代价**：2026-09-18 一次演练里五份任务书全带着这四个字段，
`apply-packages --dry-run` 在第一份就退出，后面几份**根本没被检查到**——
工具是遇到第一个错就停的。所以落地前**务必先跑一遍 `--dry-run` 看全部**，
别等空窗开了才发现。

## 警告：这个目录里有**过期草稿**，落地前必须逐个比对（2026-09-19 记）

**`apply-packages.py` 分不出"待落地的任务书"和"早就被更新的旧草稿"**——它只会写。
而这个目录两样都有：有些任务书是先用包写好、落地时又直接在 `tasks/tasks.json` 上改过
（派工前的 33 处引用修正、T0806/T0811 的 8 处更正、T1007 补的第 6 条需求……），
**那些包里存的还是改之前的文本**。一起落下去就会把这些更正**静默改回旧版**。

**落地前先跑这个**（只读，判据是逐字段比对，不是日期）：

```sh
python3 .rddev/tools/packages-vs-dag.py
```

`identical` = 那次落地是无操作；`NOT IN DAG` = 真新任务（要先写骨架）；
**`DIFFERS` = 有人得看一眼**——它可能是待落地的，也可能是过期草稿。
**判据是字段差异本身**：`tasks.json` 里那份比包**新**（多了更正后的引用、多了整条需求），
就**不要**apply它。

2026-09-19 全量比对的结果：目录里 **26 份包，5 份 identical、20 份 DIFFERS、1 份不在 DAG**。
我逐份看过三个最要紧的差异方向，**live 都比包新**——T0602 与 T0811（在飞）的 live 版本里带着
"原书这里引 `:7` 是错的"这类更正；**T1007（唯一还没派的差异项）live 比包多一整条需求**
（`docs/18:39` 的 Dependency Watch 告警那条，2026-09-19 补的）。其余 DIFFERS 落在
`deliverables`/`tests` 这类**结构字段**上——而 `apply-packages.py` 本来就拒绝改
`id`/`dependencies`/`tests`（除非显式 `--allow-structural`），所以那部分落下去也动不了。
**当次真正待落地的只有 T0511 一份。**


