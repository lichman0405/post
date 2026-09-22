/**
 * The remaining-hardcoded-string counter (T1105).
 *
 * WHY THIS FILE EXISTS
 *
 * The cheapest false claim an i18n task can make is "the core pages are
 * done, therefore the app is translated". An adjective cannot be re-run. A
 * count can. This scanner prints two numbers — literals that come from the
 * message catalog, and literals still hard-coded in the source — so the
 * claim in the RESULT can be checked by whoever reads it rather than
 * believed.
 *
 * WHY A REAL PARSER AND NOT A REGEX
 *
 * A grep for `"[A-Z][a-z]+"` cannot tell a rendered sentence from
 * `"application/json"`, and a grep for CJK cannot tell a rendered string
 * from a doc comment — this repository has 20 lines with Chinese characters
 * under apps/web and exactly one of them is user-visible text. The scanner
 * therefore parses with the same TypeScript compiler the app is typechecked
 * with and looks at NODES. A comment is trivia, never a node, so "is this
 * Chinese text user-visible?" is answered structurally rather than by
 * guessing at the grep hit's surroundings.
 *
 * WHAT COUNTS AS A CANDIDATE (the rules, in full)
 *
 *   1. A JSXText node whose text — after HTML entities are removed —
 *      contains a letter or a digit. JSX text is by construction rendered,
 *      so there is no heuristic here at all. `{" "}`, `·`, `—` and the
 *      `&apos;` entity do not contain a letter once entities are stripped
 *      and are therefore not candidates.
 *   2. A JSX attribute in VISIBLE_ATTRIBUTES whose initializer is a string
 *      literal. These are the attributes a browser or an assistive
 *      technology reads out: placeholder, title, aria-label, alt.
 *   3. A string literal that is the value of an object-literal property
 *      named in VISIBLE_PROPERTIES (`label: "Home"`). This rule exists
 *      because of a hole that was found by asking what the FIRST version of
 *      this scanner could not see: the global navigation's eight labels live
 *      in a `.ts` data table, and a sentence-shape rule cannot see a
 *      one-word label — `label: "Home"` has no space in it. The navigation
 *      is on every core page, so a counter blind to it would have reported
 *      core pages as translated while the header above them stayed English.
 *      Property names are a structural marker, so this rule is not a
 *      heuristic.
 *   4. A string literal in a `.ts` file that reads like a sentence — it
 *      contains a space AND at least SENTENCE_LETTERS letters. This is the
 *      one heuristic in the file and it exists because message maps
 *      (`messageForProfileCode`, the TYPE_LABELS tables) are plain objects
 *      in `.ts` modules, where no structural marker distinguishes a
 *      user-visible sentence from a wire token. It over-counts rather than
 *      under-counts, which is the direction an honest counter should err:
 *      `"text/plain; charset=utf-8"` is reported and a human dismisses it,
 *      whereas an under-counting rule would silently mark a real string as
 *      translated.
 *
 * WHAT IS "IN THE CATALOG"
 *
 * A candidate that is the first argument of a call to `t(...)` is counted as
 * coming from the catalog, not as hard-coded. That is the only shape the
 * catalog is allowed to be read through (see lib/i18n.ts), so a file that
 * renders nothing but `{t("…")}` reports zero.
 *
 * WHAT IS IN THE `core` GROUP, AND WHAT IS NOT (T1105)
 *
 * The group is the set of files whose hard-coded strings this repository
 * claims are ZERO. It is a list of FILES, not of routes, and a file at zero
 * means exactly that: THIS FILE renders no copy from its own source. It never
 * meant "the page this file belongs to is translated" — app/(main)/assets/page.tsx
 * is listed while its child assets-browse.tsx carries seven literals that are
 * not in the catalog, and the route-level truth is the coverage table in the
 * task's RESULT, not this list.
 *
 * A file belongs in the group when it puts copy on a CORE PAGE'S DEFAULT
 * RENDER PATH: the shell, the data-present body, the empty state — the render
 * the page produces when the API answers. Refusal paths (an error panel, a
 * catch handler, an ?code= notice) are NOT the default path. That is the
 * boundary, it is drawn in writing here, and CORE_LIB_OUT records every lib
 * module the criterion leaves outside so the boundary can be checked by
 * whoever reads a run rather than guessed.
 *
 * The criterion puts the lib modules the core pages render THROUGH into the
 * group with them: lib/pulls.ts (the PR page's tab labels and risk
 * sentences), lib/assets.ts (the asset page's type label and refusal lines),
 * lib/search.ts (the answer body's fallback headline and refusal lines),
 * lib/research-profile.ts (the profile cards' affiliation window, channel
 * words and refusal lines) and lib/i18n.ts, lib/i18n-server.ts,
 * lib/server-config.ts (the catalog itself and the two modules that read it —
 * they carry no copy of their own, and a literal added to one of them has to
 * red here rather than in a reader's eyes). Before this, the gate printed
 * "0 in 37 file(s)" while those modules rendered English inside the very
 * pages it was reporting on.
 *
 * reachableCoreLib() recomputes, from the tree, which lib modules the group's
 * files import at runtime. The classification lists are checked against that
 * set in BOTH directions, so a module that enters a core page's import graph
 * has to be classified here, and a classification that no longer describes
 * the tree fails too.
 *
 * THE FLOOR. CORE_FILES_FLOOR and CORE_LIB_REACHABLE_FLOOR are written as the
 * NUMBERS they are and compared against what the tree says, not re-derived
 * from the lists being checked: a member that leaves a list because its count
 * was inconvenient reds the gate instead of quietly shrinking it. Raise them
 * when members are added; never lower one to match a smaller tree.
 *
 * WHAT IT CANNOT SEE — stated rather than left to be discovered
 *
 *   - A user-visible string built at runtime by concatenation or by a
 *     template literal whose parts are not catalog keys.
 *   - A string that arrives from the API and is rendered verbatim. Those are
 *     data, not UI copy, and localizing them is explicitly out of scope
 *     (docs/28 §3: domain values keep their stable code).
 *   - Whether a `t()` key actually exists in the catalog — that is the
 *     catalog-parity check's job, not this one, and it is checked there
 *     (the scanner reports `missing` so the two cannot disagree).
 *
 * USAGE
 *
 *   node tests/i18n/scan-hardcoded.mjs                  # human report
 *   node tests/i18n/scan-hardcoded.mjs --json           # machine report
 *   node tests/i18n/scan-hardcoded.mjs --gate app,i18n  # exit 1 when a
 *                                                       # gated group is
 *                                                       # non-zero
 *
 * The `--gate` groups are the ones whose zero this repository claims. A
 * group that matches no file is a failure, not an empty pass: a gate whose
 * glob stopped matching would otherwise report green forever.
 */
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { createRequire } from "node:module";

