# Demo / UAT 标准

最终演示不是点击页面巡游，而是完整科研故事：两个用户、两个组织、public/private branch、一个冲突、一个 release、四类 asset 中至少两类实际参与、一个 external evidence、一个 search→new project、一个 dependency impact alert。

每步演示前置状态由 Seed builder 自动生成；不可依赖人工修 DB。

UAT 必须包含负向场景：匿名访问 private URL、Agent 尝试 publish、Git direct main push、restricted blob download、search private leakage，全部失败且提示合理。
