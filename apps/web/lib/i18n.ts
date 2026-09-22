/**
 * The message catalog and the translator (T1105, docs/28 §3).
 *
 * docs/28 §3, verbatim: "V1 UI 至少设计为可国际化：字符串集中管理，不把中文/
 * 英文硬编码散落组件。正式上线语言可先 English + Simplified Chinese；领域对象
 * ID/enum 使用英文稳定 code，显示 label 本地化。"
 *
 * Three consequences of that sentence shape this module, and each is a
 * deliberate absence rather than an oversight:
 *
 *   1. STRINGS LIVE HERE, NOT IN COMPONENTS. Every rendered sentence in the
 *      UI is a key in MESSAGES. A component that renders a literal instead
 *      is the thing the spec forbids, and `tests/i18n/scan-hardcoded.mjs`
 *      counts them so the claim is checkable rather than asserted.
 *
 *   2. TWO LOCALES, AND ONLY TWO. English and Simplified Chinese are the
 *      spec's named V1 set. The catalog is closed: adding a third is an
 *      edit to LOCALES plus a column of MESSAGES, not a new mechanism.
 *
 *   3. DOMAIN CODES ARE NOT TRANSLATED. IDs, enum values, `data-*` anchors
 *      and request bodies keep their English stable code; only a *label*
 *      is localized, and a label is only ever a display string. The catalog
 *      therefore never feeds a request body, an href or an attribute name —
 *      `t()` returns text and nothing else.
 *
 * WHY THERE IS NO I18N LIBRARY
 *
 * The task's default was to add none, and nothing here needs one: two
 * locales, flat keys, `{name}` interpolation. A runtime such as
 * next-intl/i18next would add a dependency and a build step (a plugin or a
 * loader) to do what the sixty lines below do, and the framework-shaped
 * answer for routing — `app/[locale]/…` — is explicitly out of scope (URL
 * shape belongs to specs/ui). What a library does buy is pluralization and
 * date/number formatters; neither is used, because docs/28 §4 says the
 * opposite of what those formatters do to a scientific value.
 *
 * WHY THIS FILE HAS NO IMPORTS
 *
 * Deliberate, and the reason is checkable: apps/web/lib/*.ts are all
 * import-free, which is what lets `node --test apps/web/lib/*.test.mjs` run
 * them through Node's type stripping (scripts/web-unit-tests.sh). An import
 * here would move this module out of the only unit-test gate the web app
 * has, and the catalog's parity and missing-key rules deserve to be pinned
 * by a test that runs on every CI run rather than only in a browser.
 */

/* ------------------------------------------------------------------ *
 * Locales and the preference
 * ------------------------------------------------------------------ */

/** The V1 set, verbatim from docs/28 §3: English + Simplified Chinese. */
export const LOCALES = ["en", "zh-CN"] as const;
export type Locale = (typeof LOCALES)[number];

/** The locale every page falls back to. English is the current UI's own
 *  language, so an unset preference changes nothing about today's output —
 *  which is also what makes "switch back to en restores the page" a
 *  property a test can hold this task to. */
export const DEFAULT_LOCALE: Locale = "en";

/** Where the language preference is kept.
 *
 *  A cookie, and NOT a URL prefix. The task fixed this: routing shape
 *  belongs to specs/ui (page-inventory.csv writes `/{org}/{project}`),
 *  and `app/[locale]/…` would rewrite all 26 routes at once — including
 *  the two paths tests/web-smoke/a11y-smoke.mjs asserts on. The cost of
 *  this choice is stated where it belongs, in the RESULT: a deep link
 *  cannot carry a language, so two people sharing a URL see it in
 *  whatever language each of them prefers. That is a real limitation and
 *  it is not hidden here.
 *
 *  `SameSite=Lax` and not `Secure`: the dev topology is http (docs/65), and
 *  a Secure-only cookie would make the switch a no-op on localhost. The
 *  cookie is a display preference — it is not read by the API, is never
 *  sent to a cross-site context, and carries no identity. */
export const LOCALE_COOKIE = "post_locale";

/** One year. A language preference is a preference, not a session. */
export const LOCALE_COOKIE_MAX_AGE = 60 * 60 * 24 * 365;

export function isLocale(value: unknown): value is Locale {
  return typeof value === "string" && (LOCALES as readonly string[]).includes(value);
}

/**
 * Map a cookie value (or an Accept-Language-ish tag) to a supported locale.
 *
 * Anything unrecognized becomes the default rather than throwing: the cookie
 * is attacker-influenceable in the sense that a user can type it, and a
 * layout that threw on `post_locale=xx` would turn a typo into a 500 on
 * every page. Matching is case-insensitive and accepts a region-less
 * `zh`, because that is what a browser's own tag looks like.
 */
export function normalizeLocale(value: string | null | undefined): Locale {
  if (typeof value !== "string") return DEFAULT_LOCALE;
  const wanted = value.trim().toLowerCase();
  if (wanted === "zh" || wanted === "zh-cn" || wanted === "zh-hans") return "zh-CN";
  if (wanted === "en" || wanted.startsWith("en-")) return "en";
  return DEFAULT_LOCALE;
}

/* ------------------------------------------------------------------ *
 * The catalog
 * ------------------------------------------------------------------ */

/**
 * Flat, dotted keys, both locales side by side.
 *
 * Flat rather than nested so that "do the two locales have the same keys?"
 * is one set comparison — which is exactly what the parity test does, and
 * what makes a half-translated catalog a red test instead of a runtime
 * surprise on the one page nobody opened.
 *
 * Keys are named `<surface>.<thing>`, where `<surface>` is the route or the
 * shared shell the string belongs to (`nav`, `common`, `project`, `pr`, …).
 * English is the source of truth: every value in `en` is the string this app
 * rendered before this task. Localization is not copy-editing — rewriting
 * the English would have reddened T1104's freshly-laid a11y baseline and the
 * existing e2e assertions along with it, for no product reason.
 */