const HERE = path.dirname(fileURLToPath(import.meta.url));
const ROOT = path.resolve(HERE, "../..");
const WEB = path.join(ROOT, "apps/web");

/* The TypeScript compiler the app is typechecked with, resolved from the web
 * workspace rather than installed separately: a second copy could drift from
 * the parser that actually reads this code. A missing compiler is a failed
 * run and not a smaller one — the whole point of this file is that a count
 * nobody could compute is not a count of zero. */
const TYPESCRIPT_CANDIDATES = [
  path.join(WEB, "node_modules/typescript"),
  path.join(ROOT, "node_modules/typescript"),
];
const typescriptDir = TYPESCRIPT_CANDIDATES.find((p) =>
  fs.existsSync(path.join(p, "lib/typescript.js")),
);
if (typescriptDir === undefined) {
  console.error(
    "scan-hardcoded: FAILED — the TypeScript compiler is not installed. Looked in:\n" +
      TYPESCRIPT_CANDIDATES.map((p) => `  ${p}`).join("\n") +
      "\nThis scanner parses the source; without the parser it cannot count" +
      " anything, and reporting 0 hard-coded strings because nothing could be" +
      " read is the exact failure this file exists to prevent." +
      "\nRun 'pnpm install --frozen-lockfile' at the repository root first.",
  );
  process.exit(2);
}
const require = createRequire(path.join(typescriptDir, "package.json"));
const ts = require("typescript");

/* The attributes a browser or a screen reader reads out loud. `id`, `href`,
 * `data-*` and `name` are deliberately NOT here: those are the stable codes
 * docs/28 §3 requires to stay untranslated, and a scanner that flagged them
 * would be arguing for the thing the spec forbids. */
const VISIBLE_ATTRIBUTES = new Set([
  "placeholder",
  "title",
  "aria-label",
  "aria-description",
  "aria-roledescription",
  "alt",
  /* `label` arrived with the asset page and the release detail: Primer's
   * `<Sidebar label="…">` and the labelled regions put reader-facing copy in
   * an attribute this scanner was blind to. Without it, a hard-coded
   * `label="Release breadcrumb"` is invisible — and the file it lives in
   * would still print "0 hard-coded strings", which is the false zero this
   * file exists to prevent. */
  "label",
]);

/* Object properties whose value is copy rather than a stable code. Rules 3
 * and 4 in the header; rule 3 reads this set. */
const VISIBLE_PROPERTIES = new Set([
  "label",
  "labels",
  "title",
  "text",
  "description",
  "message",
  "caption",
  "placeholder",
  "hint",
  "note",
  "summary",
  "tooltip",
  "ariaLabel",
]);

/* The one heuristic threshold in this file — see the header. */
const SENTENCE_LETTERS = 5;

