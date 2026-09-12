# 外部技术基线来源（规格生成：2026-09-12）

本项目的产品设计主要来自内部规格讨论；以下仅用于锁定开发工具与框架当前能力/版本基线。

- Claude Code Advanced Setup / Installation：`https://code.claude.com/docs/en/installation`
  - Ubuntu 20.04+ 支持；Linux/WSL 可安装；`claude doctor`；stable channel。
- Claude Code Sandboxing：`https://code.claude.com/docs/en/sandboxing`
  - Linux/WSL2 使用 bubblewrap + socat；Ubuntu 24.04+ 需要 bwrap AppArmor profile。
- Claude Code CLI Reference / Headless：`https://code.claude.com/docs/en/cli-usage`、`https://code.claude.com/docs/en/headless`
  - `claude -p`、structured output、allowed/disallowed tools、permission modes。
- Claude Code Permissions：`https://code.claude.com/docs/en/agent-sdk/permissions`
  - locked-down worker 推荐 explicit allow/deny + `dontAsk`，不要用 bypassPermissions 误以为 allowedTools 会限制全部能力。
- Go release history：`https://go.dev/doc/devel/release`
  - 2026-09-01：Go 1.27.1。
- Node.js releases：`https://nodejs.org/en/blog/release`
  - 2026-09-09：Node 24.21.0 LTS。
- Next.js support/blog：`https://nextjs.org/support-policy`、`https://nextjs.org/blog`
  - Next.js 16.3 Active LTS；2026-08-25 security release 16.3.3。
- Docker Engine Ubuntu install：`https://docs.docker.com/engine/install/ubuntu/`

生产使用前仍应由 CI/依赖更新流程检查安全补丁，不把本文件当成永久最新版本清单。
