# ADR-028：V1 不扫容器镜像——把「没有对象」写成一条**有守卫的**书面风险接受

Status: Accepted

> 定稿于 T1209 合并之后（合并提交 `034a341`，PR #355）。文中每个数字与措辞都对着**合并后的树**逐条核过
> （MIN_CHECKS=17、三条证据行、守卫行），四条「守卫能红」的演示在探针树上实测过，
> 记录在 `.rddev/runtime/supervisor-g2/T1209-adr-028-demonstrations.md`。

## 背景

- `docs/23_SECURITY_PRIVACY.md:45`（§11）：每个 Release 运行 dependency audit、SAST、secret scan、
  **container scan**、OWASP smoke、permission E2E；**Critical/High 必须修复或有书面 ADR/risk acceptance
  （V1 不接受 Critical）**。`docs/25_CICD_DEVOPS.md:24`（第 10 条）同旨：container build/SBOM。
- **这棵树不构建任何镜像**：全树没有 `Dockerfile`/`Containerfile`。
  `ops/DEV_COMMANDS.md:154` 逐字记着「仓库里没有 Dockerfile（tree claim `no-dockerfile`），没有镜像」；
  `ops/runbook-steps.json` 的 `release-4-image-digest` 那条写着「Nothing builds an image: there is no
  Dockerfile, and CI has no container build/SBOM stage (docs/25 item 10)」。
- 所以 container scan 的现状**不是「漏跑一个工具」，而是「没有可扫的产物」**。两种错法都要挡：
  ① 让它只以**无守卫**的缺席条目存在；② 把「没有对象」打成绿（等于把缺席伪装成一次检查）。
- 机制上的关键：`ops/security/absent-checks.json` 的注释已把话说死——「Whether an absent capability is
  an ACCEPTED V1 RISK is **the Supervisor's written call**（docs/23 §11 allows a written ADR/risk
  acceptance for High; Critical is not accepted in V1）」，而每个条目的 `suggested_disposition` 只是
  **Worker 的建议**。T1206/T1209 造好了机器，**这份 ADR 就是那个书面裁决**。

## Decision

1. **缺席有两层守卫，两层都必须每次重新挣得。**
   （a）**清单层**：`ops/security/absent-checks.json` 里 `container-scan` 是唯一的缺席条目，`kind` 为
   `guarded-absence`，`guard_check_id` 指向总门里那条 `container-scan` 行，`absence_witness` 是那次
   「全树找容器构建文件」的整段走查（`find` 剪掉 `.git`/`.rddev`/`node_modules`/`.next`/`.venv`/`.sbom`
   /`.backup-dr`/`.dev`/`post-wt`，要求**无输出**），外加一条「`no-dockerfile` 这条 tree claim 仍登记在
   `ops/runbook-steps.json` 里、且恰好一次」的见证，以及**第三条指向本文件自己的见证**：
   `grep -c 'no-dockerfile' docs/adr/ADR-028-*.md` 必须非零——这份书面接受若被改名或删掉，
   `absence-manifest` 行当场红（清单里同时以 `risk_accepted_in` 指名本文件）。
   `tests/security/check-absent-manifest.py` 在每次门跑时
   **真的执行**这些命令，并且 `--selftest` 会先自证「守卫被改指一个不存在的 check id」与「缺席条目被删」
   两种腐烂形状都能让它红。
   （b）**门层**：总门新增 `container-scan` 行（`tests/security/container-scan.sh`）。它在树里没有容器构建
   文件时**绿**，并打印它走查了多少文件、它的绿建立在哪条 tree claim 上；**一旦出现任何 `Dockerfile`、
   `Dockerfile.*`、`*.Dockerfile`、`Containerfile`、`Containerfile.*`，或那条 tree claim 消失，它立刻红**
   （退出码 1），并在 stderr 里写明「这一行已经不再适用，必须有真的镜像扫描来取代它」。
2. **守卫能红能绿是实测过的，不是读代码得出的。**
   - 种一行 `FROM scratch` 的 `Dockerfile`：`--only container-scan` 由 `ok` 变 `FAIL container-scan: exit 1`，
     `--only absence-manifest` 同时转红（两层守卫各自发现）；删掉后两条都恢复绿。
   - 从总门注册表里删掉任意一行：注册表完整性失败、退出码 3（下限 17 是硬约束，删行不能悄悄变绿）。
   - 证据行**只在退出码为 0 时**才被认（`master-security-gate.sh:596-606`：`rc -ne 0` 直接记 FAIL 并 `continue`，
     走到证据检查的只有 0 退出的行），所以见证命令退出 1 时，
     那一行不会因为后面仍打印的 `ok   item: container-scan` 而蒙混过关。
     下限也不是提示：注册表行数 `< MIN_CHECKS`（`:120` 的 `MIN_CHECKS=17`）时 `:415-419` 以 `EXIT_USAGE=3` 退出。
3. **本 ADR 就是 `docs/23:45` 要求的那份书面 risk acceptance 在 container scan 这一项上的形式**，
   范围**仅限**「V1 交付不含任何镜像」，并且**自我失效**：镜像一出现，守卫转红即失效信号，届时应以
   「扫描行 + 修复/新的书面接受」取代——按 §11 的同一句话，届时镜像是 Critical 的话**不可接受**。
