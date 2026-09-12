# Accessibility 与国际化

## 1. Accessibility

目标 WCAG 2.2 AA。键盘操作、focus ring、screen reader label、aria-live、对比度、skip links。Graph 视图提供结构化 outline/list；状态不仅靠颜色。

## 2. Reduced motion

尊重 `prefers-reduced-motion`。不做无意义 motion；数据加载 skeleton/spinner 简洁。

## 3. I18N

V1 UI 至少设计为可国际化：字符串集中管理，不把中文/英文硬编码散落组件。正式上线语言可先 English + Simplified Chinese；领域对象 ID/enum 使用英文稳定 code，显示 label 本地化。

## 4. Scientific units

单位不是普通 i18n。存储使用明确 unit + value；展示可转换但不能改变原始 recorded unit/provenance。避免用 locale formatting 造成小数/千位解析歧义。