export const MESSAGES: Record<Locale, Record<string, string>> = {
  en: {
    /* ---- app shell ---- */
    "shell.skipToContent": "Skip to content",
    "shell.siteTitle": "POST — Platform for Open Science & Technology",
    "shell.siteDescription":
      "Research objects, evidence and releases under version control.",

    /* ---- global navigation ---- */
    "nav.home": "Home",
    "nav.explore": "Explore",
    "nav.search": "Search",
    "nav.projects": "Projects",
    "nav.assets": "Assets",
    "nav.people": "People",
    "nav.organizations": "Organizations",
    "nav.notifications": "Notifications",
    "nav.logoLabel": "POST home",
    "nav.searchLabel": "Search POST",
    "nav.landmarkLabel": "Global",
    "nav.menuLabel": "Global navigation menu",
    "nav.signIn": "Sign in",
    "nav.signedInAs": "Signed in as {name}",
    /* The drawer's version of the same sentence, split because T0107 wraps
     * the name in a <strong>: this value ends in a space on purpose. */
    "nav.signedInAsLabel": "Signed in as ",
    "nav.yourProfile": "Your profile",
    "nav.viewProfile": "View profile",
    "nav.signOut": "Sign out",
    "nav.signingOut": "Signing out…",
    "nav.accountMenuLabel": "Account menu for {handle}",
    "nav.checkingSession": "Checking session…",
    "nav.language": "Language",
    /* The two endonyms below are identical in BOTH locales on purpose: the
       reader who needs this control is looking at a page they cannot read,
       so each language has to be named the way its own readers write it.
       The key set still has to match across locales like every other key. */
    "locale.name.en": "English",
    "locale.name.zh-CN": "简体中文",

    /* ---- shared ---- */
    "common.none": "none",
    "common.platformTitle": "POST — Platform for Open Science & Technology",

    /* ---- home status panel ---- */
    "home.webVersion": "web {version}",
    "home.developmentStatus": "Development status",
    "home.statusNote":
      "The web app has no backend of its own: it only renders status fetched over HTTP from the Go API and the scientific adapter.",
    "home.requestTraceLabel": "Request trace:",
    "home.requestTraceHelp":
      "grep the API, worker and adapter logs with this id to follow this render.",
    "home.signInPrompt": "to create and publish research objects.",

    /* ---- sign-in card ---- */
    /* The tab title is copy too — see app/(auth)/login/page.tsx. The em dash
       and the trailing "— POST" product name are part of the English string,
       so the Chinese one keeps the same shape rather than re-punctuating the
       title into a different convention. */
    "login.metaTitle": "Sign in — POST",
    "login.heading": "Sign in to POST",
    "login.signupHeading": "Create your account",
    "login.subtitle": "Platform for Open Science & Technology",
    "login.email": "Email",
    "login.password": "Password",
    "login.pleaseWait": "Please wait…",
    "login.createAccount": "Create account",
    "login.oidc": "Continue with your institution (OIDC)",
    "login.newToPost": "New to POST?",
    /* docs/42 §Login: the LINK that switches the card to sign-up mode is
     * "Create an account"; the BUTTON that submits the sign-up form is
     * "Create account". Two different strings, one letter apart, and the
     * baseline had both — sharing one key collapsed them. */
    "login.createAnAccount": "Create an account",
    "login.alreadyHaveAccount": "Already have an account?",

    /* ---- project directory (/projects) ---- */
    "projects.metaTitle": "Projects — POST",
    "projects.title": "Projects",
    "projects.subtitle":
      "Your research projects — visibility state, frozen-main status and research questions per project.",
    "projects.loading": "Loading projects",
    "projects.empty": "No projects yet. Projects you join — or create — appear here.",
    "projects.error.signIn": "Sign in to see your projects.",
    /* The four badges are LABELS. The `data-badge` attribute next to each one
       is a stable code and is deliberately absent from this catalog. */
    "projects.badge.public": "Public",
    "projects.badge.private": "Private",
    "projects.badge.frozenMain": "Frozen main",
    "projects.badge.repositoryPending": "repository pending",

    /* ---- project shell (every /projects/{id} route) ---- */
    "project.loading": "Loading project",
    "project.breadcrumb": "Breadcrumb",
    "project.tabsLabel": "Project",
    "project.notFound.title": "Project not found",
    "project.notFound.body": "This project does not exist, or you do not have access to it.",
    "project.unavailable.title": "Project unavailable",
    "project.backToProjects": "Back to Projects",
    "project.tab.overview": "Overview",
    "project.tab.research": "Research",
    "project.tab.issues": "Issues",
    "project.tab.pulls": "Pull requests",
    "project.tab.releases": "Releases",
    "project.tab.milestones": "Milestones",
    "project.tab.assets": "Assets",
    "project.tab.files": "Files",
    "project.tab.activity": "Activity",
    "project.tab.settings": "Settings",

    /* ---- project overview (the demo surface) ---- */
    "project.overview.loading": "Loading project overview",
    "project.overview.kicker": "MOF humidity validation · 298 K",
    "project.overview.headline": "Can MOF-X separate ethylene under realistic humidity?",
    "project.overview.currentDecision": "Current decision",
    "project.overview.decision": "Proceed below 40% RH",
    "project.overview.decisionNote":
      "Upstream drying or reactivation required above the threshold.",
    "project.overview.metric.objects": "Research objects",
    "project.overview.metric.claims": "Versioned claims",
    "project.overview.metric.findings": "Synthesized findings",
    "project.overview.metric.branches": "Active branches",
    "project.overview.metric.evidence": "Evidence links",
    "project.overview.primaryQuestion": "Primary question",
    "project.overview.openMap": "Open research map →",
    "project.overview.noQuestion": "No primary question yet",
    "project.overview.partiallyAnswered": "partially answered",
    "project.overview.competingHypotheses": "2 competing hypotheses",
    "project.overview.findings": "Evidence-backed findings",
    "project.overview.workInProgress": "Work in progress",
    "project.overview.branchCount": "{count} branches",
    "project.overview.workspaces": "Project workspaces",
    "project.overview.liveSurface": "Live demo surface",
    "project.overview.acceptedMain": "Accepted main",
    "project.overview.by": "by",
    "project.space.research": "Research",
    "project.space.researchDesc": "Question → hypothesis → evidence → finding",
    "project.space.researchStatus": "ready",
    "project.space.issues": "Issues",
    "project.space.issuesDesc": "Open scientific and integrity work",
    "project.space.issuesStatus": "4 open",
    "project.space.pulls": "Pull requests",
    "project.space.pullsDesc": "Research-state proposals and checks",
    "project.space.pullsStatus": "in review",
    "project.space.milestones": "Milestones",
    "project.space.milestonesDesc": "Candidate and validation timeline",
    "project.space.milestonesStatus": "4 events",
    "project.space.assets": "Assets",
    "project.space.assetsDesc": "Publishable datasets and protocols",
    "project.space.assetsStatus": "8 candidates",
    "project.space.files": "Files",
    "project.space.filesDesc": "Methods, data dictionary and analysis notes",
    "project.space.filesStatus": "main",
    "project.space.releases": "Releases",
    "project.space.releasesDesc": "Immutable, reviewed snapshots",
    "project.space.releasesStatus": "gate pending",

    /* ---- search ---- */
    "search.metaTitle": "Search — POST",
    "search.metaDescription":
      "An evidence-backed answer to a research question, with the sources it cites, what limits it and what contradicts it.",
    "search.title": "Search",
    "search.introPrefix": "Ask a question about the network — for example",
    "search.introExample":
      "which MOF materials show CO2 uptake above 3 mmol/g at 298 K?",
    "search.introSuffix":
      "The answer comes back with the sources it cites, what limits it, and what contradicts it.",
    "search.answerFor": "Answer for",

    /* ---- page titles of routes whose body is not yet in the catalog ---- */
    /* These two routes carry a generic server-built <title> and nothing else
       from this catalog: /organizations/{slug} deliberately derives no title
       from a server-side read (see that page's comment), and /assets' browse
       list is uncovered. The title is the one piece of copy those routes do
       own, so it is localized even though the body is not — a Chinese reader
       gets a Chinese tab rather than an English one on a Chinese page. Keep
       this group honest: it names routes that are HALF-covered, it is not a
       place to park work that was skipped. */
    "page.organizations.metaTitle": "Organization profile — POST",
    "page.assets.metaTitle": "Assets — POST",

    /* ---- research profile (shared by the person and organization cards) ---- */
    /* ---- conflicts (the advisory block only: see the coverage table) ---- */
    "conflicts.detectorExplanation": "Detector explanation",
    /* docs/09 §7 draws the auto-merge boundary: the machine may resolve
     * STRUCTURAL conflicts and must not resolve scientific ones ("禁止自动：
     * 科学结论 winner、Protocol 冲突数值折中、因果判断…"). This badge is that
     * boundary rendered on a conflict card — it labels the block below as
     * carrying no merge authority, which is what the limiting word "only"
     * says and what the sentence under it spells out ("The machine never
     * applies this on its own — recording a decision is the human's call").
     * It is deliberately not rephrased: "advisory" alone would drop the
     * limit, and "suggestion only" would collide with the reviewer-facing
     * sense of "suggestion" the platform does not have. The Chinese column
     * keeps 仅建议 first and the English tail the baseline had. */
    "conflicts.advisoryOnly": "advisory only",
    "conflicts.advisoryNote":
      "The machine never applies this on its own — recording a decision is the human's call.",

    "profile.loading": "Loading profile…",
    "profile.backToStatus": "Back to status",
    "profile.error.load": "The profile could not be loaded.",
    "profile.joined": "Joined {date}",
    "profile.noBio": "No bio yet.",
    "profile.saved": "Profile updated.",
    "profile.edit": "Edit profile",
    "profile.handle": "Handle",
    "profile.handleHint":
      "Lowercase letters, digits and dashes; changing it does not change this page's address.",
    "profile.displayName": "Display name",
    "profile.bio": "Bio",
    "profile.saving": "Saving…",
    "profile.save": "Save",
    "profile.cancel": "Cancel",

    "rp.title": "Research profile",
    "rp.loading": "Loading research profile…",
    "rp.error.load": "The research profile could not be loaded.",
    "rp.affiliation.datesNotRecorded": "dates not recorded",
    "rp.affiliation.until": "until {end}",
    "rp.affiliation.present": "{start} – present",
    "rp.via.web": "web",
    "rp.via.api": "API",
    "rp.via.mcp": "MCP",
    "rp.via.agent": "agent",
    "rp.via.git": "Git",
    "rp.via.system": "system",
    "rp.relation.reproduces": "Reproduced",
    "rp.relation.failsToReproduce": "Failed to reproduce",
    "rp.code.userNotFound": "This person has no public research profile.",
    "rp.code.orgNotFound": "This organization has no public profile.",
    "rp.code.unavailable": "Research profiles are temporarily unavailable. Please try again later.",
    "rp.code.unexpected":
      "The profile could not be rendered: it carried a field a profile is not allowed to show.",
    "rp.code.generic": "Something went wrong. Please try again.",
    "rp.affiliations": "Affiliations",
    "rp.empty.affiliations": "No affiliation recorded.",
    "rp.contributions": "Contributions",
    "rp.empty.contributions": "No public contribution recorded yet.",
    "rp.assets": "Assets",
    "rp.empty.assets": "No published assets yet.",
    "rp.reuse": "Reuse",
    "rp.empty.reuse": "No recorded reuse of this work yet.",
    "rp.reproductions": "Reproductions",
    "rp.empty.reproductions": "No reproduction recorded.",
    "rp.verified": "verified",
    "rp.via": "via",
    "rp.accepted": "accepted",
    "rp.released": "released",
    "rp.usedBy": "used by",
    "rp.projects": "Projects",
    "rp.empty.projects": "No public project to name yet.",
    "rp.footnote":
      "Every dimension above is a list of recorded facts. POST does not compute a score, rank or rating for a person, and this page shows none.",
    "rp.org.loading": "Loading organization profile…",
    "rp.org.error.load": "The organization profile could not be loaded.",
    "rp.org.empty.projects": "No public project yet.",
    "rp.org.empty.contributions": "No public activity recorded yet.",
    "rp.org.empty.assets":
      "No published assets from this organization's public projects yet.",
    "rp.org.footnote":
      "Activity, projects and assets are recorded facts. POST does not compute a score, rank or rating for an organization.",

    /* ---- research tab (docs/42 §Research) ---- */
    "research.error.title": "Research state unavailable",
    "research.error.fallback": "Research state is unavailable",
    "research.loading": "Loading research state",
    "research.empty.title": "No research state yet",
    "research.empty.body": "Create a research question on the main branch to begin.",
    "research.count.questions": "Questions",
    "research.count.hypotheses": "Hypotheses",
    "research.count.claims": "Claims",
    "research.count.findings": "Findings",
    "research.count.other": "Other objects",
    "research.eyebrow": "Accepted research state",
    "research.title": "Evidence-led project map",
    "research.intro":
      "Questions connect competing hypotheses to version-pinned claims, findings and active research branches.",
    "research.mainHead": "Main head",
    "research.countsLabel": "Research object counts",
    "research.questions.eyebrow": "Research questions",
    "research.questions.title": "What the project is trying to resolve",
    "research.refQuestion": "Q ·",
    "research.hypotheses": "Competing hypotheses",
    "research.hypothesisMark": "H",
    "research.findingsLinked": "Synthesized findings",
    "research.findings.eyebrow": "Key findings",
    "research.findings.title": "What the current evidence supports",
    "research.claimBasis": "Claim basis",
    "research.branches.eyebrow": "Active research branches",
    "research.branches.title": "Work in progress, separated from accepted main",
    "research.branchHead": "head",

    /* ---- releases tab (docs/42 §Release) ---- */
    "releases.title": "Releases",
    "releases.intro":
      "Immutable snapshots of main's accepted state. A release fixes the research state, the policy in force and the review record — it never changes afterwards, and it is not affected by later work on the project.",
    "releases.loading": "Loading releases",
    "releases.candidate.kicker": "Release candidate",
    "releases.candidate.title": "v0.1.0-rc1 — Humidity validation evidence package",
    "releases.candidate.body":
      "The candidate is assembled, but POST is correctly preventing an immutable release until the scientific and rights checks pass.",
    "releases.candidate.blockers": "{count} blockers",
    "releases.check.state.title": "Research state assembled",
    "releases.check.state.body": "49 graph objects and four milestones",
    "releases.check.files.title": "Reproducibility files committed",
    "releases.check.files.body": "Protocol, candidate table, analysis and decision record",
    "releases.check.review.title": "Scientific review pending",
    "releases.check.review.body": "Pull request #1 requires domain and integrity approval",
    "releases.check.validation.title": "External validation pending",
    "releases.check.validation.body": "100-cycle result at 40% RH has not been attached",
    "releases.check.rights.title": "Rights snapshot missing",
    "releases.check.rights.body": "Dataset reuse declarations must be frozen before release",
    "releases.manifest": "Manifest",
    "releases.create.title": "Create a release",
    "releases.create.body":
      "The release fixes main's current accepted state. The server re-runs the release gate (reviews, rights snapshot, main state) and refuses if anything blocks.",
    "releases.create.version": "Version",
    "releases.create.versionPlaceholder": "e.g. v1.0.0",
    "releases.create.titleLabel": "Title",
    "releases.create.optional": "(optional)",
    "releases.create.titlePlaceholder": "a short label; defaults to the version",
    "releases.create.submitting": "Creating…",
    "releases.create.submit": "Create release",
    "releases.create.note": "Releases are immutable — there is no edit or delete.",
    "releases.readonly": "Only owners and maintainers can create releases.",
    "releases.error.load": "Could not load releases.",
    "releases.error.create": "Could not create the release. Please try again.",
    "releases.created": "Release {version} created.",

    /* ---- release detail (docs/42 §Release) ---- */
    "release.error.title": "Release unavailable",
    "release.error.load": "Could not load this release.",
    "release.back": "Back to Releases",
    "release.loading": "Loading release",
    "release.breadcrumb": "Release breadcrumb",
    "release.downloadManifest": "Download manifest",
    "release.immutableNote":
      "Immutable snapshot — the state, policy and review record fixed here never change, and later project work does not affect this release. There is no edit or delete.",
    "release.fact.version": "Version",
    "release.fact.state": "State",
    "release.fact.projectPolicy": "Project policy",
    "release.fact.orgPolicy": "Organization policy",
    "release.fact.manifestHash": "Manifest hash",
    "release.fact.created": "Created",
    "release.fact.createdBy": "Created by",

    /* ---- asset page (docs/42 §Asset Page) ---- */
    "asset.loading": "Loading asset",
    "asset.error.title": "Asset",
    "asset.back": "Back to all assets",
    "asset.breadcrumb": "Breadcrumb",
    "asset.allAssets": "Assets",
    "asset.versionChip": "version {version}",
    "asset.publishedAt": "Published {date}",
    /* The two connectives of the "Published … by … in …" line. They are
     * separate values because the name, the handle and the project are
     * elements (each is linked or styled), so the sentence is assembled
     * around them; the leading space is part of the value. */
    "asset.publishedBy": " by ",
    "asset.publishedIn": " in ",
    "asset.publish.newPublicVersion": "Publish a new public version from this one",
    "asset.publish.thisVersion": "Publish this version publicly",
    "asset.publish.notBuildable":
      "This page could not rebuild the version's document, so it does not offer a publication.",
    "asset.publish.tooLong":
      "This version's document is {length} characters once encoded into the link to the confirmation page, past the {budget}-character budget such a link can carry, so the publication is not offered from this page.",
    "asset.origin.title": "Origin",
    "asset.origin.empty": "No resolvable origin recorded for this version.",
    "asset.creators.title": "Creators",
    "asset.creators.empty": "No credited party recorded for this version.",
    "asset.metadata.title": "Metadata",
    "asset.metadata.empty": "This version declares no metadata.",
    "asset.dependencies.title": "Dependencies",
    "asset.dependencies.empty": "This version pins no published dependency.",
    "asset.dependencies.unresolved": "not in this repository",
    "asset.lineage.title": "Lineage",
    "asset.lineage.empty": "No fork or derive edge recorded for this version.",
    "asset.usedBy.title": "Used by",
    "asset.usedBy.empty": "No public usage recorded for this version.",
    "asset.versions.title": "Versions",
    "asset.events.title": "Network events",
    "asset.events.empty": "No public event recorded for this asset.",
    "asset.eventVersion": "version {version}",
    "asset.type.dataset": "Dataset",
    "asset.type.protocol": "Protocol",
    "asset.type.materialCollection": "Material collection",
    "asset.type.benchmark": "Benchmark",
    "asset.code.notFound": "No such asset — or it is not shared with you.",
    "asset.code.unavailable": "Asset data is temporarily unavailable. Try again.",
    "asset.code.listValidationFailed": "That filter is not one this repository offers.",
    "asset.code.pageValidationFailed": "That version address is not one this repository can serve.",
    "asset.code.generic": "Something went wrong loading assets.",
    "assets.browse.empty.all": "No published assets yet.",
    "assets.browse.empty.dataset": "No published dataset assets yet.",
    "assets.browse.empty.protocol": "No published protocol assets yet.",
    "assets.browse.empty.materialCollection": "No published material collection assets yet.",
    "assets.browse.empty.benchmark": "No published benchmark assets yet.",

    /* ---- pull requests (docs/42 §PR) ---- */
    "pull.list.title": "Pull requests",
    "pull.list.intro":
      "Proposed research-state diffs against {project}'s main branch, oldest first. Open a pull request to see its machine integrity report.",
    "pull.list.loading": "Loading pull requests",
    "pull.list.empty": "No pull requests yet. Research PRs appear here once proposed.",
    "pull.list.error.load": "Could not load pull requests.",
    "pull.list.col.number": "Number",
    "pull.list.col.title": "Title",
    "pull.list.col.state": "State",
    "pull.list.col.opened": "Opened",
    "pull.back": "Back to pull requests",
    "pull.loading": "Loading pull request",
    "pull.error.load": "Could not load this pull request.",
    "pull.error.review": "Could not record this review.",
    "pull.openedBy": "opened {date} by {actor}",
    "pull.baseState": "Base (main) state",
    "pull.proposedState": "Proposed (branch) state",
    "pull.tabsLabel": "Pull request views",
    "pull.risk.blocking.one": "{count} blocking risk on this proposal",
    "pull.risk.blocking.many": "{count} blocking risks on this proposal",
    "pull.risk.weigh.one": "{count} risk to weigh before merging",
    "pull.risk.weigh.many": "{count} risks to weigh before merging",
    "pull.risk.open": "Open {tab}",
    "pull.dimension.schema": "Schema",
    "pull.dimension.provenance": "Provenance",
    "pull.dimension.dependency": "Dependency",
    "pull.dimension.rights": "Rights",
    "pull.dimension.visibility": "Visibility",
    "pull.dimension.blob": "Blob references",
    "pull.code.notFound": "This pull request does not exist, or you do not have access to it.",
    "pull.code.validationFailed":
      "That request could not be processed. Check the pull request number and try again.",
    "pull.code.projectNotFound": "This project does not exist, or you do not have access to it.",
    "pull.code.stateNotFound":
      "One of the compared research states no longer exists, so the diff cannot be shown.",
    "pull.code.reviewAlreadySubmitted":
      "You already recorded this review kind for this head — the author has to update the proposal, then review it again.",
    "pull.code.prTerminal":
      "This pull request is already closed; a closed proposal accepts no further reviews.",
    "pull.code.forbidden": "You are not permitted to submit a review in this project.",
    "pull.code.unauthenticated": "Sign in to submit a review.",
    "pull.code.csrfFailed": "The request was rejected as cross-site. Reload the page and try again.",
    "pull.code.unavailable": "Pull request data is temporarily unavailable. Please try again later.",
    "pull.code.generic": "Something went wrong while loading pull request data. Please try again.",
    "pull.tab.summary": "Summary",
    "pull.tab.scientific": "Scientific changes",
    "pull.tab.knowledge": "Knowledge changes",
    "pull.tab.evidence": "Evidence",
    "pull.tab.checks": "Checks",
    "pull.tab.raw": "Raw Files",
    "pull.tabHint.summary":
      "What this proposal changes, in one screen: counts, both review dimensions, and the risks a reviewer must not miss.",
    "pull.tabHint.scientific":
      "Every scientific object the proposal creates, updates, aborts or reopens, with its three-way fields.",
    "pull.tabHint.knowledge":
      "Changes to the knowledge objects (claims, hypotheses, research questions, findings) and the knowledge relations between them.",
    "pull.tabHint.evidence":
      "Evidence assertions and the evidence relations (docs/10 §4) this proposal adds or changes.",
    "pull.tabHint.checks": "The machine integrity report: one row per check, with its own reason.",
    "pull.tabHint.raw":
      "The recorded Git refs of both sides. This is the raw file view — the semantic diff above is the page's subject.",
    "pull.changeKind.created": "created",
    "pull.changeKind.updated": "updated",
    "pull.changeKind.aborted": "aborted",
    "pull.changeKind.reopened": "reopened",
    "pull.visibility.inheritedDefault": "inherited default",
    "pull.visibility.bornPinned": "{title} (born pinned to {policy})",
    "pull.visibility.changed": "{title} (policy {before} → {after})",
    "pull.risk.checkFailed": "{dimension} check failed: {check}{subject}",
    "pull.risk.integrityBlocked": "The machine integrity verdict is blocked",
    "pull.risk.targetMoved.title": "The target branch moved {count} of the compared entries too",
    "pull.risk.targetMoved.detail":
      "{entries} — both sides changed these since the branch point, so the two moves have to be read together before this proposal merges.",
    "pull.risk.aborted.title.one": "{count} object is aborted by this proposal",
    "pull.risk.aborted.title.many": "{count} objects are aborted by this proposal",
    "pull.risk.aborted.detail":
      "{entries} — nothing disappears, so an abort is a scientific act the reviewer must see.",
    "pull.risk.schema.title.one": "{count} object change their governing schema",
    "pull.risk.schema.title.many": "{count} objects change their governing schema",
    "pull.risk.visibility.title.one": "Visibility changes on {count} object",
    "pull.risk.visibility.title.many": "Visibility changes on {count} objects",
    "pull.risk.visibility.detail":
      "{entries} — rights are version-pinned, so the reviewer has to see who can read what after this merge.",
    "pull.risk.refused.title.one": "{count} review request changes on the current head",
    "pull.risk.refused.title.many": "{count} reviews request changes on the current head",
    "pull.risk.refused.entry": "{kind} review by {reviewer}",
    "pull.count.objectsCreated": "Objects created",
    "pull.count.objectsUpdated": "Objects updated",
    "pull.count.objectsAborted": "Objects aborted",
    "pull.count.objectsReopened": "Objects reopened",
    "pull.count.relationsCreated": "Relations created",
    "pull.count.relationsUpdated": "Relations updated",
    "pull.state.basePinned": "Base (pinned)",
    "pull.state.proposed": "Proposed",
    "pull.state.targetHead": "Target (branch head)",
    "pull.reviewState": "Review state of the current head",
    "pull.review.none": "no {kind} review recorded for this head",
    "pull.integrityVerdict": "Machine integrity verdict:",
    "pull.earlier.title": "Decisions on older heads",
    "pull.earlier.judged": "judged",
    "pull.earlier.notHead": " (not the head proposed now)",
    "pull.noGitRef": "no git ref",
    "pull.openConflicts": "Open the conflict resolution view for these states",
    "pull.review.title": "Record a review",
    "pull.review.desc":
      "One decision per dimension on the current head ({stateId}). The API records the reviewer from the session and refuses a second decision of the same dimension on the same head.",
    "pull.review.dimension": "Dimension",
    "pull.review.decision": "Decision",
    "pull.review.choose": "Choose a decision…",
    "pull.review.reasoning": "Reasoning",
    "pull.review.submit": "Record review",
    "pull.review.submitting": "Recording…",
    "pull.review.recorded": "Recorded {decision}",
    "pull.scientific.objects": "Objects",
    "pull.scientific.relations": "Relations",
    "pull.scientific.emptyObjects": "No scientific object changed in this proposal.",
    "pull.scientific.emptyRelations": "No relation changed in this proposal.",
    "pull.knowledge.empty":
      "No knowledge changes in this proposal: no claim, hypothesis, research question or finding moved, and no knowledge relation changed.",
    "pull.evidence.empty":
      "No evidence changes in this proposal: no evidence assertion and no evidence relation moved.",
    "pull.change.moved": "target branch moved this too",
    "pull.change.newObject": "new object",
    "pull.change.newRelation": "new relation",
    "pull.change.changed": "changed: {fields}",
    "pull.verdict": "Verdict: {verdict}",
    "pull.computedAt": "Computed at {at}.",
    "pull.raw.empty":
      "No Git ref is recorded for either side of this comparison, so there is no raw file diff to show. The semantic diff above is the full answer for this proposal.",
    "pull.raw.sourceFiles": "Proposed files (base → proposed head)",
    "pull.raw.targetFiles": "Target branch files (base → target head)",
    "pull.raw.baseRef": "base git ref",
    "pull.raw.headRef": "head git ref",
    "pull.raw.noneEmptyTree":
      "none — the git convention for a born-in-push diff is the empty tree",
    "pull.raw.note":
      "Each link opens that commit's raw patch through the project's Files raw-diff channel. The semantic diff on the other tabs is the subject of this page; the raw files are secondary by design (docs/06 §6).",
    "pull.raw.patch": "raw patch of",

    /* ---- search answer (docs/42 §Search Answer) ---- */
    "search.loading": "Searching and reading the sources…",
    "search.retry": "Try again",
    "search.error.request": "request {requestId}",
    "search.provenance.label": "Search",
    "search.provenance.suffix":
      ". Sources and underlying results are one list in this answer; the cited ones are the subset the summary leans on.",
    "search.answer.title": "Answer",
    "search.view.badge": "View",
    "search.view.tooltip":
      "Written by a model from the sources below; it adds no claim of its own.",
    "search.cites": "Cites",
    "search.fallback.title": "Structured result",
    "search.code.invalidRequest": "That search could not be run as written. Check the question and try again.",
    "search.code.unauthenticated": "Sign in to run a search.",
    "search.code.unavailable": "Search is temporarily unavailable. Try again.",
    "search.code.recordFailed": "The search could not be recorded, so it was not answered. Try again.",
    "search.code.malformedAnswer": "The answer could not be read, so nothing is shown for this search.",
    "search.code.unreachable": "The search service could not be reached. Try again.",
    "search.fallback.headline.noProvider": "No written answer: this deployment has no answer model configured.",
    "search.fallback.headline.noSources": "No written answer: the search returned no source to cite.",
    "search.fallback.headline.providerError": "No written answer: the answer model could not be reached.",
    "search.fallback.headline.providerTimeout": "No written answer: the answer model did not answer in time.",
    "search.fallback.headline.invalidAnswer":
      "No written answer: the model's document did not satisfy the answer schema.",
    "search.fallback.headline.ungroundedCitation":
      "No written answer: the model cited an entity the search did not return.",
    "search.fallback.headline.other": "No written answer for this search.",
    "search.fallback.noReason": "no written answer",
    "search.fallback.note":
      "No answer was written, and the sources below are what the search found. They are the whole result: the platform does not write an answer it cannot ground in them.",
    "search.citedSources.title": "Sources the answer cites",
    "search.limitations": "Limitations",
    "search.conflicts": "Conflicts",
    "search.evidenceMap.title": "Evidence map",
    "search.evidenceMap.note":
      "Derived from this answer's own statements: each limitation or conflict beside the sources its refs name.",
    "search.results.title": "Underlying results",
    "search.cited": "Cited",
    "search.sourceVersion": "version {version}",
    "search.sourceProject": "project {projectId}",
    "search.factors.summary": "Ranking factors ({count})",
  },
  "zh-CN": {
    /* ---- app shell ---- */
    "shell.skipToContent": "跳到主要内容",
    "shell.siteTitle": "POST —— 开放科学与技术平台",
    "shell.siteDescription": "版本控制下的科研对象、证据与发布。",

    /* ---- global navigation ---- */
    "nav.home": "首页",
    "nav.explore": "探索",
    "nav.search": "搜索",
    "nav.projects": "项目",
    "nav.assets": "资产",
    "nav.people": "成员",
    "nav.organizations": "组织",
    "nav.notifications": "通知",
    "nav.logoLabel": "POST 首页",
    "nav.searchLabel": "搜索 POST",
    "nav.landmarkLabel": "全局导航",
    "nav.menuLabel": "全局导航菜单",
    "nav.signIn": "登录",
    "nav.signedInAs": "已登录：{name}",
    "nav.signedInAsLabel": "已登录：",
    "nav.yourProfile": "你的个人主页",
    "nav.viewProfile": "查看个人主页",
    "nav.signOut": "退出登录",
    "nav.signingOut": "正在退出…",
    "nav.accountMenuLabel": "{handle} 的账户菜单",
    "nav.checkingSession": "正在检查会话…",
    "nav.language": "语言",
    "locale.name.en": "English",
    "locale.name.zh-CN": "简体中文",

    /* ---- shared ---- */
    "common.none": "无",
    "common.platformTitle": "POST —— 开放科学与技术平台",

    /* ---- home status panel ---- */
    /* `web` is the name of the running component, not copy: it stays. */
    "home.webVersion": "web {version}",
    "home.developmentStatus": "开发状态",
    "home.statusNote":
      "Web 应用自身没有后端：它只渲染通过 HTTP 从 Go API 与科学适配器取回的状态。",
    "home.requestTraceLabel": "请求追踪：",
    "home.requestTraceHelp": "用这个 id 检索 API、worker 与适配器日志即可追踪本次渲染。",
    "home.signInPrompt": "以创建并发布科研对象。",

    /* ---- sign-in card ---- */
    "login.metaTitle": "登录 — POST",
    "login.heading": "登录 POST",
    "login.signupHeading": "创建你的账户",
    "login.subtitle": "开放科学与技术平台",
    "login.email": "邮箱",
    "login.password": "密码",
    "login.pleaseWait": "请稍候…",
    "login.createAccount": "创建账户",
    "login.oidc": "使用所属机构登录（OIDC）",
    "login.newToPost": "还没有 POST 账户？",
    "login.createAnAccount": "创建一个账户",
    "login.alreadyHaveAccount": "已有账户？",

    /* ---- project directory (/projects) ---- */
    "projects.metaTitle": "项目 — POST",
    "projects.title": "项目",
    "projects.subtitle":
      "你的科研项目 —— 可见性状态、frozen main 状态，以及每个项目的研究问题。",
    "projects.loading": "正在加载项目",
    "projects.empty": "还没有项目。你加入或创建的项目会出现在这里。",
    "projects.error.signIn": "登录后查看你的项目。",
    /* `frozen main` 是领域术语（CLAUDE.md §9 不变量 3 的 frozen main），
       不译：它指的是那条受保护的集成分支，而不是"被冻结的主分支"。 */
    "projects.badge.public": "公开",
    "projects.badge.private": "私有",
    "projects.badge.frozenMain": "frozen main",
    "projects.badge.repositoryPending": "仓库待创建",

    /* ---- project shell (every /projects/{id} route) ---- */
    "project.loading": "正在加载项目",
    "project.breadcrumb": "面包屑导航",
    "project.tabsLabel": "项目",
    "project.notFound.title": "项目不存在",
    "project.notFound.body": "该项目不存在，或者你没有访问权限。",
    "project.unavailable.title": "项目暂不可用",
    "project.backToProjects": "返回项目列表",
    "project.tab.overview": "概览",
    "project.tab.research": "研究",
    "project.tab.issues": "议题",
    "project.tab.pulls": "拉取请求",
    "project.tab.releases": "发布",
    "project.tab.milestones": "里程碑",
    "project.tab.assets": "资产",
    "project.tab.files": "文件",
    "project.tab.activity": "动态",
    "project.tab.settings": "设置",

    /* ---- project overview (the demo surface) ---- */
    "project.overview.loading": "正在加载项目概览",
    /* 298 K / 40% RH / mmol/g 是量值与单位，两种语言里逐字相同（docs/28 §4）。 */
    "project.overview.kicker": "MOF 湿度验证 · 298 K",
    "project.overview.headline": "MOF-X 能在真实湿度下分离乙烯吗？",
    "project.overview.currentDecision": "当前决策",
    "project.overview.decision": "在 40% RH 以下推进",
    "project.overview.decisionNote": "超过该阈值需要上游干燥或再生。",
    "project.overview.metric.objects": "科研对象",
    "project.overview.metric.claims": "带版本的论断",
    "project.overview.metric.findings": "综合发现",
    "project.overview.metric.branches": "活跃分支",
    "project.overview.metric.evidence": "证据链接",
    "project.overview.primaryQuestion": "主要问题",
    "project.overview.openMap": "打开研究图谱 →",
    "project.overview.noQuestion": "尚无主要问题",
    "project.overview.partiallyAnswered": "部分回答",
    "project.overview.competingHypotheses": "2 个竞争假设",
    "project.overview.findings": "有证据支撑的发现",
    "project.overview.workInProgress": "进行中的工作",
    "project.overview.branchCount": "{count} 个分支",
    "project.overview.workspaces": "项目工作区",
    "project.overview.liveSurface": "实时演示界面",
    "project.overview.acceptedMain": "已接受的 main",
    "project.overview.by": "作者：",
    "project.space.research": "研究",
    "project.space.researchDesc": "问题 → 假设 → 证据 → 发现",
    "project.space.researchStatus": "就绪",
    "project.space.issues": "议题",
    "project.space.issuesDesc": "进行中的科研与诚信工作",
    "project.space.issuesStatus": "4 项待办",
    "project.space.pulls": "拉取请求",
    "project.space.pullsDesc": "研究状态的提案与检查",
    "project.space.pullsStatus": "评审中",
    "project.space.milestones": "里程碑",
    "project.space.milestonesDesc": "候选与验证时间线",
    "project.space.milestonesStatus": "4 个事件",
    "project.space.assets": "资产",
    "project.space.assetsDesc": "可发布的数据集与实验方案",
    "project.space.assetsStatus": "8 个候选",
    "project.space.files": "文件",
    "project.space.filesDesc": "方法、数据字典与分析笔记",
    "project.space.filesStatus": "main",
    "project.space.releases": "发布",
    "project.space.releasesDesc": "不可变、经评审的快照",
    "project.space.releasesStatus": "待过门禁",

    /* ---- search ---- */
    "search.metaTitle": "搜索 — POST",
    "search.metaDescription":
      "对一个研究问题的、有证据支撑的回答，连同它引用的来源、它的局限与相互矛盾之处。",
    "search.title": "搜索",
    "search.introPrefix": "就整张网络提一个问题 —— 例如",
    /* 这一句是「可以照抄进搜索框的示例查询」，句子翻译，而其中的
       MOF / CO2 / 3 mmol/g / 298 K 全部逐字保留。 */
    "search.introExample": "哪些 MOF 材料在 298 K 下的 CO2 吸附量超过 3 mmol/g？",
    "search.introSuffix": "回答会连同它引用的来源、它的局限与相互矛盾之处一起返回。",
    "search.answerFor": "回答",

    /* ---- 仅本地化了标题、正文尚未入表的页面（说明见英文栏同名分组） ---- */
    /* 破折号的用法与英文栏保持一致（「—— POST」），不改成中文的另一种标题习惯，
       因为这两条是浏览器标签页标题，与 en 栏的 `X — POST` 是同一个形状。 */
    "page.organizations.metaTitle": "组织档案 — POST",
    "page.assets.metaTitle": "资产 — POST",

    /* ---- research profile (shared by the person and organization cards) ---- */
    /* ---- conflicts（只覆盖咨询块，见覆盖表） ---- */
    "conflicts.detectorExplanation": "检测器说明",
    /* 原文就是「仅建议 · advisory only」，两种语言并列写在同一个标签里；
       中文栏保留双语形式，因为这条标签本身就是给人看的提示语。 */
    "conflicts.advisoryOnly": "仅建议 · advisory only",
    "conflicts.advisoryNote":
      "机器不会自行套用这个建议 —— 做出决定并记录是人来完成的。",

    "profile.loading": "正在加载个人资料…",
    "profile.backToStatus": "返回状态页",
    "profile.error.load": "个人资料加载失败。",
    "profile.joined": "加入于 {date}",
    "profile.noBio": "还没有简介。",
    "profile.saved": "个人资料已更新。",
    "profile.edit": "编辑个人资料",
    "profile.handle": "用户名",
    "profile.handleHint": "小写字母、数字与短横线；修改它不会改变本页地址。",
    "profile.displayName": "显示名称",
    "profile.bio": "简介",
    "profile.saving": "正在保存…",
    "profile.save": "保存",
    "profile.cancel": "取消",

    "rp.title": "研究档案",
    "rp.loading": "正在加载研究档案…",
    "rp.error.load": "研究档案加载失败。",
    "rp.affiliation.datesNotRecorded": "日期未记录",
    "rp.affiliation.until": "截至 {end}",
    "rp.affiliation.present": "{start} – 至今",
    "rp.via.web": "网页",
    "rp.via.api": "API",
    "rp.via.mcp": "MCP",
    "rp.via.agent": "智能体",
    "rp.via.git": "Git",
    "rp.via.system": "系统",
    "rp.relation.reproduces": "已复现",
    "rp.relation.failsToReproduce": "复现失败",
    "rp.code.userNotFound": "此人没有公开的研究档案。",
    "rp.code.orgNotFound": "该组织没有公开档案。",
    "rp.code.unavailable": "研究档案暂时不可用。请稍后重试。",
    "rp.code.unexpected": "无法渲染该档案：它携带了档案不允许展示的字段。",
    "rp.code.generic": "出错了。请重试。",
    "rp.affiliations": "任职经历",
    "rp.empty.affiliations": "暂无任职记录。",
    "rp.contributions": "贡献",
    "rp.empty.contributions": "暂无公开贡献记录。",
    "rp.assets": "资产",
    "rp.empty.assets": "暂无已发布资产。",
    "rp.reuse": "复用",
    "rp.empty.reuse": "暂无该工作的复用记录。",
    "rp.reproductions": "复现",
    "rp.empty.reproductions": "暂无复现记录。",
    "rp.verified": "已核实",
    "rp.via": "经由",
    "rp.accepted": "已接受",
    "rp.released": "已发布",
    "rp.usedBy": "被用于",
    "rp.projects": "项目",
    "rp.empty.projects": "暂无可具名的公开项目。",
    "rp.footnote":
      "以上每一栏都是一组被记录下来的事实。POST 不会为个人计算评分、排名或评级，本页也不展示任何一种。",
    "rp.org.loading": "正在加载组织档案…",
    "rp.org.error.load": "组织档案加载失败。",
    "rp.org.empty.projects": "暂无公开项目。",
    "rp.org.empty.contributions": "暂无公开活动记录。",
    "rp.org.empty.assets": "该组织的公开项目中暂无已发布资产。",
    "rp.org.footnote":
      "活动、项目与资产都是被记录下来的事实。POST 不会为组织计算评分、排名或评级。",

    /* ---- research tab (docs/42 §Research) ---- */
    "research.error.title": "研究状态不可用",
    "research.error.fallback": "研究状态不可用",
    "research.loading": "正在加载研究状态",
    "research.empty.title": "尚无研究状态",
    "research.empty.body": "在 main 分支上创建一个研究问题即可开始。",
    "research.count.questions": "研究问题",
    "research.count.hypotheses": "假设",
    "research.count.claims": "主张",
    "research.count.findings": "结论",
    "research.count.other": "其他对象",
    "research.eyebrow": "已接受的研究状态",
    "research.title": "以证据驱动的项目图谱",
    "research.intro":
      "研究问题把相互竞争的假设与固定到版本的 claim、结论和正在进行的研究分支连接起来。",
    "research.mainHead": "main 头指针",
    "research.countsLabel": "研究对象计数",
    "research.questions.eyebrow": "研究问题",
    "research.questions.title": "项目试图解决的问题",
    "research.refQuestion": "Q ·",
    "research.hypotheses": "相互竞争的假设",
    "research.hypothesisMark": "H",
    "research.findingsLinked": "综合结论",
    "research.findings.eyebrow": "关键结论",
    "research.findings.title": "当前证据支持什么",
    "research.claimBasis": "主张依据",
    "research.branches.eyebrow": "进行中的研究分支",
    "research.branches.title": "与已接受的 main 分离的进行中工作",
    "research.branchHead": "head",

    /* ---- releases tab (docs/42 §Release) ---- */
    "releases.title": "发布",
    "releases.intro":
      "main 已接受状态的不可变快照。一次发布固定研究状态、生效中的策略与审阅记录 —— 它此后永不改变，也不受项目后续工作的影响。",
    "releases.loading": "正在加载发布列表",
    "releases.candidate.kicker": "候选发布",
    "releases.candidate.title": "v0.1.0-rc1 —— 湿度验证证据包",
    "releases.candidate.body":
      "候选已组装完成，但 POST 正在正确地阻止不可变发布，直到科学检查与权利检查通过。",
    "releases.candidate.blockers": "{count} 项阻断",
    "releases.check.state.title": "研究状态已组装",
    "releases.check.state.body": "49 个图谱对象与四个里程碑",
    "releases.check.files.title": "可复现文件已提交",
    "releases.check.files.body": "实验方案、候选表、分析与决策记录",
    "releases.check.review.title": "科学审阅待完成",
    "releases.check.review.body": "拉取请求 #1 需要领域与完整性批准",
    "releases.check.validation.title": "外部验证待完成",
    "releases.check.validation.body": "40% RH 下的 100 次循环结果尚未附上",
    "releases.check.rights.title": "权利快照缺失",
    "releases.check.rights.body": "数据集复用声明必须在发布前冻结",
    "releases.manifest": "清单",
    "releases.create.title": "创建发布",
    "releases.create.body":
      "该发布会固定 main 当前已接受的状态。服务端会重新运行发布门禁（审阅、权利快照、main 状态），只要有阻断就会被拒绝。",
    "releases.create.version": "版本",
    "releases.create.versionPlaceholder": "例如 v1.0.0",
    "releases.create.titleLabel": "标题",
    "releases.create.optional": "（可选）",
    "releases.create.titlePlaceholder": "一个简短标签；默认与版本号相同",
    "releases.create.submitting": "正在创建…",
    "releases.create.submit": "创建发布",
    "releases.create.note": "发布是不可变的 —— 没有编辑，也没有删除。",
    "releases.readonly": "只有 owner 与 maintainer 可以创建发布。",
    "releases.error.load": "无法加载发布列表。",
    "releases.error.create": "无法创建发布，请重试。",
    "releases.created": "发布 {version} 已创建。",

    /* ---- release detail (docs/42 §Release) ---- */
    "release.error.title": "发布不可用",
    "release.error.load": "无法加载该发布。",
    "release.back": "返回发布列表",
    "release.loading": "正在加载发布",
    "release.breadcrumb": "发布导航路径",
    "release.downloadManifest": "下载清单",
    "release.immutableNote":
      "不可变快照 —— 此处固定的状态、策略与审阅记录永不改变，项目后续工作也不会影响该发布。没有编辑，也没有删除。",
    "release.fact.version": "版本",
    "release.fact.state": "状态",
    "release.fact.projectPolicy": "项目策略",
    "release.fact.orgPolicy": "组织策略",
    "release.fact.manifestHash": "清单哈希",
    "release.fact.created": "创建时间",
    "release.fact.createdBy": "创建者",

    /* ---- asset page (docs/42 §Asset Page) ---- */
    "asset.loading": "正在加载资产",
    "asset.error.title": "资产",
    "asset.back": "返回全部资产",
    "asset.breadcrumb": "面包屑导航",
    "asset.allAssets": "资产",
    "asset.versionChip": "版本 {version}",
    "asset.publishedAt": "发布于 {date}",
    "asset.publishedBy": " 由 ",
    "asset.publishedIn": " 于 ",
    "asset.publish.newPublicVersion": "从此版本发布一个新的公开版本",
    "asset.publish.thisVersion": "将本版本公开发布",
    "asset.publish.notBuildable": "本页无法重建该版本的文档，因此不提供发布。",
    "asset.publish.tooLong":
      "该版本的文档编码进确认页链接后为 {length} 个字符，超过了此类链接可承载的 {budget} 字符预算，因此本页不提供发布。",
    "asset.origin.title": "来源",
    "asset.origin.empty": "该版本没有可解析的来源记录。",
    "asset.creators.title": "贡献者",
    "asset.creators.empty": "该版本没有记录的贡献者。",
    "asset.metadata.title": "元数据",
    "asset.metadata.empty": "该版本未声明任何元数据。",
    "asset.dependencies.title": "依赖",
    "asset.dependencies.empty": "该版本没有固定任何已发布的依赖。",
    "asset.dependencies.unresolved": "不在本仓库中",
    "asset.lineage.title": "血缘",
    "asset.lineage.empty": "该版本没有派生或分叉关系记录。",
    "asset.usedBy.title": "被使用",
    "asset.usedBy.empty": "该版本没有公开使用记录。",
    "asset.versions.title": "版本",
    "asset.events.title": "网络事件",
    "asset.events.empty": "该资产没有公开事件记录。",
    "asset.eventVersion": "版本 {version}",
    "asset.type.dataset": "数据集",
    "asset.type.protocol": "协议",
    "asset.type.materialCollection": "材料集合",
    "asset.type.benchmark": "基准",
    "asset.code.notFound": "没有该资产 —— 或者它没有向你共享。",
    "asset.code.unavailable": "资产数据暂时不可用。请重试。",
    "asset.code.listValidationFailed": "该筛选条件不是本仓库提供的。",
    "asset.code.pageValidationFailed": "该版本地址不是本仓库能提供的。",
    "asset.code.generic": "加载资产时出错。",
    "assets.browse.empty.all": "尚无已发布的资产。",
    "assets.browse.empty.dataset": "尚无已发布的数据集类资产。",
    "assets.browse.empty.protocol": "尚无已发布的协议类资产。",
    "assets.browse.empty.materialCollection": "尚无已发布的材料集合类资产。",
    "assets.browse.empty.benchmark": "尚无已发布的基准类资产。",

    /* ---- pull requests (docs/42 §PR) ---- */
    "pull.list.title": "拉取请求",
    "pull.list.intro":
      "针对 {project} 的 main 分支提出的研究状态变更，最早的在前。打开一个拉取请求即可查看它的机器完整性报告。",
    "pull.list.loading": "正在加载拉取请求",
    "pull.list.empty": "尚无拉取请求。研究 PR 一旦提出就会出现在这里。",
    "pull.list.error.load": "无法加载拉取请求。",
    "pull.list.col.number": "编号",
    "pull.list.col.title": "标题",
    "pull.list.col.state": "状态",
    "pull.list.col.opened": "创建时间",
    "pull.back": "返回拉取请求列表",
    "pull.loading": "正在加载拉取请求",
    "pull.error.load": "无法加载该拉取请求。",
    "pull.error.review": "无法记录该审阅。",
    "pull.openedBy": "{date} 由 {actor} 提出",
    "pull.baseState": "基线（main）状态",
    "pull.proposedState": "提案（分支）状态",
    "pull.tabsLabel": "拉取请求视图",
    "pull.risk.blocking.one": "本提案有 {count} 项阻断风险",
    "pull.risk.blocking.many": "本提案有 {count} 项阻断风险",
    "pull.risk.weigh.one": "合并前需要权衡 {count} 项风险",
    "pull.risk.weigh.many": "合并前需要权衡 {count} 项风险",
    "pull.risk.open": "打开{tab}",
    "pull.dimension.schema": "模式",
    "pull.dimension.provenance": "溯源",
    "pull.dimension.dependency": "依赖",
    "pull.dimension.rights": "权利",
    "pull.dimension.visibility": "可见性",
    "pull.dimension.blob": "Blob 引用",
    "pull.code.notFound": "该拉取请求不存在，或者你无权访问它。",
    "pull.code.validationFailed": "该请求无法处理。请检查拉取请求编号后重试。",
    "pull.code.projectNotFound": "该项目不存在，或者你无权访问它。",
    "pull.code.stateNotFound": "所比较的两个研究状态之一已不存在，因此无法显示差异。",
    "pull.code.reviewAlreadySubmitted": "你已经为当前 head 记录过这种类型的审阅 —— 作者必须更新提案后才能再次审阅。",
    "pull.code.prTerminal": "该拉取请求已经关闭；已关闭的提案不再接受审阅。",
    "pull.code.forbidden": "你无权在本项目中提交审阅。",
    "pull.code.unauthenticated": "请先登录再提交审阅。",
    "pull.code.csrfFailed": "该请求因跨站被拒绝。请刷新页面后重试。",
    "pull.code.unavailable": "拉取请求数据暂时不可用。请稍后重试。",
    "pull.code.generic": "加载拉取请求数据时出错。请重试。",
    "pull.tab.summary": "摘要",
    "pull.tab.scientific": "科学变更",
    "pull.tab.knowledge": "知识变更",
    "pull.tab.evidence": "证据",
    "pull.tab.checks": "检查",
    "pull.tab.raw": "原始文件",
    "pull.tabHint.summary": "一屏说清本提案改了什么：计数、两个审阅维度，以及审阅者不可错过的风险。",
    "pull.tabHint.scientific": "本提案新建、更新、中止或重开的每个科学对象，及其三方对比字段。",
    "pull.tabHint.knowledge": "知识对象（主张、假设、研究问题、发现）及其之间知识关系的变更。",
    "pull.tabHint.evidence": "本提案新增或修改的证据断言与证据关系（docs/10 §4）。",
    "pull.tabHint.checks": "机器完整性报告：每项检查一行，并给出各自的理由。",
    "pull.tabHint.raw": "记录下来的两侧 Git 引用。这里是原始文件视图 —— 页面的主题是上面的语义差异。",
    "pull.changeKind.created": "新建",
    "pull.changeKind.updated": "更新",
    "pull.changeKind.aborted": "中止",
    "pull.changeKind.reopened": "重开",
    "pull.visibility.inheritedDefault": "继承默认策略",
    "pull.visibility.bornPinned": "{title}（创建即固定为 {policy}）",
    "pull.visibility.changed": "{title}（策略 {before} → {after}）",
    "pull.risk.checkFailed": "{dimension}检查未通过：{check}{subject}",
    "pull.risk.integrityBlocked": "机器完整性裁定为「阻断」",
    "pull.risk.targetMoved.title": "目标分支也改动了所比较条目中的 {count} 项",
    "pull.risk.targetMoved.detail": "{entries} —— 自分支点以来双方都改动了这些内容，因此合并本提案前必须把两边的改动放在一起阅读。",
    "pull.risk.aborted.title.one": "本提案中止了 {count} 个对象",
    "pull.risk.aborted.title.many": "本提案中止了 {count} 个对象",
    "pull.risk.aborted.detail": "{entries} —— 任何东西都不会消失，因此中止是审阅者必须看到的科学行为。",
    "pull.risk.schema.title.one": "{count} 个对象改变了其治理模式",
    "pull.risk.schema.title.many": "{count} 个对象改变了其治理模式",
    "pull.risk.visibility.title.one": "{count} 个对象的可见性发生变化",
    "pull.risk.visibility.title.many": "{count} 个对象的可见性发生变化",
    "pull.risk.visibility.detail": "{entries} —— 权利是按版本固定的，因此审阅者必须看到合并后谁能读到什么。",
    "pull.risk.refused.title.one": "当前 head 上有 {count} 条审阅请求修改",
    "pull.risk.refused.title.many": "当前 head 上有 {count} 条审阅请求修改",
    "pull.risk.refused.entry": "由 {reviewer} 提交的 {kind} 审阅",
    "pull.count.objectsCreated": "新建对象",
    "pull.count.objectsUpdated": "更新对象",
    "pull.count.objectsAborted": "中止对象",
    "pull.count.objectsReopened": "重新打开对象",
    "pull.count.relationsCreated": "新建关系",
    "pull.count.relationsUpdated": "更新关系",
    "pull.state.basePinned": "基线（固定）",
    "pull.state.proposed": "提案",
    "pull.state.targetHead": "目标（分支头）",
    "pull.reviewState": "当前 head 的审阅状态",
    "pull.review.none": "当前 head 尚无 {kind} 审阅记录",
    "pull.integrityVerdict": "机器完整性判定：",
    "pull.earlier.title": "旧 head 上的决定",
    "pull.earlier.judged": "判定于",
    "pull.earlier.notHead": "（不是当前提案的 head）",
    "pull.noGitRef": "无 git ref",
    "pull.openConflicts": "打开这些状态的冲突解决视图",
    "pull.review.title": "记录一次审阅",
    "pull.review.desc":
      "当前 head（{stateId}）上每个维度只能有一个决定。API 会从会话中记录审阅人，并拒绝在同一 head 上对同一维度做第二次决定。",
    "pull.review.dimension": "维度",
    "pull.review.decision": "决定",
    "pull.review.choose": "选择一个决定…",
    "pull.review.reasoning": "理由",
    "pull.review.submit": "记录审阅",
    "pull.review.submitting": "正在记录…",
    "pull.review.recorded": "已记录 {decision}",
    "pull.scientific.objects": "对象",
    "pull.scientific.relations": "关系",
    "pull.scientific.emptyObjects": "本提案没有科学对象变更。",
    "pull.scientific.emptyRelations": "本提案没有关系变更。",
    "pull.knowledge.empty":
      "本提案没有知识变更：没有 claim、假设、研究问题或结论发生移动，也没有知识关系变更。",
    "pull.evidence.empty": "本提案没有证据变更：没有证据断言或证据关系发生移动。",
    "pull.change.moved": "目标分支也移动了它",
    "pull.change.newObject": "新对象",
    "pull.change.newRelation": "新关系",
    "pull.change.changed": "变更：{fields}",
    "pull.verdict": "判定：{verdict}",
    "pull.computedAt": "计算于 {at}。",
    "pull.raw.empty":
      "这次比较的两侧都没有记录 Git ref，因此没有原始文件差异可显示。上面的语义差异就是本提案的完整答案。",
    "pull.raw.sourceFiles": "提案文件（基线 → 提案 head）",
    "pull.raw.targetFiles": "目标分支文件（基线 → 目标 head）",
    "pull.raw.baseRef": "基线 git ref",
    "pull.raw.headRef": "head git ref",
    "pull.raw.noneEmptyTree": "无 —— 推送时新建的差异按 git 约定是空树",
    "pull.raw.note":
      "每个链接都通过项目的 Files 原始差异通道打开该提交的原始补丁。本页的主题是其他标签页上的语义差异；原始文件按设计是次要的（docs/06 §6）。",
    "pull.raw.patch": "原始补丁",

    /* ---- search answer (docs/42 §Search Answer) ---- */
    "search.loading": "正在检索并阅读来源…",
    "search.retry": "重试",
    "search.error.request": "请求 {requestId}",
    "search.provenance.label": "搜索",
    "search.provenance.suffix":
      "。本次回答中「来源」与「底层结果」是同一份列表；被引用的是摘要所依赖的那个子集。",
    "search.answer.title": "回答",
    "search.view.badge": "视图",
    "search.view.tooltip": "由模型根据下面的来源撰写；它本身不增加任何主张。",
    "search.cites": "引用",
    "search.fallback.title": "结构化结果",
    "search.code.invalidRequest": "该检索无法按原样运行。请检查问题后重试。",
    "search.code.unauthenticated": "请先登录再运行检索。",
    "search.code.unavailable": "检索服务暂时不可用。请重试。",
    "search.code.recordFailed": "该检索无法被记录，因此没有作答。请重试。",
    "search.code.malformedAnswer": "该回答无法读取，因此本次检索不显示任何内容。",
    "search.code.unreachable": "无法连接到检索服务。请重试。",
    "search.fallback.headline.noProvider": "没有书面回答：本部署未配置作答模型。",
    "search.fallback.headline.noSources": "没有书面回答：本次检索没有返回可引用的来源。",
    "search.fallback.headline.providerError": "没有书面回答：无法连接到作答模型。",
    "search.fallback.headline.providerTimeout": "没有书面回答：作答模型未及时作答。",
    "search.fallback.headline.invalidAnswer": "没有书面回答：模型产出的文档不符合回答模式。",
    "search.fallback.headline.ungroundedCitation": "没有书面回答：模型引用了本次检索没有返回的实体。",
    "search.fallback.headline.other": "本次检索没有书面回答。",
    "search.fallback.noReason": "没有书面回答",
    "search.fallback.note":
      "没有撰写回答，下面的来源就是本次检索找到的内容。它们就是完整的结果：平台不会撰写无法以它们为依据的回答。",
    "search.citedSources.title": "回答引用的来源",
    "search.limitations": "局限",
    "search.conflicts": "冲突",
    "search.evidenceMap.title": "证据图",
    "search.evidenceMap.note":
      "由本次回答自身的陈述推导：每一条局限或冲突，紧挨着它的 refs 所指的来源。",
    "search.results.title": "底层结果",
    "search.cited": "已引用",
    "search.sourceVersion": "版本 {version}",
    "search.sourceProject": "项目 {projectId}",
    "search.factors.summary": "排序因子（{count}）",
  },
};