/* Text that is RENDERED but is not copy, keyed by `<file relative to repo
 * root> :: <exact text>`.
 *
 * WHY AN ALLOWLIST AND NOT A RULE. The only two entries are the product
 * wordmark: `<span>POST</span>` in the two shells. A rule that recognized it
 * ("a single all-caps ASCII token") would exempt every future wordmark-shaped
 * string in the tree, including one that SHOULD be translated — a scanner
 * hole is worse than a scanner exception, because the exception is visible and
 * the hole is not. So the exemption is enumerated, keyed to the exact file
 * and the exact text, and the RUN COUNTS HOW MANY IT SKIPPED: a third
 * `<span>POST</span>` somewhere else still fails the core group, and the
 * skipped count printed below is what makes this list impossible to grow
 * silently. */
const NON_COPY_TEXT = new Map([
  ["apps/web/app/(auth)/layout.tsx :: POST", "the product wordmark in the (auth) shell"],
  ["apps/web/app/nav/global-nav.tsx :: POST", "the product wordmark in the (main) shell"],
]);

/* Paths that are not product UI copy. */
const SKIP_DIRS = new Set(["node_modules", ".next", "dist", "coverage"]);

/* Files whose hard-coded count this task claims is zero.
 *
 * WHY THE `core` GROUP IS AN EXPLICIT LIST AND NOT A DIRECTORY.
 *
 * A directory is a denominator nobody reads. `core: ["app"]` would say "every
 * rendered component" and would be true on the day it was written and false
 * the moment a file was added — and it hides WHICH files the claim covers, so
 * "0 hard-coded strings" and "0 hard-coded strings in the 26 files I chose"
 * print the same number. This task's whole risk is a coverage claim that is
 * broader than its evidence (the cheapest false claim being "core pages done
 * → whole site done"), so the claim names its files.
 *
 * WHY EVERY PATH IS CHECKED TO EXIST. A renamed or deleted file would
 * silently leave the group, and a group that shrinks is a denominator that
 * shrinks — the count stays 0 and gets easier to keep at 0. `--gate` therefore
 * FAILS on a listed file that is not there, rather than skipping it.
 *
 * `app` is the wide group: every .ts/.tsx under apps/web/app. Its number is
 * reported on every run and is NOT claimed to be zero — the pages this task
 * did not convert are in it, and the RESULT's coverage table is where that
 * shortfall is stated. It exists so the honest number is printed next to the
 * claimed one instead of being absent. */
/* ---- The core group's lib half, and the modules it leaves out (T1105) ----
 * See the header, "WHAT IS IN THE `core` GROUP". Paths are relative to
 * apps/web, like every other entry in GATE_GROUPS.core.
 *
 * IN: the modules that put copy on a core page's default render path. Each
 * one is a plain `.ts` module that cannot reach the catalog, so its labels and
 * sentences are catalog KEYS (`pullTabLabelKey`, `assetTypeLabelKey`,
 * `searchCodeKey`, `researchProfileCodeKey`, …) resolved by the page that
 * imports it — which is why these files can be at zero and the page still
 * render Chinese.
 *
 * OUT: reachable from a core file, but their copy renders only on a refusal
 * path, or on a page that is not one of docs/42's core pages. Every run
 * prints this list with each module's live count, so the exclusion is visible
 * and the count cannot drift unwatched. */
export const CORE_LIB_MODULES = [
  // The PR page (docs/42's Pull request): tab labels, tab hints, the
  // integrity dimensions, the risk sentences and the refusal lines.
  "lib/pulls.ts",
  // The asset page: the type label in the header and the refusal lines.
  "lib/assets.ts",
  // The search answer body: the fallback headline and the refusal lines.
  "lib/search.ts",
  // The research/organization profile cards: the affiliation window, the
  // channel words, the reproduction relation names and the refusal lines.
  "lib/research-profile.ts",
  // The catalog itself. It is the answer, not a caller of it: this scanner
  // counts its KEYS (600-) and skips its VALUES, and a literal added to it
  // would be a locale value in the wrong place. Listed here so that the
  // reachable set below is exactly the group plus the out-list.
  "lib/i18n.ts",
  // Reads the catalog for server components, and the CSP/API-origin config.
  // Both carry no copy today; they are here so that a sentence added to one
  // of them reds in the gate rather than in a reader's eyes.
  "lib/i18n-server.ts",
  "lib/server-config.ts",
];

export const CORE_LIB_OUT = [
  [
    "lib/auth.ts",
    "messageForCode/messageForOIDCError render in login-card's catch handler and in the ?code= notice (app/(auth)/login/login-card.tsx:38,48,50) — the login page's own render is the catalog's, and the fixture's /login route carries no code. Its 16 literals are the refusal-path gap this round leaves English.",
  ],
  [
    "lib/profile.ts",
    "messageForProfileCode renders in the profile card's failure and 404 branches (app/(main)/users/[id]/profile-card.tsx:80,120,144); the card renders the profile itself when the API answers. 9 literals, refusal path only.",
  ],
  [
    "lib/projects.ts",
    "messageForProjectCode renders in the directory's and the shell's catch handlers (projects/project-directory.tsx:44, projects/[id]/project-shell.tsx:96) and on the settings page, which is not a core page. 12 literals, refusal path only.",
  ],
  [
    "lib/publish.ts",
    "messageForPublishCode renders only on projects/[id]/assets/publish, which is not one of docs/42's core pages; the asset page's own publish entry already resolves through the catalog (publishEntry(t, page)). 25 literals, none on a core page's default path.",
  ],
  [
    "lib/releases.ts",
    "messageForReleaseCode renders in the release list's and detail's catch handlers (releases/page.tsx:90,133, releases/[releaseId]/release-detail.tsx:61). 10 literals, refusal path only.",
  ],
];

