# 核心状态机

## Project
planning → active ↔ paused → archived；archived 可 reactivate。无“research completed”终态。

## Branch
active → merged | aborted；merged/aborted history immutable。

## Object lifecycle
active → aborted → reopened → active；可 superseded（关系/状态事件）但不物理删除。

## PR
open → review_required → changes_requested/approved → merge_ready → merged；也可 closed/aborted。Scientific/Integrity review 各自状态。

## Claim/Finding assessment
preliminary、supported、contested、unresolved、superseded、aborted。不要把这些当线性升级等级。

## Asset Version
published → active；后来可 status notice aborted/superseded，但 version content immutable。

## Publication
private candidate → publication_review → published；失败回到 private candidate，不存在自动 published。
