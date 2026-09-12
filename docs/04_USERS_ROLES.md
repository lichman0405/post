# 用户、权限角色、科研责任与贡献角色

## 1. 用户群体

V1 不只服务一种职业身份。官方材料 vertical 的典型用户包括项目负责人、实验研究者、计算研究者、数据研究者、企业研发人员、独立研究者、学生和普通外部贡献者。

界面不按“职业”硬编码功能，而按权限与上下文呈现。

## 2. Access Role（能做什么）

V1 推荐固定四档：

- `Owner`：组织/项目最高治理权限，不能绕过 frozen main 与审计。
- `Maintainer`：治理 branch/PR/release/publish/member policy。
- `Contributor`：在授权 Project 中通过 Agent/API 创建研发分支和科研变更、发起 PR。
- `Viewer`：只读授权内容。

Public Project 的非成员用户不是 Contributor role，但可 Fork 并从自己的空间发起外部 PR。

## 3. Scientific Responsibility（什么需要你审核）

不与 Access Role 混合。项目可配置：Experimental Reviewer、Computational Reviewer、Data Reviewer、Project Lead、IP Reviewer 等责任标签。责任用于 Review routing，不自动赋予更高访问权限。

类似 CODEOWNERS 的 Research Owners 规则可按对象类型/Schema/领域匹配 reviewer。

## 4. Contribution Role（实际上做了什么）

Contribution Event 可标注多个角色：

- Research Question Proposal
- Hypothesis Proposal
- Research Design
- Method Development
- Experimental Investigation
- Computational Investigation
- Data Curation
- Analysis
- Validation
- Reproduction
- Scientific Review
- Asset Stewardship
- Project Maintenance

这些不是身份，不赋权，不计固定分值。

## 5. Reputation（别人为什么愿意相信你）

由可验证事件派生，不做单一总分：Accepted Contribution、Release inclusion、Reuse、Independent reproduction、Review quality、Cross-project impact、Industrial/Public contribution 等形成多维 Profile。

## 6. 人与组织关系

Person identity 跨 Organization 长期存在。Affiliation 具有 start/end 时间和 verification status。组织不能删除个人历史贡献；离职只终止 affiliation/role。

## 7. Confidential Verified Contribution

私有项目可以由 Organization 向个人 Profile 出具不可泄密的聚合贡献证明。必须标明 verifier、期间、贡献类别、披露级别；不得泄露 private project 名称、客户、材料或底层资产。