/* The floor: the group's SIZE, as a number, checked against what the tree
 * says. 37 app files + 7 lib modules. Raise it when you add a member. */
export const CORE_FILES_FLOOR = 44;

/* The number of lib modules the group's files import at runtime: the 7 above
 * plus the 5 in CORE_LIB_OUT. raise-only, like CORE_FILES_FLOOR. */
export const CORE_LIB_REACHABLE_FLOOR = 12;

export const GATE_GROUPS = {
  core: [
    "app/layout.tsx",
    "app/providers.tsx",
    "app/i18n-provider.tsx",
    "app/nav/global-nav.tsx",
    "app/nav/nav-links.tsx",
    "app/nav/nav-links-static.tsx",
    "app/nav/nav-bell.tsx",
    "app/nav/nav-user-area.tsx",
    "app/nav/nav-destinations.ts",
    "app/nav/mobile-nav-menu.tsx",
    "app/nav/language-switcher.tsx",
    "app/(main)/layout.tsx",
    "app/(main)/status-panel.tsx",
    "app/(main)/auth-status.tsx",
    "app/(main)/projects/project-directory.tsx",
    "app/(main)/projects/project-badges.tsx",
    "app/(main)/projects/project-tabs.ts",
    "app/(main)/projects/[id]/project-shell.tsx",
    "app/(main)/projects/[id]/page.tsx",
    "app/(main)/search/page.tsx",
    "app/(main)/users/[id]/profile-card.tsx",
    "app/(main)/users/[id]/research-profile-card.tsx",
    "app/(main)/organizations/[slug]/organization-profile-card.tsx",
    "app/components/research-profile-sections.tsx",
    "app/(auth)/layout.tsx",
    "app/(auth)/login/login-card.tsx",
    // The four route entry files whose own copy (the tab title, and nothing
    // else) now comes from the catalog. They are listed individually rather
    // than by directory for the same reason as everything above, and the
    // caveat is the same one: a file at 0 here means THIS FILE renders no
    // hard-coded copy. It does not mean the route is internationalized —
    // app/(main)/assets/page.tsx is listed, and its child assets-browse.tsx
    // carries 7 literals that are NOT in the catalog. The route-level truth
    // is the coverage table in the task's RESULT, not this list.
    "app/(auth)/login/page.tsx",
    "app/(main)/projects/page.tsx",
    "app/(main)/organizations/[slug]/page.tsx",
    "app/(main)/assets/page.tsx",
    // The five routes the first review found still English (plus the pull-list
    // tab converted in the same pass). Their entry files were already at 0 —
    // what the review caught lived one level down, in the component the route
    // renders, which is why an entry-file list is not the claim. Listed by
    // path, one line each, same reason as above.
    "app/(main)/projects/[id]/research/page.tsx",
    "app/(main)/projects/[id]/pulls/page.tsx",
    "app/(main)/projects/[id]/pulls/[number]/page.tsx",
    "app/(main)/projects/[id]/releases/page.tsx",
    "app/(main)/projects/[id]/releases/[releaseId]/release-detail.tsx",
    "app/(main)/assets/asset-page.tsx",
    "app/(main)/search/search-answer.tsx",
    // The lib half: the modules those pages render THROUGH (T1105's third
    // round — see CORE_LIB_MODULES above and the header). They are listed by
    // path exactly like the files above, and the floor counts them.
    ...CORE_LIB_MODULES,
  ],
  app: ["app"],
};

/* ------------------------------------------------------------------ */

function* walk(dir) {
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    if (entry.name.startsWith(".") && entry.name !== ".") {
      if (SKIP_DIRS.has(entry.name)) continue;
    }
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) {
      if (SKIP_DIRS.has(entry.name)) continue;
      yield* walk(full);
    } else if (entry.isFile() && /\.tsx?$/.test(entry.name)) {
      yield full;
    }
  }
}

/* Entities carry letters (`&apos;` would otherwise read as the word "apos"),
 * so they are removed before the "does this text contain a letter" test. */
