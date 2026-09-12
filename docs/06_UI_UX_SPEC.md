# Web UI / UX 详细规范

## 1. 设计目标

视觉保持 GitHub/Primer 风格：高信息密度、克制、工程化、可扫读、强状态表达。产品不是“AI SaaS 营销工具”，不使用渐变紫、霓虹、玻璃拟态、漂浮大卡片、过量圆角、过度插画或大面积动画。

## 2. 视觉语言

- 背景以白/深灰（未来 dark mode）和 muted gray 为主。
- 链接与 active 使用蓝色语义 token。
- Success 绿色、Danger 红色、Attention 黄色/琥珀色。
- 紫色仅在必要语义状态使用，不作品牌背景或 CTA 渐变。
- 边框清晰；卡片圆角小；表格与列表优先于“卡片瀑布”。
- 不靠颜色单独传递状态：必须配 icon/text/state label。
- Typography 优先系统 UI font；代码/ID 使用 monospace。

## 3. 布局

- 桌面主工作区目标 1280–1600px。
- Project 页面使用 GitHub 类顶部 repo header + tab nav。
- 详情页面主栏 + 右侧 metadata/sidebar；Research Map 可占全宽。
- 表格在窄屏允许横向滚动；移动端 public content 可阅读，复杂治理操作可提示桌面体验更佳但不可完全不可用。

## 4. Project Overview

首屏必须在无需滚动过多情况下展示：Project purpose、main frozen/visibility、key research questions、key findings、active research branches、PR/issues attention、latest release、network/asset dependency warnings。

绝不把文件树作为首页首屏。

## 5. Research Map

默认粗粒度，以 Research Question → Hypothesis/Research Path → Finding 组织。节点数量控制；默认聚合。点击逐层 drill-down 到 Claim/Evidence/Object。完整 RSG 不默认渲染为蜘蛛网。

支持视图：By Question（默认）、By Finding、By Material/System、By Research Path、Timeline。V1 可先实现前两种。

## 6. PR 页面

PR 首屏展示 Research State Diff：新增/修改/Abort 的科研对象、知识变化、Evidence 变化、Protocol/Schema changes、visibility changes、dependency impact。Raw Git diff 位于二级 tab。

必须显示 Scientific Review 与 Integrity/Provenance Review 两层状态。

## 7. Files 页面

100% 只读。允许：tree、file preview、history、raw diff、download（受权限）、“View scientific context”。禁止：Edit、Upload、Replace、Delete、drag-drop mutation。

## 8. Publish/Visibility UX

任何 private→public 必须独立确认页，列出：将公开的对象、metadata、blob access、依赖、private evidence/attestation、rights/license。禁止一个 generic “Are you sure?” 弹窗替代影响清单。

## 9. Agent 入口

Web 可提供 “Work with Agent / Open in Claude Code” 操作，但不内置聊天窗口作为主产品中心。Agent 是研发操作入口，Web 保持可浏览、可治理。

## 10. 可访问性

键盘可达、focus visible、语义 HTML、label、ARIA live 对动态状态、颜色对比满足 WCAG AA。Graph 视图必须提供等价列表/outline，不得只靠画布表达。