/* ------------------------------------------------------------------ *
 * The translator
 * ------------------------------------------------------------------ */

/** The translator's shape, named once so the client hook, the server helper
 *  and the components that receive one as a prop all agree on it. */
export type Translate = (key: string, vars?: Record<string, string | number>) => string;

export const MISSING_KEY_PREFIX = "⟦missing:";

/**
 * Look a key up, substituting `{name}` placeholders.
 *
 * MISSING-KEY POLICY, chosen and pinned by a test: a key that is absent from
 * the catalog renders as the key itself, wrapped in ⟦missing: …⟧.
 *
 *   - Falling back silently to the English string is the policy that looks
 *     harmless and is not: the page is complete, nothing is red, and the one
 *     string that was never translated is invisible to everybody including
 *     the person who was supposed to translate it. That is how a catalog
 *     rots.
 *   - Throwing is worse: a miss on one string would blank an entire page.
 *   - Rendering the key is visible in the UI, greppable in a screenshot, and
 *     assertable in a test, and it degrades one string rather than a page.
 *
 * The ⟦missing: …⟧ wrapper is there so that a rendered key cannot be mistaken
 * for a deliberate string: no catalog value contains ⟦, and the browser suite
 * fails if a core page renders one.
 */
export function translate(
  locale: Locale,
  key: string,
  vars?: Record<string, string | number>,
): string {
  const table = MESSAGES[locale] ?? MESSAGES[DEFAULT_LOCALE];
  const value = table[key];
  if (value === undefined) {
    return `${MISSING_KEY_PREFIX}${key}⟧`;
  }
  if (vars === undefined) return value;
  return value.replace(/\{(\w+)\}/g, (whole, name: string) => {
    const replacement = vars[name];
    return replacement === undefined ? whole : String(replacement);
  });
}

