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

## 5. Git ↔ RSG consistency

每个 accepted State Commit 保存 git commit/ref + rsg state hash。后台 reconciliation job 检查 drift。任何 drift 是 high-severity operational alert。
