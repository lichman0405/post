# 术语表与领域模型

## 1. 层级

### Organization / Workspace
公司、实验室、研究机构、团队等网络主体。

### Program
长期研发方向，例如“MOF 气体分离”。V1 可作为 Project 的可选分组，不承载版本控制。

### Project
一个明确研发目标的托管单元，是 Repository 与 RSG 的边界。

## 2. 版本控制术语

- `Research Branch`：一条研发演化路径。
- `State Commit`：一次 RSG state transition 的不可变记录。
- `Pull Request`：一组 proposed RSG diff + file diff + review context。
- `Frozen Main`：当前 accepted research state，禁止直接写。
- `Release`：一个 Project accepted state 的永久冻结快照。

## 3. 科研对象

### Research Question
项目希望回答的科研问题，可包含子问题，不等同 Issue。

### Hypothesis
尚待检验的科学命题，保留其提出历史，即使以后被证伪也不消失。

### Material
材料/结构/候选实体。V1 材料领域可包含化学式、结构标识、CIF blob reference、标签等。

### Sample
具体实验样品/批次，与 Material 区分；可有 batch、preparation lineage。

### Experiment
实验活动记录；包含输入、Protocol、条件、输出、operator、provenance。

### Calculation
计算活动记录；包含 software/method/parameters/input/output/provenance。

### Dataset
一组可识别的数据对象；Project 内 Dataset 不等于 Network Research Asset。

### Protocol
实验/计算/处理方法的版本化方法对象。

### Claim
可以独立判断、独立 Review、独立挂 Evidence 的科学命题。不得把多个不同证明要求的命题混为一个 Claim。

### Finding
人类可读的一组经过整理的 Claim。Finding 是正式对象，不等于 Agent 动态摘要。

### External Reference
论文、专利、外部数据库、标准、datasheet 等外部来源，采用 live identity + pinned snapshot。

## 4. 关系对象

### Relation
RSG 中有方向、类型、版本 pin 的边；例如 uses、produces、derived_from、follows_protocol、contains、depends_on。

### Evidence Assertion
把 Evidence Source 与 Claim/Hypothesis 的具体版本连接起来，描述 supports/contradicts 等关系、scope、directness、inference type、review state。

## 5. Network Objects

### Research Asset
项目正式发布的可复用资产。V1：Dataset、Protocol、Material Collection、Benchmark。

### Published Knowledge Object
显式 Publish to Network 的 Research Question/Hypothesis/Claim/Finding 版本，可被跨 Project 引用或贡献 External Evidence。

### Blueprint
可复用的 Project Template/Schema profile 等研发蓝图。V1 先做官方模板，社区发布能力可最简实现或 Feature Flag。

## 6. 生命周期语义

- `active`：当前有效/在研究。
- `aborted`：明确停止使用/推进，但历史保留。
- `reopened`：在 abort 之后通过新 transition 恢复继续研究。
- `superseded`：被新版本/对象替代；旧对象仍存在。
- `contested`：存在实质性矛盾证据或竞争解释。

避免使用“deleted”作为正式科研对象状态。
