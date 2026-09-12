# Design Tokens / GitHub-style 视觉护栏

实际实现优先直接消费 Primer functional tokens；本文件给 custom component 的语义规则。

## 色彩角色
- canvas.default / canvas.subtle
- fg.default / fg.muted
- border.default / border.muted
- accent.fg / accent.emphasis
- success.fg / success.emphasis
- attention.fg / attention.emphasis
- danger.fg / danger.emphasis

严禁定义 `brandGradient`、`purpleGlow`、`glassBackground` 等 token。

## 圆角
小/中等，信息工作台风格。Buttons/inputs 约 6px 视觉；card 不使用 24px+ 大圆角。

## 阴影
仅 overlay/menu/dialog；普通 card 主要用 border，不用浮夸 shadow。

## Typography
正文 14–16px；辅助 12–14px；标题层级克制。ID/hash/code 使用 mono。长文本正文行长控制。

## Spacing
4px base grid，常用 4/8/12/16/24/32。列表高密度但点击 target 可访问。
