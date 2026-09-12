# 科研版本控制、PR 与 Semantic Merge

## 1. Branch

Branch 表示研发路径而非个人工作区。Public Project 新 Branch 默认 Public；Private Project 新 Branch 默认 Private。任何 private→public 必须显式 publication review。

## 2. Commit / State Transition

UI 呈现“科研变化”而非仅文件 diff。每个 state commit 应能列：objects created/updated/aborted/reopened、relations changed、files attached、schema/policy refs、actor/via、message。

## 3. Frozen main

正式研发开始后 main 为 protected/frozen。禁止所有直接 push/semantic write；即便 Owner 也只能经 PR merge。Emergency unfreeze 不在 V1 提供，避免形成绕过路径；必要维护用 admin break-glass runbook，并记录审计。

## 4. PR

PR 包含：source/target branch、base state、RSG diff、file diff、validation results、review requirements、discussion、visibility impact、dependency impact、merge strategy。

## 5. Review

- Scientific Review：科学方法/结论判断。
- Integrity Review：schema、provenance、dependency、rights、hash、visibility、required fields。

Review 状态可以按维度记录，不用单一 approve 抹平所有差异。

## 6. Semantic conflict 分类

- Attribute conflict：同字段不同修改。
- Identity conflict：疑似同对象/不同对象。
- Relation/provenance conflict。
- Schema compatibility conflict。
- Knowledge conflict：Claim assessment/evidence conflict。
- Rights/visibility conflict。
- Dependency conflict。

## 7. 自动合并边界

允许自动：不同对象、同对象不同非冲突字段、可合并关系集合、append-only evidence。

禁止自动：科学结论 winner、Protocol 冲突数值折中、因果判断、private→public、rights conflict。

## 8. 冲突解决动作

Accept A / Accept B / Keep both versions / Explicit coexistence / Create validation branch / Request more evidence / Abort proposed change。Scientific conflict 可在 main 中保持 `contested/unresolved`。

## 9. Merge 与 Publish

Merge 只改变 accepted state；若 source 包含 private state 而 target public，需要独立 Publication Gate，且只公开经批准对象。Private branch history 不自动公开。
