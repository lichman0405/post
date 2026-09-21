# 全站信息架构

## 1. 全局顶部导航

建议 V1：

`Home | Explore | Search | Projects | Assets | People | Organizations | Notifications | Profile`

登录前保留 Explore/Search/Public entity 浏览；需要写操作时引导登录。

## 2. Home

登录用户：My Projects、Watching、Needs Attention、Recent Research Events、Suggested Open Contributions。

未登录：公开项目、Research Assets、近期 Findings、可贡献项目、搜索入口；禁止营销页压过产品内容。

## 3. Project 导航

`Overview | Research | Issues | Pull Requests | Releases | Milestones | Assets | Files | Activity | Settings`

- Overview：Research Summary、main 状态、active branches、key findings、open questions、needs attention。
- Research：默认 Research Map，以 Question/Findings/Paths 组织；可切换 Objects、Evidence、Provenance。
- Issues：工作流事项。
- Pull Requests：Research State Diff + Review。
- Releases：immutable snapshots。
- Milestones：项目研究时间线上的有日期事实（候选入选、论文投稿、专利申报、外部验证、自定义标签），按 `occurred_at` 升序即研究顺序；可以指向一个 release（链接可选），但它**不是**项目生命周期状态——列表不携带完成态，任何 milestone 都不驱动状态迁移。
- Assets：本 Project 发布/依赖/衍生的 assets。
- Files：只读底层 Git/Blob 专业视图。
- Activity：state transitions、audit、contribution events。
- Settings：members、visibility、policies、schemas、integrations。

## 4. Network entity 页面

- Research Asset Page
- Published Research Question/Hypothesis/Claim/Finding Page
- Person Research Profile
- Organization Research Profile

所有页面必须显示 persistent identity、version、origin、rights、lineage、current network state。

## 5. Search Answer

结果页主区域不是结果列表，而是 Evidence-backed Answer：Answer summary、comparison、evidence profile、conflicts/limitations、Research Map、source objects、Start Research Project。底部才是 underlying results。

## 6. Explore

维度：Projects、Assets、Knowledge、People、Organizations、Open Contributions。排序不得以“点赞数”为核心；允许 freshness、reuse、reproduction、match、curated relevance。
