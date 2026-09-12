# 51 — Frontend Standard

Next.js 16 Active LTS + React + TypeScript + Primer/Octicons。

- GitHub 风格、中性、高密度、明确边框与状态。
- 禁止渐变紫、霓虹、玻璃拟态、营销卡片墙、夸张动画。
- Public pages 服务端输出可索引内容。
- Core business mutation 一律调 Go API；不要用 Server Actions 形成第二后端。
- Files 页面只读。
- Research Map 必须有 list/table fallback，不能只有 Canvas/graph。
- 所有页面设计 loading/empty/error/permission denied 状态。
- 关键路径 Playwright + a11y/keyboard 检查。