function stripEntities(text) {
  return text.replace(/&[a-zA-Z]+;/g, " ").replace(/&#\d+;/g, " ");
}

const WORD = /[\p{L}\p{N}]/u;
const CJK = /[一-鿿]/u;

function line(text, pos) {
  return text.slice(0, pos).split("\n").length;
}

function isT(call) {
  return ts.isCallExpression(call) && ts.isIdentifier(call.expression) && call.expression.text === "t";
}

/* A string literal used as a catalog key: the first argument of `t(...)`. */
function tKey(node) {
  if (!ts.isCallExpression(node) || !isT(node)) return null;
  const first = node.arguments[0];
  if (first === undefined) return null;
  if (ts.isStringLiteral(first) || ts.isNoSubstitutionTemplateLiteral(first)) return first.text;
  return null;
}

function collect(file) {
  const text = fs.readFileSync(file, "utf8");
  const kind = file.endsWith(".tsx") ? ts.ScriptKind.TSX : ts.ScriptKind.TS;
  const source = ts.createSourceFile(file, text, ts.ScriptTarget.Latest, true, kind);

  const hard = [];
  const keys = [];
  const cjk = [];
  const nonCopy = [];
  const isTs = file.endsWith(".ts");
  const rel = path.relative(ROOT, path.resolve(file));

  /* Applied in ONE place rather than at each push site, so a rule added later
   * cannot quietly bypass the exemption — and, just as important, so the
   * exemption cannot quietly bypass a rule. */
  const push = (item) => {
    if (NON_COPY_TEXT.has(`${rel} :: ${item.value}`)) {
      nonCopy.push(item);
      return;
    }
    hard.push(item);
  };

  /* The catalog's own file holds message VALUES, which are the catalog. It
   * is the one file where a sentence-shaped literal is not a candidate. */
  const isCatalogFile = path.resolve(file) === path.join(WEB, "lib/i18n.ts");

  const visit = (node) => {
    if (ts.isCallExpression(node)) {
      const key = tKey(node);
      if (key !== null) {
        keys.push({ key, line: line(text, node.getStart(source)) });
      }
    }

    if (ts.isJsxText(node) && !isCatalogFile) {
      const cleaned = stripEntities(node.text);
      if (WORD.test(cleaned)) {
        const value = cleaned.replace(/\s+/g, " ").trim();
        push({ value, line: line(text, node.getStart(source)), rule: "jsx-text" });
        if (CJK.test(cleaned)) cjk.push({ value, line: line(text, node.getStart(source)) });
      }
    }

    if (ts.isJsxAttribute(node) && !isCatalogFile) {
      const name = node.name.getText(source);
      if (VISIBLE_ATTRIBUTES.has(name) && node.initializer !== undefined) {
        const init = node.initializer;
        if (ts.isStringLiteral(init) && WORD.test(init.text)) {
          push({
            value: init.text,
            line: line(text, init.getStart(source)),
            rule: `jsx-attr:${name}`,
          });
          if (CJK.test(init.text)) {
            cjk.push({ value: init.text, line: line(text, init.getStart(source)) });
          }
        }
      }
    }

    if (!isCatalogFile && ts.isStringLiteral(node)) {
      const parent = node.parent;
      /* A catalog key is not a candidate: it is the catalog's own address
       * (`t("nav.home")`), and counting it would make every translated
       * string count as untranslated. */
      const isKey = parent !== undefined && ts.isCallExpression(parent) && isT(parent);
      /* Rule 3: the value of a copy-shaped object property, in any file
       * type. `label: "Home"` in a .ts table is rendered by a component. */
      const isVisibleProperty =
        parent !== undefined &&
        ts.isPropertyAssignment(parent) &&
        (ts.isStringLiteral(parent.name) || ts.isIdentifier(parent.name)) &&
        VISIBLE_PROPERTIES.has(parent.name.text) &&
        WORD.test(node.text);
      /* Rule 4: the .ts sentence heuristic — see the header for why it is a
       * heuristic and why it over-counts on purpose. */
      const isSentence =
        isTs &&
        node.text.includes(" ") &&
        (node.text.match(/[\p{L}]/gu) ?? []).length >= SENTENCE_LETTERS;
      /* A string handed to `new Error(...)` is a developer message. It never
       * reaches the DOM, so it is not UI copy — and it is the one case in this
       * file where "it is English prose in a .ts file" is a shape the sentence
       * heuristic cannot tell from a rendered sentence. */
      const isThrownMessage =
        parent !== undefined &&
        ts.isNewExpression(parent) &&
        parent.expression.getText(source) === "Error";
      if (!isKey && !isThrownMessage && (isVisibleProperty || isSentence)) {
        const rule = isVisibleProperty ? `property:${parent.name.getText(source)}` : "sentence";
        push({ value: node.text, line: line(text, node.getStart(source)), rule });
        if (CJK.test(node.text)) cjk.push({ value: node.text, line: line(text, node.getStart(source)), rule });
      }
    }

    ts.forEachChild(node, visit);
  };
  visit(source);

  return { hard, keys, cjk, nonCopy };
}

/* The catalog's key set, read from the same AST. The catalog file is one
 * object literal keyed by locale, and each locale maps a flat dotted key to
 * its message — flat rather than nested so that parity between the two
 * locales is a set comparison and not a tree walk. */
export function readCatalog(catalogPath = path.join(WEB, "lib/i18n.ts")) {
  const text = fs.readFileSync(catalogPath, "utf8");
  const source = ts.createSourceFile(catalogPath, text, ts.ScriptTarget.Latest, true, ts.ScriptKind.TS);
  const out = {};
  const visit = (node) => {
    if (ts.isPropertyAssignment(node)) {
      const locale = ts.isStringLiteral(node.name) || ts.isIdentifier(node.name) ? node.name.text : null;
      if (locale !== null && ts.isObjectLiteralExpression(node.initializer)) {
        const keys = new Set();
        for (const prop of node.initializer.properties) {
          if (ts.isPropertyAssignment(prop) && ts.isStringLiteral(prop.name)) keys.add(prop.name.text);
        }
        if (keys.size > 0) out[locale] = keys;
      }
    }
    ts.forEachChild(node, visit);
  };
  visit(source);
  return out;
}

export function scan() {
  const files = [...walk(WEB)].sort();
  const perFile = [];
  let hardTotal = 0;
  const allKeys = new Map();
  const cjkAll = [];

  const nonCopyAll = [];
  for (const file of files) {
    const { hard, keys, cjk, nonCopy } = collect(file);
    const rel = path.relative(ROOT, file);
    for (const n of nonCopy) nonCopyAll.push({ file: rel, ...n });
    if (hard.length > 0) {
      perFile.push({ file: rel, hard });
      hardTotal += hard.length;
    }
    for (const k of keys) {
      if (!allKeys.has(k.key)) allKeys.set(k.key, rel);
    }
    for (const c of cjk) cjkAll.push({ file: rel, ...c });
  }

  const catalog = readCatalog();
  const locales = Object.keys(catalog).sort();
  const en = catalog.en ?? new Set();
  const referenced = new Set(allKeys.keys());
  const missingFromCatalog = [...referenced].filter((k) => locales.some((l) => !catalog[l].has(k))).sort();
  const unusedCatalogKeys = [...en].filter((k) => !referenced.has(k)).sort();
  const localeParity = locales.map((l) => ({
    locale: l,
    keys: catalog[l].size,
    onlyHere: [...catalog[l]].filter((k) => locales.some((o) => o !== l && !catalog[o].has(k))).sort(),
  }));

  return {
    files_scanned: files.length,
    hardcoded_total: hardTotal,
    per_file: perFile,
    catalog: {
      locales,
      sizes: Object.fromEntries(locales.map((l) => [l, catalog[l].size])),
      parity: localeParity,
    },
    referenced_keys: referenced.size,
    missing_from_catalog: missingFromCatalog,
    unused_catalog_keys: unusedCatalogKeys,
    cjk_non_comment: cjkAll,
    non_copy_skipped: nonCopyAll,
  };
}

/* A group's files, or null when the group name is unknown. Unknown group
 * names are a failure at the call site (see main) rather than an empty
 * match — see the header on why an empty glob must not read as green. */
/** Resolve a group to the files it covers, or null when the group is not one
 *  this file defines.
 *
 *  A DIRECTORY entry is walked; a FILE entry must exist. The missing-file case
 *  is a THROW rather than a `continue`: a group whose denominator can silently
 *  lose a member is a gate that gets easier to pass the more the tree moves,
 *  and the whole point of listing the files was that the number and the claim
 *  stay glued to each other. Callers turn the throw into a failed run. */
export function groupFiles(group) {
  const entries = GATE_GROUPS[group];
  if (entries === undefined) return null;
  const files = [];
  for (const entry of entries) {
    const full = path.join(WEB, entry);
    if (!fs.existsSync(full)) {
      throw new Error(
        `gate group \`${group}\` lists ${entry}, which does not exist under apps/web. ` +
          "A group that skips its missing members shrinks every time the tree moves " +
          "and reports 0 over a smaller set each time. Update the list (and the " +
          "coverage claim that cites it) or restore the file.",
      );
    }
    if (fs.statSync(full).isDirectory()) files.push(...walk(full));
    else files.push(full);
  }
  return files.map((f) => path.relative(ROOT, f)).sort();
}

/** Resolve one relative import specifier from `fromFile` to a module under
 *  apps/web/lib, named the way CORE_LIB_MODULES names it ("lib/x.ts"), or
 *  null when it resolves inside apps/web but outside lib/. */
function resolveLibImport(fromFile, spec) {
  const base = path.resolve(path.dirname(fromFile), spec);
  for (const candidate of [base, `${base}.ts`, `${base}.tsx`, path.join(base, "index.ts")]) {
    if (!fs.existsSync(candidate) || !fs.statSync(candidate).isFile()) continue;
    const rel = path.relative(WEB, candidate).split(path.sep).join("/");
    return rel.startsWith("lib/") ? rel : null;
  }
  return null;
}

/**
 * Every lib module the files in the `core` group import AT RUNTIME, with the
 * core files that import it — computed from the tree, not from the lists.
 *
 * Type-only imports are dropped: `import type` never executes, Node's type
 * stripping removes it, and a module reached only that way puts no string on
 * any page (lib/pulls.ts imports `Translate` that way, on purpose, so that the
 * import graph of the RUNTIME stays exactly the shape this file's suite
 * supports).
 *
 * The result is what the classification lists are checked against, in both
 * directions, so that this set cannot grow or shrink without someone writing
 * down what the change means. */
export function reachableCoreLib() {
  const out = new Map();
  for (const entry of GATE_GROUPS.core) {
    if (!/\.tsx?$/.test(entry) || entry.startsWith("lib/")) continue;
    const file = path.join(WEB, entry);
    const text = fs.readFileSync(file, "utf8");
    const source = ts.createSourceFile(
      file,
      text,
      ts.ScriptTarget.Latest,
      true,
      entry.endsWith(".tsx") ? ts.ScriptKind.TSX : ts.ScriptKind.TS,
    );
    for (const statement of source.statements) {
      if (!ts.isImportDeclaration(statement)) continue;
      if (statement.importClause !== undefined && statement.importClause.isTypeOnly) continue;
      const spec = statement.moduleSpecifier;
      if (!ts.isStringLiteral(spec) || !spec.text.startsWith(".")) continue;
      const rel = resolveLibImport(file, spec.text);
      if (rel === null) continue;
      if (!out.has(rel)) out.set(rel, new Set());
      out.get(rel).add(entry);
    }
  }
  return out;
}

/**
 * The T1105 checks that turn the core group's lib half from a claim into a
 * measurement. Returns the lines to print; throws when the tree and the
 * classification disagree. Called before anything is counted, so a run whose
 * lists do not describe the tree fails instead of printing a number. */
function checkCoreLibHalf() {
  const reachable = reachableCoreLib();
  const groupSet = new Set(CORE_LIB_MODULES);
  const outSet = new Set(CORE_LIB_OUT.map(([file]) => file));
  const classified = new Set([...groupSet, ...outSet]);

  const unclassified = [...reachable.keys()].filter((f) => !classified.has(f)).sort();
  if (unclassified.length > 0) {
    throw new Error(
      `the core group's files import ${unclassified.join(", ")}, which is in neither ` +
        "CORE_LIB_MODULES nor CORE_LIB_OUT. A lib module on a core page's render path " +
        "has to be classified: put it in the group if it renders copy on that page's " +
        "default path, or in the out-list with the reason it does not — a run that " +
        "leaves it unclassified is a run whose 0 does not cover it.",
    );
  }
  const stale = [...classified].filter((f) => !reachable.has(f)).sort();
  if (stale.length > 0) {
    throw new Error(
      `the classification lists ${stale.join(", ")}, which no file in the core group ` +
        "imports any more. Either a core file stopped using it (then the coverage claim " +
        "and this list have to be updated together) or it moved, and either way the " +
        "lists no longer describe the tree the gate reports on.",
    );
  }
  if (reachable.size !== CORE_LIB_REACHABLE_FLOOR) {
    throw new Error(
      `the core group's files import ${reachable.size} lib module(s); ` +
        `CORE_LIB_REACHABLE_FLOOR is ${CORE_LIB_REACHABLE_FLOOR}. Raise the floor if a ` +
        "module was added (and classify it); do not lower it to match a smaller tree.",
    );
  }

  return {
    reachable,
    excluded: CORE_LIB_OUT.map(([file, why]) => ({
      file,
      why,
      importers: [...reachable.get(file)].sort(),
    })),
  };
}

function main() {
  const argv = process.argv.slice(2);
  const asJson = argv.includes("--json");
  const gateIdx = argv.indexOf("--gate");
  const gates = gateIdx === -1 ? [] : (argv[gateIdx + 1] ?? "").split(",").filter(Boolean);

  /* T1105: the lib half of the core group, checked BEFORE anything is
   * counted. A tree whose classification lists no longer describe it gets no
   * number out of this program — see checkCoreLibHalf. */
  let coreLib;
  try {
    coreLib = checkCoreLibHalf();
  } catch (err) {
    console.error(`scan-hardcoded: FAILED — ${err.message}`);
    process.exit(2);
  }

  const result = scan();

  /* The per-group counts, computed from the same run rather than a second
   * scan, so the report and the gate cannot disagree. */
  const groups = {};
  for (const name of Object.keys(GATE_GROUPS)) {
    let files;
    try {
      files = groupFiles(name);
    } catch (err) {
      console.error(`scan-hardcoded: FAILED — ${err.message}`);
      process.exit(2);
    }
    const set = new Set(files);
    groups[name] = {
      files: files.length,
      hardcoded: result.per_file.filter((f) => set.has(f.file)).reduce((n, f) => n + f.hard.length, 0),
    };
  }

  const report = { ...result, groups, core_lib: { in_gate: CORE_LIB_MODULES, excluded: coreLib.excluded } };

  /* The floor, checked in every mode: the group is the sum of named parts
   * (CORE_FILES_FLOOR), and a run that reports a smaller set than the floor
   * is a run whose zero covers less than the claim — which is the failure
   * mode this whole file exists to prevent. */
  if (groups.core.files !== CORE_FILES_FLOOR) {
    console.error(
      `scan-hardcoded: FAILED — the core group matched ${groups.core.files} file(s); ` +
        `CORE_FILES_FLOOR is ${CORE_FILES_FLOOR}. A member left the list (or a path moved). ` +
        "Restore it, or raise the floor deliberately in the same change — never lower it " +
        "to match a smaller tree.",
    );
    process.exit(2);
  }

  if (asJson) {
    console.log(JSON.stringify(report, null, 2));
  } else {
    console.log(`scan-hardcoded: ${result.files_scanned} source file(s) parsed under apps/web`);
    console.log(`scan-hardcoded: IN CATALOG      ${result.catalog.sizes.en ?? 0} entries` +
      ` (${result.catalog.locales.join(", ")})`);
    console.log(`scan-hardcoded: REFERENCED      ${result.referenced_keys} distinct t() key(s) used in the UI`);
    console.log(`scan-hardcoded: NOT IN CATALOG  ${result.hardcoded_total} hard-coded literal(s) still rendered from source`);
    for (const [name, g] of Object.entries(groups)) {
      console.log(`scan-hardcoded:   gate '${name}': ${g.hardcoded} in ${g.files} file(s)`);
    }
    /* The lib half, printed on every run: the modules in the group are part of
     * the group's own count above, and the ones the criterion leaves out are
     * listed here WITH their live counts, so an exclusion is a sentence on
     * the record rather than a silence. */
    const hardByFile = new Map(result.per_file.map((f) => [f.file, f.hard.length]));
    console.log(
      `scan-hardcoded: core lib modules IN the gate: ${CORE_LIB_MODULES.length}` +
        ` (of ${coreLib.reachable.size} reachable from the core group)`,
    );
    console.log(
      `scan-hardcoded: core lib modules OUT of the gate, by the criterion "copy on a core page's default render path": ${coreLib.excluded.length}`,
    );
    for (const { file, why, importers } of coreLib.excluded) {
      const rel = `apps/web/${file}`;
      console.log(`  ${file} — ${hardByFile.get(rel) ?? 0} literal(s), imported by ${importers.length} core file(s)`);
      console.log(`    why: ${why}`);
    }
    if (result.missing_from_catalog.length > 0) {
      console.log(`scan-hardcoded: MISSING FROM CATALOG ${result.missing_from_catalog.length}: ` +
        result.missing_from_catalog.join(", "));
    }
    /* Printed on every run, not only when it is non-zero: the exemption list
     * above is a claim about the tree, and a reader has to be able to see that
     * it is exactly the two wordmarks it says it is. */
    console.log(`scan-hardcoded: EXEMPT (rendered, not copy) ${result.non_copy_skipped.length}`);
    for (const n of result.non_copy_skipped) {
      const reason = NON_COPY_TEXT.get(`${n.file} :: ${n.value}`) ?? "(no reason recorded)";
      console.log(`  ${n.file}:${n.line} ${JSON.stringify(n.value)} — ${reason}`);
    }
    console.log(`scan-hardcoded: CJK outside comments ${result.cjk_non_comment.length}`);
    for (const c of result.cjk_non_comment) {
      console.log(`  ${c.file}:${c.line} [${c.rule}] ${JSON.stringify(c.value)}`);
    }
    if (result.hardcoded_total > 0) {
      console.log("scan-hardcoded: the largest remaining files:");
      for (const f of [...result.per_file].sort((a, b) => b.hard.length - a.hard.length).slice(0, 15)) {
        console.log(`  ${String(f.hard.length).padStart(4)}  ${f.file}`);
      }
    }
  }

  if (gates.length > 0) {
    let bad = false;
    for (const name of gates) {
      if (groups[name] === undefined) {
        console.error(`scan-hardcoded: FAILED — '${name}' is not a gate group; known: ${Object.keys(GATE_GROUPS).join(", ")}`);
        bad = true;
        continue;
      }
      if (groups[name].files === 0) {
        console.error(`scan-hardcoded: FAILED — gate '${name}' matched no source file; an empty gate is not a green gate.`);
        bad = true;
        continue;
      }
      if (groups[name].hardcoded > 0) {
        console.error(`scan-hardcoded: FAILED — gate '${name}': ${groups[name].hardcoded} hard-coded literal(s) still rendered from source.`);
        bad = true;
      }
    }
    if (bad) process.exit(1);
    console.log(`scan-hardcoded: gate(s) ${gates.join(", ")}: 0 hard-coded literals.`);
  }

  /* CJK outside comments is a hard invariant, not a reported number: it is
   * the one string this task was told to move and it must not come back. */
  if (result.cjk_non_comment.length > 0) {
    console.error(`scan-hardcoded: FAILED — ${result.cjk_non_comment.length} Chinese string(s) outside comments; every one is user-visible text that belongs in the catalog.`);
    process.exit(1);
  }
}

if (process.argv[1] !== undefined && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main();
}
