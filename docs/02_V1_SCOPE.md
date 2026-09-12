# V1 范围与非范围

## 1. V1 必须证明的产品假设

V1 必须证明三件事：

1. 一个材料研发项目能否用 RSG + Branch/PR/Main/Release 被完整、自然地协作管理。
2. 一个被接受的研发结果能否发布为 Network Object/Research Asset，并被其他项目引用、Fork、依赖或贡献外部 Evidence。
3. 一个贡献者能否通过真实研发行为形成可验证的 Research Profile，而不依赖论文作者身份或组织头衔。

## 2. V1 官方领域

底层模型通用，但官方 Schema、模板、Seed Demo 和 UI 优先覆盖材料科学：MOF、气体分离、材料筛选、合成、表征、DFT、MD、GCMC/RASPA、实验验证。

## 3. V1 必做功能

### Identity / Organization
- 注册、登录、公开 Research Profile。
- Organization 创建、成员关系、基础角色。
- affiliation 带时间范围。

### Projects
- Public / Private Project。
- Project Purpose、Program 归属（Program V1 可轻量）。
- Frozen main、Research Branch、State Commit、PR、Review、Merge。
- Public Project 的公开 Branch/PR；Private Branch 不隐式公开。

### Scientific Objects
- Research Question、Hypothesis、Material、Sample、Experiment、Calculation、Dataset、Protocol、Claim、Finding、External Reference。
- Object version、relations、schema、provenance。
- Abort/Reopen。

### Evidence
- Evidence Assertion。
- Provenance/Evidence 分离。
- supports、contradicts、consistent_with、inconsistent_with、reproduces、fails_to_reproduce、validates、challenges、contextualizes。

### Release / Asset
- Immutable Release。
- Asset Hub：Dataset、Protocol、Material Collection、Benchmark。
- Publish、version、lineage、Reference、Dependency、Fork/Derive。
- Basic Rights metadata。

### Open Network
- Explore Public Projects/Assets/Knowledge/People/Organizations。
- Public web access without login。
- Search the network，生成 Evidence-backed Research Answer。
- Open Contribution Opportunity。
- External contribution → PR → credit。

### Agent interface
- MCP/API 读写科研状态。
- Governance 操作（merge/publish/visibility）限制为 Web 或显式 approval path。
- Git Compatibility Mode。

### Events
- Web notification、Email abstraction、RSS/Atom、Webhook API 基础。
- Dependency impact alert。

## 4. V1 明确不做

- 自动迁移旧 GitHub/GitLab/NAS/ELN/LIMS 项目。
- 自建计算/HPC/实验执行层。
- Federation / self-hosted node 网络互联。
- 跨组织 Project 的细粒度 compartment UI（只预留 policy 模型）。
- 科研悬赏、支付、赏金市场。
- 全互联网论文/数据库爬取和通用 Scholar 搜索。
- 新法律许可证体系。
- 在线文件编辑器/IDE/Jupyter。
- 自动科学真值判定或单一 Research Score。

## 5. V1 扩展预留要求

虽然不实现，数据模型/API 不得堵死：multi-org collaboration、object-level access、embargo、federation、self-hosted、更多 vertical schema、更多 asset type、schema/blueprint hub、private evidence attestation、execution providers。

## 6. V1 成功判据

以 `docs/31_MASTER_ACCEPTANCE.md` 为唯一高层 Gate。功能数量不是成功标准，端到端网络闭环才是。