4. **SAST 与 SBOM 不在本接受范围内。** 它们已由 T1209（`034a341（PR #355）`）从 `absent` 改为总门里的
   **实检行**：`sast-go` / `sast-python` / `sast-node`（gosec v2.29.0、bandit 1.8.6、
   eslint 9.39.5 + eslint-plugin-security 4.0.1）、`sbom-go` / `sbom-node` / `sbom-python`
   （cyclonedx-gomod v1.9.0、`pnpm sbom`、`uv export --format cyclonedx1.5`）与 `license-audit`
   （对 `ops/security/license-allowlist.json` 判每一条许可证）。注册表下限由 9 升到 **17**。
5. 残余风险按现状写，不修辞（见 Consequences 第 3 条）。

## Consequences

1. **引入镜像的改动会在门里立刻撞红**，而不是静默通过——「缺席」从此只在**有守卫**的意义上被允许，
   且这条守卫与 `absent-checks.json` 交叉核对（清单里指名的 check id 必须在注册表里真实存在），
   清单和门不会再各自漂移。
2. 交付物一侧的对称性保持：`docs/36_RELEASE_RUNBOOK.md` 的 `release-4-image-digest` 那条本就把它记为
   `absent`（Nothing builds an image），本 ADR 与该记录一致，不新增一条「跳过」。
3. **残余风险（照写）**：今天真正在跑的只有 `docker-compose.yml` 拉的上游基础设施镜像
   （pgvector 0.8.6-pg16、redis 7.4.11-alpine、minio RELEASE.2025-09-07T16-13-09Z、gitea 1.27.3、
   mailpit v1.31.1），**按 tag 钉住、无人扫描**；这些镜像里的 CVE 只能靠读 release notes 发现，
   不由任何门发现。（T1209 起它们的**许可证**每次跑门都被打印出来并要求一个书面裁决：redis 7.4.11（RSALv2/SSPLv1）与
   minio（AGPL-3.0）印在 `NEEDS ADR — service images under a restrictive licence`，mailpit 印在
   `LICENCE UNVERIFIED — a service image whose licence nobody has recorded`，五个镜像都登记在
   `tests/security/key-components.json`（`judgement` 逐条：pgvector=allow〔PostgreSQL Licence〕、gitea=allow〔MIT〕、
   redis=needs-adr、minio=needs-adr、mailpit=unknown）——**但那一行刻意不替部署方决定**（原文：`this row does not decide them:
   docs/40 principle 1 asks for a written decision by whoever owns the deployment's licensing`），
   决定在 owner 手上，问题已记在 `.rddev/runtime/owner-decisions-needed.md`。而且那是「许可证是什么」，
   **不是「镜像里有什么」**。）staging 模板的 R03 规则
   （拒绝非 `@sha256:<64 hex>` 的钉法）管的是**怎么引用镜像**，不是**镜像里有什么**；依赖审计管的是应用
   自己的包，不是基础镜像的 OS 包。
   ——**代价若显现，位置和价钱都已写在清单里**：镜像本身（每应用一个 Dockerfile + CI 的 build 阶段，
   `docs/25` 第 10 条）是那件真活儿；扫描器（trivy/grype image 模式 + 每镜像 High/Critical 失败 +
   一个 ignore 文件）只是镜像存在之后的一天工作量。守卫行必须被那个扫描器**取代**，而不是顺手删掉。

## Alternatives

- **只留在 `absent-checks.json`、不写 ADR**：否决——那正是「无守卫的缺席」，本 ADR 存在的唯一理由。
- **装一个扫描器去扫不存在的镜像**：否决——没有对象，工具只会 `NOT ASKED`，等于把缺席伪装成一次检查。
- **给 `docs/23:45` 加一条豁免**：否决——规格是产品面；处置一条工程缺席要走规格自己给的那条路
  （书面接受），不是改规格。
- **把这一行直接写成绿、承诺「以后补」**：否决——与「没对象」是同一个事实，诚实度却低一档；
  守卫正是两者的全部区别。
- **让 container scan 也进 `required_jobs`**：否决——那等于让一个没有对象的检查成为必需作业，
  CI 会长期挂一条无信息的绿；**它的位置在总门内部作为一条有见证的缺席**，而不是一条独立作业。

## 可当场复核的事实（每条都带命令）

| 断言 | 怎么当场看 |
|---|---|
| 全树没有容器构建文件 | `bash tests/security/container-scan.sh` → `ok   container-scan: no container build file in the tree (N file(s) walked, tree claim 'no-dockerfile' holds)`，退出码 0 |
| 守卫**能说不**（门层） | 种一个 `Dockerfile` 后重跑上一条 → 退出码 1，stderr 列出找到的文件并写明「这一行要由真的镜像扫描取代」；删掉后恢复绿 |
| 缺席有见证、且见证被执行 | `python3 tests/security/check-absent-manifest.py --selftest` → `ok   item: container-scan — witness …` 与 `ok   selftest: …`，退出码 0 |
| 见证**能说不**（清单层） | 种一个 `Dockerfile` 后跑 `python3 tests/security/check-absent-manifest.py` → `FAIL item container-scan: the absence witness now produces output…`，退出码 1 |
| 清单与门交叉核对 | 总门 `--only absence-manifest` → 证据行 `^ok   item: container-scan — guarded absence: the .container-scan. row of this gate is registered` |
| 下限是硬的 | 从注册表删掉任意一行 → `registry integrity FAILED — 16 check(s) registered, floor is 17`，退出码 3 |
| 规格出处 | `docs/23_SECURITY_PRIVACY.md:45`、`docs/25_CICD_DEVOPS.md:24`、`ops/DEV_COMMANDS.md:154`、`ops/runbook-steps.json` → `release-4-image-digest` |
