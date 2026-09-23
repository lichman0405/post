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
- Search the network，生成 Evidence-backed Research Answer。**搜索与 Research Answer 一律要求登录**
  （`L3-①` 裁定：不改）；匿名只可见公开页面本身。
- Open Contribution Opportunity。
- External contribution → PR → credit。

### Agent interface
- Semantic HTTP API（OpenAPI-first）读写科研状态。
- MCP / Agent 工具面 **V1 不含**：`specs/mcp/tools.json` 与 `docs/47` 是 V1.x 的设计输入，
  不是 V1 交付物（`L3-③` 裁定，见 §4）。
- Governance 操作（merge/publish/visibility）限制为 Web 或显式 approval path。
- Git Compatibility Mode。

### Events
- Web notification、Email abstraction、RSS/Atom、Webhook API 基础。
- Dependency impact alert。

## 4. V1 明确不做

> §4 的每一条都是**范围裁定**，不是「还没做」：列在这里的东西 V1 的规格与契约都不承诺它。
> 下面带 `L3-⓪`/`L3-②`/`L3-③`/`L3-④` 标记的四条是 owner 于 2026-09-23 新裁定的
> （`tasks/decisions.md` ㊻）；其余各条是 V1 立项时就定下的范围。

- **把平台内容发送到平台之外的第三方服务**（模型、向量、分析）。V1 只保留接口位置、
  测试用确定性假件、不接线；跨出平台的数据分类规则见 `docs/55`（`L3-⓪`）。
- **面向用户的大文件上传入口**：契约不声明 `blobs:request-upload` / `blobs/{blobId}:finalize`，
  Web 不含上传 UI；V1 的 blob 只可能由平台内部流程产生（`L3-②`）。
- **MCP / Agent 工具面**：`specs/mcp/tools.json` 声明的 21 条工具与 `docs/47` 是 V1.x 设计，
  V1 不交付、不接线（`L3-③`）。
- **公开声明的发现面**：attestation 页面只有知道编号 `pid` 才能打开，不提供列表、搜索或
  任何导航入口；「只有知道编号才可达」是产品决定，不是缺口（`L3-④`）。
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
