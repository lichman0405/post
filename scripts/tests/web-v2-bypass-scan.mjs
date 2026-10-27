/**
 * Static anti-bypass scan for the WEB-01 real-UI harness (docs/70 §6.2-6.4).
 *
 * The browser journey may only perform business steps through the page's
 * normal, visible controls. This scan is the cheap, deterministic half of
 * that rule: it reads every journey and helper module and refuses
 *
 *   - test-side HTTP clients (page.request, context.request, any
 *     APIRequestContext, request.newContext, fetch, XMLHttpRequest, axios) —
 *     these would let the test do what the user is supposed to do;
 *   - storage/cookie injection (addCookies, addInitScript, storageState,
 *     localStorage/sessionStorage, document.cookie);
 *   - request interception that could fake a response (page/context.route,
 *     route.fulfill, setContent);
 *   - SQL/CLI access used to create or mutate business state;
 *   - in-page script execution in every Playwright spelling: page.evaluate,
 *     locator.evaluate/evaluateAll, page.$eval/$$eval, evaluateHandle. This
 *     is where a hidden fetch would go.
 *
 * The static scan is auxiliary, not the proof: docs/70 §6.4 also requires
 * runtime evidence (trace, real services, step results), which journey.mjs
 * records. The scan exists so a bypass cannot be introduced quietly by a
 * helper that later journeys import.
 *
 * Comments are stripped first: prose in a module may NAME the forbidden
 * APIs while documenting that they are forbidden. A bypass in an actual
 * expression is still found.
 *
 * Usage: node scripts/tests/web-v2-bypass-scan.mjs [directory]
 * Exit:  0 clean, 1 findings (or no files scanned — an empty scan is not a
 *        clean scan).
 */

import { readFile, readdir, stat } from "node:fs/promises";
import path from "node:path";
import process from "node:process";

const TARGET = process.argv[2] ?? path.join(import.meta.dirname, "..", "..", "tests", "web-v2");

const SKIP_DIRS = new Set(["node_modules", "evidence", ".git", "test-results", "playwright-report"]);

