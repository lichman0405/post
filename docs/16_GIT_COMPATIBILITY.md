# Git Compatibility Mode

## 1. 目的

保留 Git clone/fetch/branch/commit/push 给高级计算用户、现有脚本和工具；但不允许 Git 成为绕过 RSG 的后门。

## 2. Backend

V1 使用内部 Gitea 负责 Git smart HTTP/SSH、bare objects、refs。自研平台负责 identity mapping、branch policy、semantic ingestion、PR/RSG、domain audit。用户不访问 Gitea UI。

## 3. main protection

Gitea 层与平台层双重保护 main；禁止 direct push。平台 merge service 使用受控 service identity 更新 Git ref，并同步 RSG transaction/outbox。

## 4. Push ingestion

Push 到 research branch 后：webhook/outbox → inspect changed paths/manifests → validate known scientific manifests → generate candidate RSG diff → mark `semantic_complete` or `unstructured_changes`。

无法理解的文件可保留，但 branch 不能发起可 merge 的正式 PR，直到 Agent/API 补足 required scientific semantics。

### 4.1 `semantic_complete` / `unstructured_changes` 的作用域（记录既有实现，不改行为）

这一对标记是**从 push ingestion 的证据派生的**（`git_branch_semantic_states`，迁移 00042），
它是**缺陷标记**而不是合格证：`unstructured_changes` 的意思是「**已记录的 push 证据**在这个 head
上看到了平台无法解析的路径」。因此：

- 一个**没有任何 push ingestion 证据**的分支——平台内新建的、syncer 从外部采纳 ref 的、
  或者是 fork 导入的——其值为 `semantic_complete`，依据是「**缺少证据不是存在无法解析内容的证据**」。
- 两道门（`pull_request_semantic_gate` 于 PR 开启、`branch_merge_semantic_gate` 于合并）
  **只对 push ingestion 的内容生效**，它们**不证明**其他到达路径被检查过。
- **把内容带进平台的路径，自己负责那遍检查。** 每条非 push 路径都要在导入时按本节第一段
  走一遍（inspect → validate → mark），把证据造出来让标记派生正确；**不得依赖默认值**，
  **也不得一律标成 `unstructured_changes`**——后者只有一次真正的 push 才能解开，
  会把合法的 fork 流程永久锁死。
- 「API 状态提交不移动这个标记」是一条**已记录的 L1 决定**（迁移 00042 头部）：状态提交与
  它要解析的文件之间没有连结，而在任意提交上清标记正是本节的验收标准要禁止的绕过。

## 5. Git ↔ RSG consistency

每个 accepted State Commit 保存 git commit/ref + rsg state hash。后台 reconciliation job 检查 drift。任何 drift 是 high-severity operational alert。
