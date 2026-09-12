# 产品方案说明 / Product Vision
**正式产品名：POST — Platform for Open Science & Technology。** “Open R&D Network” 是产品类别与长期愿景，不是另一个独立产品。


## 1. 背景

代码研发已经形成 Git + GitHub/GitLab 的成熟协作范式：版本历史、分支、Issue、PR、Review、Merge、Release、Fork 和贡献身份都具备统一载体。科研与产业研发却仍常依赖网盘、Excel、Word、邮件、聊天、NAS、ELN/LIMS、计算目录和口头知识，造成版本不可控、实验与计算脱节、失败路径消失、贡献无法证明、跨组织协作困难、数据资产难以复用。

本产品希望把 Git 的“可演化、可审查、可回溯”思想扩展到科研研发，但不要求实验人员学习 Git 命令，也不把科研强行塞进文件树。

## 2. 产品定义

Open R&D Network 是一个科研研发托管与开放协作网络。Project 是研发边界；RSG 是科研状态；Git 是版本引擎；Agent 是科研操作代理；Web 是观察、治理、审核、发布与网络发现界面。

长期目标不是做一个科研社区，而是形成真正的“共同研发网络”：不同组织和个人围绕 Project、Knowledge Object、Research Asset、Evidence 和 Contribution 发生可审计的协作。

## 3. 长期网络效应

网络效应来自五类连接：

1. 人 ↔ Project：真实贡献而非关注关系。
2. Project ↔ Project：Fork、Reference、Dependency、External Evidence。
3. Project ↔ Asset：发布、依赖、衍生、版本升级。
4. Knowledge ↔ Evidence：跨项目支持、反驳、复现和竞争假设。
5. Contribution ↔ Identity：长期研究身份、组织关系和可验证影响。

平台不靠数据锁定留人，而靠研发历史、网络关系、贡献声誉与资产复用形成吸引力。

## 4. 核心产品原则

### 4.1 科研语义优先

用户操作的是 Research Question、Experiment、Calculation、Claim、Finding 等科研对象，不是“修改某个 YAML 文件”。文件变化是科研动作的结果。

### 4.2 Accepted state 与 work-in-progress 分离

研发发生在 Branch；`main` 代表项目当前被团队正式接受的状态。main 冻结后只通过 PR Merge 更新。

### 4.3 History append-only

正式科研历史不物理删除。错误、失败、放弃通过 Abort 记录，并永久保存原因和上下文。

### 4.4 Publish 与 Merge 分离

Merge 决定“是否被项目接受”；Publish 决定“是否扩大可见性或进入网络”。任何 private→public 必须显式确认。

### 4.5 Evidence 不等于 Citation

DOI、数据库条目、文档只是来源。证据需精确到具体 assertion、figure、table、dataset、experiment 或 calculation，并说明它与 Claim 的关系。

### 4.6 Agent 是代理，不是责任主体

Agent 可创建 branch、对象和 proposal，可做结构冲突处理与自动校验，但不能替人做科学共识、法律授权、敏感信息公开或最终 main merge 决策。

## 5. 非目标

V1 明确不是：ELN、LIMS、科研网盘、在线 IDE、科研执行平台、论文写作器、通用互联网科研搜索、众包悬赏市场、社交媒体、法务合同系统、Graph DB 产品。

这些系统可与本平台集成，但不改变核心定位。