const PATTERNS = [
  { label: "test-side HTTP request (page.request)", re: /\bpage\s*\.\s*request\b/ },
  { label: "test-side HTTP request (context.request)", re: /\b(?:context|browserContext|ctx|browser)\s*\.\s*request\b/ },
  { label: "test-side HTTP client (APIRequestContext)", re: /\bAPIRequestContext\b/ },
  { label: "test-side HTTP client (request.newContext)", re: /\brequest\s*\.\s*newContext\b/ },
  { label: "test-side fetch", re: /\bfetch\s*\(/ },
  { label: "test-side XMLHttpRequest", re: /\bXMLHttpRequest\b/ },
  { label: "test-side axios", re: /\baxios\b/ },
  { label: "cookie injection (addCookies)", re: /\baddCookies\b/ },
  { label: "script injection (addInitScript)", re: /\baddInitScript\b/ },
  { label: "session replay (storageState)", re: /\bstorageState\b/ },
  { label: "browser storage access (localStorage)", re: /\blocalStorage\b/ },
  { label: "browser storage access (sessionStorage)", re: /\bsessionStorage\b/ },
  { label: "browser storage access (indexedDB)", re: /\bindexedDB\b/ },
  { label: "cookie access (document.cookie)", re: /\bdocument\s*\.\s*cookie\b/ },
  // Every Playwright spelling of "run this function inside the page":
  // page.evaluate, locator.evaluate, locator.evaluateAll and evaluateHandle.
  { label: "in-page script execution (evaluate/evaluateAll)", re: /\.\s*evaluate(?:All)?\s*\(/ },
  { label: "in-page script execution (page.$eval/$$eval)", re: /\b(?:page|frame)\s*\.\s*\$\$?eval\b/ },
  { label: "in-page script execution (evaluateHandle)", re: /\bevaluateHandle\s*\(/ },
  { label: "request interception (page.route/context.route)", re: /\b(?:page|context)\s*\.\s*route\b/ },
  { label: "response faking (route.fulfill)", re: /\broute\s*\.\s*fulfill\b/ },
  { label: "document injection (setContent)", re: /\bsetContent\b/ },
  { label: "CLI execution (child_process)", re: /\bchild_process\b/ },
  { label: "CLI execution (execSync/spawnSync/execFileSync/execa)", re: /\b(?:execSync|spawnSync|execFileSync|execa)\b/ },
  { label: "SQL client (psql)", re: /\bpsql\b/ },
  { label: "database DSN (postgres:// or redis://)", re: /\b(?:postgres|redis):\/\// },
  { label: "SQL driver import (pg)", re: /(?:from\s+["']pg["']|require\(\s*["']pg["']\s*\))/ },
  {
    label: "filesystem access outside the evidence helper",
    re: /(?:from\s+["']node:fs(?:\/promises)?["']|from\s+["']fs(?:\/promises)?["']|require\(\s*["'](?:node:)?fs(?:\/promises)?["']\s*\))/,
    allow: new Set(["lib/evidence.mjs"]),
  },
];

/** Blank out comments while preserving byte offsets and line numbers. */
export function stripComments(source) {
  return source
    .replace(/\/\*[\s\S]*?\*\//g, (match) => match.replace(/[^\n]/g, " "))
    .replace(/(^|[^:])\/\/[^\n]*/gm, (match, prefix) => prefix + " ".repeat(match.length - prefix.length));
}

function lineOf(source, index) {
  let line = 1;
  for (let i = 0; i < index && i < source.length; i += 1) {
    if (source[i] === "\n") line += 1;
  }
  return line;
}

async function collect(dir, base = dir, out = []) {
  let entries;
  try {
    entries = await readdir(dir, { withFileTypes: true });
  } catch (err) {
    throw new Error(`cannot read ${dir}: ${err.message}`);
  }
  for (const entry of entries.sort((a, b) => a.name.localeCompare(b.name))) {
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) {
      if (!SKIP_DIRS.has(entry.name)) {
        await collect(full, base, out);
      }
      continue;
    }
    if (!entry.isFile() || !entry.name.endsWith(".mjs")) continue;
    if (entry.name.endsWith(".test.mjs")) continue; // unit tests of the helpers
    out.push(path.relative(base, full));
  }
  return out;
}

async function main() {
  const info = await stat(TARGET).catch(() => null);
  if (info === null || !info.isDirectory()) {
    console.error(`web-v2 bypass scan: FAILED — ${TARGET} is not a directory`);
    process.exit(1);
  }

  const files = await collect(TARGET);
  if (files.length === 0) {
    console.error(`web-v2 bypass scan: FAILED — no .mjs journey/helper modules under ${TARGET}; an empty scan is not a clean scan`);
    process.exit(1);
  }

  const findings = [];
  for (const rel of files) {
    const raw = await readFile(path.join(TARGET, rel), "utf8");
    const source = stripComments(raw);
    for (const pattern of PATTERNS) {
      if (pattern.allow?.has(rel)) continue;
      const re = new RegExp(pattern.re.source, pattern.re.flags.includes("g") ? pattern.re.flags : pattern.re.flags + "g");
      for (let match = re.exec(source); match !== null; match = re.exec(source)) {
        findings.push({ file: rel, line: lineOf(source, match.index), label: pattern.label, text: match[0] });
      }
    }
  }

  if (findings.length > 0) {
    console.error(`web-v2 bypass scan: FAILED — ${findings.length} forbidden bypass pattern(s):`);
    for (const finding of findings) {
      console.error(`  BYPASS ${finding.file}:${finding.line}: ${finding.label} (${finding.text})`);
    }
    process.exit(1);
  }

  console.log(`web-v2 bypass scan: OK — scanned ${files.length} journey/helper module(s): ${files.join(", ")}`);
}

main().catch((err) => {
  console.error(`web-v2 bypass scan: FAILED — ${err instanceof Error ? err.message : String(err)}`);
  process.exit(1);
});
