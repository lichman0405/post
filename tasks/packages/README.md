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
python3 /tmp/apply-packages.py            # 先干跑，看会替换哪些字段
python3 /tmp/apply-packages.py --write    # 真正写进 tasks/tasks.json
python3 scripts/spec_version.py --write   # 重新生成指纹 —— 不跑这步 main 会红
python3 scripts/validate_task_state.py    # 9 项检查
./bin/rddev task ready <TASK_ID>          # 然后才能派工
```

**注意**：`apply-packages.py` 住在 `/tmp`，**不在仓库里** ——
它是过程工具，不是产品的一部分，所以没提交。重启之后如果它没了，
照上面这四步的语义重写即可（要点是：先证明 JSON 往返字节一致、拒绝覆盖非 `todo` 的任务、
拒绝丢掉 `tasks/tests.json` 里已登记的测试）。

## 字段形状

每份任务书与 `tasks/tasks.json` 里那条任务的字段**同名同义**，
另外多一个 `supervisor_scope_narrowing`：**为什么这么划范围**。
它会被原样带进 `tasks/tasks.json`，也就是会被 Worker 读到 —— 这是有意的，
让干活的人看见每一条范围决定的依据，而不是自己猜。
