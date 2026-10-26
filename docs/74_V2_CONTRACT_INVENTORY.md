# 74 — V2 契约盘点与后端断点（WEB-00 / T1332）

日期：2026-10-26。基准：`c037256`。本文件是 WEB-00 的源码盘点，不是产品验收；
`tasks/v2/contract-inventory.json` 是机器可读的同一份数据。

## 1. 结论摘要

- 27 个 G 行：source_present 3、partial 15、missing 8、unverified 1。
- 已登记 Web 页面 28 个，`cmd/api` 子包 37 个。
- 三个硬断点：G07（Web 科研变更没有 provider 写入路径）、首次 Asset 发布（源码存在、旅程未验证）、附件 blob 下载（Git Files 有 raw，S3 blob 没有下载路由）。
- 历史 V1 绿灯不计入本表；没有执行的网页步骤一律不是 passed。

## 2. G01–G27 盘点

| 编号 | 能力 | 结论 | 承接口 | 主要证据 |
|---|---|---|---|---|
| G01 | 注册、登入、个人资料 | **source_present** | WEB-02 | `apps/web/app/(auth)/login/login-card.tsx`, `apps/web/app/(auth)/login/page.tsx`, `cmd/api/authhttp/**` |
| G02 | Home 工作台 | **partial** | WEB-02 | `apps/web/app/(main)/page.tsx` |
| G03 | 建立 Organization、邀请成员 | **partial** | WEB-03 | `apps/web/app/(main)/organizations/page.tsx`, `apps/web/app/(main)/organizations/[slug]/page.tsx`, `cmd/api/orgshttp/**` |
| G04 | 建立 Project | **missing** | WEB-04 | `apps/web/app/(main)/projects/project-directory.tsx`, `apps/web/lib/projects.ts`, `cmd/api/projectshttp/**` |
| G05 | Branch 与研究状态 | **missing** | WEB-06 | `cmd/api/rsghttp/wiring.go`, `internal/application/branches/service.go`, `apps/web/app/(main)/projects/[id]/research/page.tsx` |
| G06 | Object Detail、Research Map | **partial** | WEB-05 | `apps/web/app/(main)/projects/[id]/research/page.tsx`, `cmd/api/rsghttp/page.go`, `cmd/api/rsghttp/graph.go` |
| G07 | Web 变更与 Git/RSG 一致 | **missing** | WEB-07 | `internal/application/rsg/service.go`, `internal/application/rsg/evidence.go`, `internal/application/rsg/overview.go` |
| G08 | Evidence/Provenance | **partial** | WEB-10 | `cmd/api/evidencehttp/**`, `cmd/api/provenancehttp/**`, `internal/application/evidencegraph/**` |
| G09 | Issues/Discussion | **missing** | WEB-11 | `apps/web/app/(main)/projects/[id]/issues/page.tsx`, `cmd/api/discussionhttp/**`, `internal/application/discussions/**` |
| G10 | PR 建立/送审/评审/合并 | **partial** | WEB-12/13/14 | `apps/web/lib/pulls.ts`, `apps/web/app/(main)/projects/[id]/pulls/[number]/page.tsx`, `cmd/api/pullrequestshttp/**` |
| G11 | Scientific Conflict | **source_present** | WEB-13 | `apps/web/app/(main)/projects/[id]/conflicts/page.tsx`, `cmd/api/conflicthttp/**`, `internal/application/resolutions/**` |
| G12 | Freeze/Abort/Reopen/Policy | **partial** | WEB-14/23 | `cmd/api/freezehttp/**`, `cmd/api/aborthttp/**`, `internal/application/mainfreeze/**` |
| G13 | Release/Milestones | **source_present** | WEB-15 | `apps/web/app/(main)/projects/[id]/releases/page.tsx`, `cmd/api/releasehttp/**`, `cmd/api/milestonehttp/**` |
| G14 | 首次 Publish Asset | **partial** | WEB-16 | `apps/web/app/(main)/projects/[id]/assets/publish/page.tsx`, `apps/web/lib/publish.ts`, `cmd/api/assetshttp/handlers.go` |
| G15 | Knowledge 发布/详情 | **missing** | WEB-17 | `cmd/api/knowledgehttp/**`, `internal/application/knowledgepublish/**`, `apps/web/app/(main)/explore/explore-tabs.tsx` |
| G16 | Search → Project | **partial** | WEB-19 | `apps/web/lib/search.ts`, `apps/web/app/(main)/search/page.tsx`, `cmd/api/searchhttp/draft.go` |
| G17 | Reference/Dependency/Fork/Derive | **partial** | WEB-20 | `cmd/api/forkshttp/**`, `internal/application/forks/**`, `internal/application/relations/**` |
| G18 | 外部提案与贡献 | **partial** | WEB-21 | `cmd/api/forkshttp/**`, `cmd/api/knowledgehttp/**`, `internal/application/contribution/**` |
| G19 | People/Organizations 发现 | **partial** | WEB-18 | `apps/web/app/(main)/people/page.tsx`, `apps/web/app/(main)/users/[id]/page.tsx`, `apps/web/app/(main)/organizations/[slug]/page.tsx` |
| G20 | Watch/Inbox/依赖预警 | **partial** | WEB-22 | `apps/web/app/(main)/notifications/page.tsx`, `apps/web/lib/inbox.ts`, `cmd/api/inboxhttp/**` |
| G21 | Files/附件/导出 | **missing** | WEB-09 | `apps/web/app/(main)/projects/[id]/files/page.tsx`, `cmd/api/fileshttp/files.go`, `internal/assets/preview.go` |
| G22 | 项目工作事项与研究记录 | **missing** | WEB-08 | `apps/web/app/(main)/projects/[id]/research/page.tsx`, `internal/application/sciobjects/**` |
| G23 | 可用性与真实 UI Gate | **partial** | WEB-01/25 | `tests/**`, `tests/e2e-*/**`, `.github/workflows/ci.yml` |
| G24 | 项目模板与补充字段 | **missing** | WEB-26 | `cmd/api/templateshttp/**`, `cmd/api/schemaprofileshttp/**`, `internal/application/templates/**` |
| G25 | 数据预览与基础分析 | **partial** | WEB-27 | `internal/assets/preview.go`, `cmd/api/fileshttp/files.go`, `services/scientific-adapter/**` |
| G26 | 可部署团队试用版 | **partial** | WEB-29/30 | `docs/35_DEPLOYMENT_RUNBOOK.md`, `docs/36_RELEASE_RUNBOOK.md`, `docs/37_BACKUP_DR.md` |
| G27 | Kimi 与持续监督链路 | **unverified** | WEB-28 | `cmd/rddev/**`, `internal/devorchestrator/**`, `docs/61_DEVELOPMENT_ORCHESTRATION.md` |

