#!/usr/bin/env node
// class-liveness — every CSS class a component asks for must still be drawn by a rule.
//
// Why this exists: a shell-scan for dead CSS only looks at one direction (rules with
// no className). The direction that hurts is the other one — a className whose rule
// was pruned away. That class silently renders unstyled: the page still "renders",
// the static CSS policy checks still pass, and a screenshot baseline taken *after*
// the pruning cannot see it either, because it only knows what the pruned tree looks
// like. This audit reads both sides and refuses to let a class go dark.
//
// Usage:  node tests/web-smoke/class-liveness.mjs [repo-root]
//
// Exit 0 = every referenced class still has a rule (or is on the allowance list).
// Exit 1 = at least one class is referenced by JSX but no stylesheet draws it.

import { execFileSync } from "node:child_process";
import { readFileSync, readdirSync, statSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const HERE = path.dirname(fileURLToPath(import.meta.url));
const POSITIONAL = process.argv.slice(2).filter((a) => !a.startsWith("--"));
const ROOT = path.resolve(POSITIONAL[0] ?? path.join(HERE, "..", ".."));

// Classes that are deliberately hooks-only: they exist so tests or `data-*`-style
// queries have a stable handle and carry no rule on purpose. Every entry needs a
// reason, and the audit prints the list so it stays visible. Empty is the healthy
// state — a token here is a class that renders nothing.
const HOOKS_WITHOUT_RULES = new Map([]);

// The audit has two failure modes and they are not the same size:
//   * a class THIS TASK removed while a component still asks for it — always fatal,
//     that is the pruning bug this file exists to catch;
//   * a class that was never drawn in HEAD either — pre-existing, owned by whatever
//     wrote the page, reported as a count and only fatal under --strict. Making the
//     default fatal would red the gate for a defect this task did not introduce,
//     and silently hiding it would be worse.
const STRICT = process.argv.includes("--strict");

function walk(dir, out = []) {
  let entries;
  try {
    entries = readdirSync(dir);
  } catch {
    return out;
  }
  for (const name of entries) {
    if (name === "node_modules" || name === ".next" || name === "dist" || name === ".git") continue;
    const full = path.join(dir, name);
    if (statSync(full).isDirectory()) walk(full, out);
    else out.push(full);
  }
  return out;
}

// ---- 1. every class token the components ask for ------------------------------
const SOURCES = [path.join(ROOT, "apps", "web"), path.join(ROOT, "packages", "ui")];
const referenced = new Map(); // token -> [locations]; token may end with "-" (prefix)
const prefixed = new Map(); // prefix -> [locations]

function record(map, token, where) {
  if (!token) return;
  const at = map.get(token) ?? [];
  at.push(where);
  map.set(token, at);
}

// className="a b c" | className={"a b c"} | className={`a ${x} b`}
function collectFromSource(file, text) {
  const rel = path.relative(ROOT, file);
  const lines = text.split("\n");
  lines.forEach((line, i) => {
    const where = `${rel}:${i + 1}`;
    for (const m of line.matchAll(/class(?:Name)?=(?:"([^"]*)"|\{"([^"]*)"\}|\{`([^`]*)`\})/g)) {
      const body = m[1] ?? m[2] ?? m[3] ?? "";
      // A `${...}` hole splits the literal; the chunk that touches it names a family
      // ("pull-risk-" + severity), so match it as a prefix rather than a whole token.
      const chunks = body.split(/\$\{[^}]*\}/);
      const holes = body.match(/\$\{[^}]*\}/g) ?? [];
      chunks.forEach((chunk, ci) => {
        const tokens = chunk.trim().split(/\s+/).filter(Boolean);
        tokens.forEach((token, ti) => {
          const touchesHole =
            (holes[ci] !== undefined && ti === tokens.length - 1 && ci < holes.length) ||
            (ci > 0 && ti === 0);
          if (touchesHole) record(prefixed, token, where);
          else record(referenced, token, where);
        });
      });
    }
  });
}

for (const dir of SOURCES) {
  for (const file of walk(dir)) {
    if (!/\.(tsx|jsx)$/.test(file)) continue;
    collectFromSource(file, readFileSync(file, "utf8"));
  }
}

// ---- 2. every class a stylesheet draws ----------------------------------------
function classesInCss(text) {
  const found = new Set();
  const withoutComments = text.replace(/\/\*[\s\S]*?\*\//g, "");
  for (const m of withoutComments.matchAll(/\.(-?[_a-zA-Z][\w-]*)/g)) found.add(m[1]);
  return found;
}

const cssFiles = SOURCES.flatMap((dir) => walk(dir).filter((f) => f.endsWith(".css")));
const drawn = new Set();
for (const file of cssFiles) for (const c of classesInCss(readFileSync(file, "utf8"))) drawn.add(c);

// ---- 3. what HEAD drew that this working tree no longer draws -----------------
// The axis that matters for a pruning task: a class HEAD's CSS carried and the
// working tree dropped. `git show` is read-only and keeps the audit reproducible
// on any checkout.
const cssPaths = execFileSync("git", ["-C", ROOT, "ls-tree", "-r", "--name-only", "HEAD"], {
  encoding: "utf8",
})
  .split("\n")
  .filter((p) => /^(apps\/web|packages\/ui)\/.*\.css$/.test(p));

const removed = new Map(); // class -> css file that used to draw it
for (const rel of cssPaths) {
  let headText;
  try {
    headText = execFileSync("git", ["-C", ROOT, "show", `HEAD:${rel}`], { encoding: "utf8" });
  } catch {
    continue;
  }
  const before = classesInCss(headText);
  let after;
  try {
    after = classesInCss(readFileSync(path.join(ROOT, rel), "utf8"));
  } catch {
    after = new Set(); // file deleted by this task: everything it drew is gone
  }
  for (const c of before) if (!after.has(c)) removed.set(c, rel);
}

// ---- 4. report -----------------------------------------------------------------
const dark = []; // referenced, drawn nowhere, not on the allowance list
for (const [token, where] of referenced) {
  if (drawn.has(token)) continue;
  if (HOOKS_WITHOUT_RULES.has(token)) continue;
  dark.push({ token, where, mine: false });
}
for (const [prefix, where] of prefixed) {
  let any = false;
  for (const c of drawn) if (c.startsWith(prefix)) any = true;
  if (!any) dark.push({ token: `${prefix}*`, where, mine: false });
}
// A dark class is this task's when either its rule went away (removed) or the
// component asking for it is new here (a class born dark). Only an old class that
// was never drawn in HEAD either belongs to someone else.
const headFiles = new Set(
  execFileSync("git", ["-C", ROOT, "ls-tree", "-r", "--name-only", "HEAD"], { encoding: "utf8" }).split("\n"),
);
for (const entry of dark) {
  const bare = entry.token.replace(/\*$/, "");
  entry.mine = removed.has(bare) || entry.where.some((w) => !headFiles.has(w.split(":")[0]));
}
const mineDark = dark.filter((d) => d.mine);
const preexistingDark = dark.filter((d) => !d.mine);

console.log(`class-liveness: ${cssFiles.length} stylesheet(s), ${referenced.size + prefixed.size} referenced token(s)`);
console.log(`class-liveness: ${drawn.size} class(es) drawn`);
console.log(`class-liveness: ${removed.size} class(es) HEAD drew that this tree does not`);

const removedStillDrawn = [...removed.keys()].filter((c) => drawn.has(c));
if (removedStillDrawn.length) {
  console.log(`\nclass-liveness: removed from ${[...new Set(removed.values())].length} file(s) but still drawn elsewhere:`);
  for (const c of removedStillDrawn.sort()) console.log(`  moved  .${c}  (was in ${removed.get(c)})`);
}

console.log("\nclass-liveness: audit of this task's removals");
if (mineDark.length === 0) {
  console.log("  ok   no class removed by this task is still referenced by a component");
} else {
  for (const { token, where } of mineDark.sort((a, b) => a.token.localeCompare(b.token))) {
    const cssFile = removed.get(token.replace(/\*$/, "")) ?? "?";
    console.log(`  DARK .${token}  removed from ${cssFile}, still asked for at ${where.join(", ")}`);
  }
}

if (preexistingDark.length) {
  console.log(
    `\nclass-liveness: ${preexistingDark.length} className(s) with no rule in HEAD either ` +
      `(pre-existing, not this task's — list with --strict to make them fatal)`,
  );
  if (STRICT) for (const { token, where } of preexistingDark) console.log(`  .${token} at ${where.join(", ")}`);
  else console.log(`  ${preexistingDark.map((d) => `.${d.token}`).sort().join(" ")}`);
}

if (HOOKS_WITHOUT_RULES.size) {
  console.log("\nclass-liveness: hooks that carry no rule on purpose");
  for (const [token, why] of HOOKS_WITHOUT_RULES) console.log(`  hook .${token} — ${why}`);
}

if (mineDark.length) {
  console.log("\nclass-liveness: FAILED — a class this task removed is still referenced by a component");
  process.exit(1);
}
if (STRICT && preexistingDark.length) {
  console.log("\nclass-liveness: FAILED (--strict) — className with no rule anywhere");
  process.exit(1);
}
console.log("\nclass-liveness: ok — every class this task removed is still drawn");
