# Seed Demo：MOF 气体分离开放研发项目

用于开发、截图、E2E 和最终验收，所有数据为明显标注的 synthetic/demo 数据，不冒充真实科研结论。

## Project
`demo-mof-humidity-separation`

Purpose：验证 MOF-X 在高湿环境下的 C2H4/C2H6 separation performance。

## Research Question
Q1：MOF-X 在 298K / 40–70% RH 下是否仍保持选择性？

## Hypotheses
H1：性能下降主要来自 water competitive adsorption。
H2：性能下降来自 partial framework degradation。

## Objects
至少：3 Materials、5 Samples、8 Experiments、6 Calculations、5 Datasets、3 Protocol versions、6 Claims、3 Findings、3 External References。

## Evidence
包含 supporting、contradicting、consistent_with、reproduction，故意构造一个 contested finding。

## Branches
`humidity-40rh`, `humidity-70rh`, `mechanism-water-binding`, `protocol-activation-180c`。

## PR
至少一个无冲突 merge、一个 Protocol scientific conflict、一个 private branch selective publication。

## Releases
R0.1 baseline、R1.0 humidity-validation。

## Assets
Dataset benchmark、Protocol、Material Collection、Benchmark 四类各至少 1 个。

## External contribution
第二个 demo user/org Fork/Reference H1，产生一个 contradictory experiment evidence，再提交 external evidence/PR。