/** True when `text` is the translator's missing-key output. Exported so the
 *  tests and the browser suite share one definition of "this is a miss"
 *  rather than each matching on the marker string. */
export function isMissingKeyText(text: string): boolean {
  return text.includes(MISSING_KEY_PREFIX);
}

/**
 * A label a lib module could not map: translated, or the raw value it was
 * not taught.
 *
 * lib/*.ts cannot reach the catalog, so their label functions return a key
 * for a value they know and `null` for one they do not. That raw value has
 * to reach the reader UNCHANGED — an asset type or a channel this build
 * predates is shown as it arrived, which is what the page did before the
 * catalog existed. Writing `t(key ?? raw)` looks equivalent and is not: it
 * sends the raw value through the catalog, so the reader sees
 * `⟦missing:benchmark_v2⟧` where the page used to say `benchmark_v2`. Callers
 * use this function instead of the `??` shorthand so the fallback stays the
 * baseline's.
 */
export function translateOr(t: Translate, key: string | null, raw: string): string {
  return key === null ? raw : t(key);
}

/* ------------------------------------------------------------------ *
 * Dates
 * ------------------------------------------------------------------ */

/**
 * Render an API timestamp as a fixed `YYYY-MM-DD` calendar date.
 *
 * THIS IS THE `profile-card.tsx` BUG docs/28 §4 NAMES. That line was
 * `new Date(profile.created_at).toLocaleDateString()` — no locale argument,
 * so the rendering was decided by whatever default the *runtime* had. The
 * same row of data therefore printed `9/22/2026` on a US-configured host and
 * `22.09.2026` on a German one, and nothing in the repository said which of
 * the two was correct. That is "locale formatting 造成歧义" literally.
 *
 * Explicit format, not explicit locale. The two choices were:
 *   (a) `toLocaleDateString("en-CA")` — fixes the format to ISO-ish by
 *       naming a locale, but it still reads the *machine's* time zone, so
 *       the same instant renders as two different days on two hosts.
 *   (b) this: the UTC calendar fields, formatted by hand. One date per
 *       instant, on every machine, in both UI languages.
 * (b) is chosen because the value is the answer to "when was this account
 * created" and two hosts disagreeing about it is the defect being fixed.
 *
 * The cost, stated rather than hidden: it is the UTC day, so a user in
 * UTC+8 who joined at 02:00 local sees the previous day. That is a
 * one-line change (swap the getUTC* calls) if the product would rather have
 * the viewer's local day — but then it is no longer reproducible, and the
 * test that pins it would have to pin it against a fixed time zone.
 *
 * The date is NOT localized into a per-locale format on purpose: a date
 * rendered as `22.09.2026` in one language and `9/22/2026` in the other
 * would be exactly the ambiguity docs/28 §4 rules out, and a reader
 * comparing two screenshots would have to work out whether the value
 * changed or only its spelling.
 */
export function formatCalendarDate(value: string | number | Date): string {
  const date = value instanceof Date ? value : new Date(value);
  if (Number.isNaN(date.getTime())) return "";
  const year = String(date.getUTCFullYear()).padStart(4, "0");
  const month = String(date.getUTCMonth() + 1).padStart(2, "0");
  const day = String(date.getUTCDate()).padStart(2, "0");
  return `${year}-${month}-${day}`;
}