## 3. 三个后端断点

### BP-G07 — Web 科研变更到 provider ref / Git

- 结论：**missing**
- 承接口：T1308 (WEB-07)
- 证据：internal/application/rsg/service.go / evidence.go 把写入交给 states.Service.Commit；状态行只有可空 GitCommitSHA。
- 证据：internal/application/merge/service.go + cmd/api/mergegit/bridge.go 才调用 provider 合并/前进 ref。
- 证据：cmd/api/rsghttp/wiring.go 的 Service 接口没有 GitProvider 写入端口。
- 处置：WEB-07 必须先设计并实现 provider 写入/失败重试/reconciliation；必要时先写 ADR。不得用 CLI git、mock provider 或只断言 DB 成功收尾。

### BP-FIRST-PUBLISH — 首次 Asset 发布（无既存 Asset）

- 结论：**source_present_but_unverified**
- 承接口：T1317 (WEB-16)
- 证据：internal/application/assetpublish/command.go 的 prepare 返回 newAsset 布尔，存在首次创建分支。
- 证据：cmd/api/assetshttp/handlers.go 有 preview 与 publish 路由。
- 证据：apps/web/app/(main)/projects/[id]/assets/publish/page.tsx 与 lib/publish.ts 提供 Web 入口。
- 处置：源码存在不等于首次发布旅程已通；WEB-16 必须从 accepted state/Release 在空 Asset 项目走完四类型，不能用预建 Asset 或 API 代行。

### BP-BLOB-DOWNLOAD — 研究上下文附件（S3 blob）下载

- 结论：**missing**
- 承接口：T1310 (WEB-09)
- 证据：cmd/api/fileshttp/files.go 的 /files/raw 只流式转发 Git provider 的 blob。
- 证据：internal/assets/preview.go 与 blob_attachments 只提供 access_level/预览元数据。
- 证据：在 cmd/api 下 grep 不到 blob attachment 的下载/预签名路由。
- 处置：WEB-09 需要新增受权限约束的 blob 下载通道；Public metadata 不等于 blob open，Restricted/private 必须拒绝匿名与无权账号。

## 4. 本盘点不证明什么

- 不证明任何 U01–U14 网页旅程通过；所有产品验收仍是 `not_run`。
- 不证明后端路由可用；路由存在只是源码事实，不是跨服务集成证据。
- 不证明附件权限正确；G21 的 blob 下载路由缺失本身就是待实现项。
- 不证明 Kimi/Worker 链可用；WEB-28 仍受 429 阻塞。

## 5. 下一步

1. WEB-07 先按 BP-G07 设计 provider 写入/失败重试/reconciliation，必要时先写 ADR。
2. WEB-09 按 BP-BLOB-DOWNLOAD 新增受权限约束的 blob 下载，并保持 Files 只读。
3. WEB-16 从 accepted state/Release 在空 Asset 项目完成四类型首次发布。
4. WEB-01 用本盘点的路由/动作清单建立 UI 驱动 harness，逐条接入 blocking CI。
