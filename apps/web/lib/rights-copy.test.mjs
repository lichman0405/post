/**
 * The copy boundary on every rights surface (T0703).
 *
 * docs/38 §1-3: POST records rights declarations, it does not judge them —
 * and the platform's own surface must not read as an assurance about what
 * the law allows. A page that displayed a license id next to "guaranteed" or
 * "legally binding" would turn a record of what a publisher declared into
 * this platform's statement, which is exactly what the acceptance criteria
 * for this task forbid (不声称法律保证).
 *
 * A rule that lives only in a reviewer's head is not a rule, so this scans
 * the web sources for wording that would cross the boundary and fails on it.
 * It scans source text rather than rendered HTML because the rights surfaces
 * have no data source yet — the publish path is T0705 — and a scan of the
 * whole apps/web tree is the version of this check that keeps working once
 * they do.
 *
 * Two ways this could pass while testing nothing, both closed below:
 * deleting the files it scans (the scan asserts the rights files exist and
 * the set is not empty), and the phrases living here as data (this file
 * excludes itself, and only itself).
 *
 * Run: node --test "apps/web/lib/rights-copy.test.mjs"
 */
import assert from "node:assert/strict";
import { readFileSync, readdirSync, statSync } from "node:fs";
import { join, relative, sep } from "node:path";
import { fileURLToPath } from "node:url";
import { test } from "node:test";

const WEB_ROOT = fileURLToPath(new URL("../", import.meta.url));
const SELF = "lib/rights-copy.test.mjs";

/**
 * The only file excused from the scan: this one, which has to spell the
 * phrases out to refuse them. The list is a constant of its own so the
 * excuse cannot quietly grow.
 */
const EXCLUDED_FROM_SCAN = new Set([SELF]);

/**
 * The files that render a rights declaration. Each must exist and be
 * scanned: a guard whose subjects are gone reports success over nothing.
 */
const RIGHTS_SURFACES = ["lib/rights.ts", "app/components/rights-panel.tsx", "app/globals.css"];

/** Source extensions worth scanning; anything else (json, images) is not copy. */
const SCANNED_EXTENSIONS = [".ts", ".tsx", ".mjs", ".css"];

/** Build output and installed packages are not our copy. */
const SKIPPED_DIRECTORIES = new Set(["node_modules", ".next", "out", "build"]);

/**
 * The phrasing that would claim more than a record of a declaration. Each
 * pattern names the claim it refuses.
 *
 * The list is lexical and unconditional: a match is refused wherever the
 * words appear, a disclaimer included. Two of these rules are therefore
 * rules a disclaimer has to write around — the "guarantee" rule refuses
 * "POST does not guarantee that any declared license is valid." and the
 * "warranty" rule refuses "No warranty is provided about the declaration.",
 * and neither sentence may be used on a rights surface. That is what this
 * list is, and the fix for a finding is to reword the sentence; the list is
 * never loosened to admit one. The test "the words a disclaimer would reach
 * for are refused by design" pins both those refusals and the rewrites that
 * pass, so the constraint is visible here rather than discovered as a
 * surprise.
 *
 * Why the scan does not try to tell an affirmative claim from a negated
 * one: it reads one line at a time and knows no grammar, so a negation test
 * would have to be a window around the match — and "POST does not verify
 * licenses, and it guarantees that the data is open." contains both, is
 * refused today, and would buy its claim a free pass under any such window.
 * The two errors are not symmetric: a false positive costs an author one
 * reword, a false negative puts this platform's name behind a legal
 * assurance. So the scan errs toward the reword.
 *
 * The boundary sentences this repository does require are written inside
 * the rule — "does not give legal advice" is asserted elsewhere in this
 * file and matches none of the patterns below.
 */
const FORBIDDEN = [
  { pattern: /\bguarantee(s|d)?\b/i, claim: "a guarantee about a use, an outcome or the data" },
  { pattern: /\bwarrant(y|ies)\b/i, claim: "a warranty" },
  { pattern: /\bwarrants?\b/i, claim: "a warranty" },
  {
    pattern: /\blegally (binding|valid|safe|permitted|allowed|enforceable)\b/i,
    claim: "a statement about the legal effect of a declaration",
  },
  { pattern: /\benforceab(le|ility)\b/i, claim: "a statement about enforceability" },
  { pattern: /\bcertif(y|ies|ied|ication)\b/i, claim: "a certification by this platform" },
  {
    pattern: /\b(ensures?|making|makes) (legal )?compliance\b/i,
    claim: "a compliance claim",
  },
  { pattern: /\bcomplies with (the )?law\b/i, claim: "a compliance claim" },
  { pattern: /\bsafe to (use|share|publish|redistribute)\b/i, claim: "a safety assurance" },
  {
    pattern: /\byou (may|can) (legally )?(use|copy|redistribute|reuse|publish)\b/i,
    claim: "permission advice",
  },
  {
    pattern: /\b(this|the) (license|agreement|declaration) (grants|permits|allows) (you|anyone)\b/i,
    claim: "interpretation of the declaration on the platform's behalf",
  },
  { pattern: /\bwe (verify|have verified|check|guarantee)\b/i, claim: "the platform vouching for a declaration" },
  {
    // Chinese copy, for the case where the criterion's wording reaches the
    // source: the patterns are the affirmative claims only. 法律保证 on its
    // own is not one — 不声称法律保证 ("makes no legal-guarantee claim") is
    // the acceptance criterion's own sentence, and a guard that refused it
    // would refuse the comment that states the rule.
    pattern: /保证(合法|法律|合规|不侵权)|合法性保证|确保合规|法律上(有效|安全|允许)/,
    claim: "a legal-guarantee claim in Chinese copy",
  },
];

/** Every scannable file under apps/web, as paths relative to apps/web. */
function scanTargets() {
  const found = [];
  const walk = (dir) => {
    for (const entry of readdirSync(dir, { withFileTypes: true })) {
      if (entry.isDirectory()) {
        if (SKIPPED_DIRECTORIES.has(entry.name)) continue;
        walk(join(dir, entry.name));
        continue;
      }
      if (!SCANNED_EXTENSIONS.some((ext) => entry.name.endsWith(ext))) continue;
      found.push(relative(WEB_ROOT, join(dir, entry.name)).split(sep).join("/"));
    }
  };
  walk(WEB_ROOT);
  return found.sort();
}

function read(relativePath) {
  return readFileSync(join(WEB_ROOT, relativePath), "utf8");
}

test("the scan covers the rights surfaces and is not empty", () => {
  const targets = scanTargets();
  // A glob that walks nothing reports "no violations" over nothing at all.
  assert.ok(targets.length >= 10, `expected a populated scan, found ${targets.length} file(s)`);
  for (const surface of RIGHTS_SURFACES) {
    assert.ok(targets.includes(surface), `${surface} is not in the scan set`);
    assert.ok(read(surface).trim().length > 0, `${surface} is empty`);
  }
  assert.ok(
    targets.includes(SELF),
    "the walk must reach every scannable file, this one included; the excuse is applied by name below",
  );
  assert.deepEqual(
    [...EXCLUDED_FROM_SCAN],
    [SELF],
    "this file is the only one allowed to spell the forbidden phrases",
  );
});

test("no web source claims a legal guarantee", () => {
  const violations = [];
  for (const target of scanTargets()) {
    if (EXCLUDED_FROM_SCAN.has(target)) continue;
    const lines = read(target).split("\n");
    lines.forEach((line, index) => {
      for (const { pattern, claim } of FORBIDDEN) {
        if (pattern.test(line)) {
          violations.push(`${target}:${index + 1}: ${claim}\n    ${line.trim()}`);
        }
      }
    });
  }
  assert.deepEqual(
    violations,
    [],
    `copy that claims more than a record of a declaration:\n  ${violations.join("\n  ")}`,
  );
});

test("the boundary sentence is present wherever a rights panel renders", () => {
  // The scan above refuses added claims; this refuses a silent deletion of
  // the sentence that states the boundary. Both directions matter: a panel
  // that shows a license id with no notice is the failure mode the
  // acceptance criteria name, and it contains no forbidden word.
  const panel = read("app/components/rights-panel.tsx");
  assert.match(panel, /RIGHTS_LEGAL_NOTICE/, "the rights panel must render the legal notice");
  assert.match(panel, /RIGHTS_ACCESS_NOTICE/, "the rights panel must render the access notice");

  const rightsModule = read("lib/rights.ts");
  assert.match(
    rightsModule,
    /does not give legal advice/,
    "the notice must say the platform gives no legal advice",
  );
});

test("the scan itself can see a violation", () => {
  // The patterns are asserted against text they must match, so a pattern
  // that had drifted into matching nothing (a typo, an over-escaped dot)
  // fails here instead of reporting a clean tree forever.
  const samples = [
    "This license is guaranteed to permit redistribution.",
    "These terms are legally binding.",
    "POST certifies that this dataset is open.",
    "This ensures compliance with your obligations.",
    "The declaration grants you the right to redistribute.",
    "本平台保证合法性。",
  ];
  for (const sample of samples) {
    assert.ok(
      FORBIDDEN.some(({ pattern }) => pattern.test(sample)),
      `no pattern refuses: ${sample}`,
    );
  }
});

test("the scan leaves the honest sentences alone", () => {
  // The other direction: a boundary sentence must not be refused by its own
  // guard, or the fix for a finding would be to delete the disclaimer.
  const honest = [
    "POST does not give legal advice.",
    "This is the publisher's own declaration, recorded and shown as it was made.",
    "Metadata visibility and data access are separate.",
    "The id is shown exactly as the publisher declared it.",
    "This declaration does not state: Derivatives, Data access.",
    "Restricted",
    "See the agreement",
    "不声称法律保证",
  ];
  for (const line of honest) {
    for (const { pattern, claim } of FORBIDDEN) {
      assert.ok(!pattern.test(line), `"${line}" was refused as ${claim}`);
    }
  }
});

test("every rule refuses its own affirmative sample", () => {
  // The comment on FORBIDDEN says a match is refused wherever it appears,
  // so each rule has to still bite. One sample per rule, attributed to the
  // rule by its pattern source, so a rule that had drifted into matching
  // nothing fails here rather than reporting a clean tree forever.
  const AFFIRMATIVE = [
    { source: "\\bguarantee(s|d)?\\b", sample: "This license is guaranteed to permit redistribution." },
    { source: "\\bwarrant(y|ies)\\b", sample: "This data comes with a warranty of fitness for research use." },
    { source: "\\bwarrants?\\b", sample: "POST warrants that this declaration is accurate." },
    { source: "\\blegally (binding|valid|safe|permitted|allowed|enforceable)\\b", sample: "These terms are legally binding." },
    { source: "\\benforceab(le|ility)\\b", sample: "The declaration is enforceable against you." },
    { source: "\\bcertif(y|ies|ied|ication)\\b", sample: "POST certifies that this dataset is open." },
    { source: "\\b(ensures?|making|makes) (legal )?compliance\\b", sample: "This ensures compliance with your obligations." },
    { source: "\\bcomplies with (the )?law\\b", sample: "The declaration complies with the law." },
    { source: "\\bsafe to (use|share|publish|redistribute)\\b", sample: "This dataset is safe to publish." },
    { source: "\\byou (may|can) (legally )?(use|copy|redistribute|reuse|publish)\\b", sample: "You may redistribute this dataset." },
    { source: "\\b(this|the) (license|agreement|declaration) (grants|permits|allows) (you|anyone)\\b", sample: "The declaration grants you the right to redistribute." },
    { source: "\\bwe (verify|have verified|check|guarantee)\\b", sample: "We verify every declaration before it is shown." },
    { source: "保证(合法|法律|合规|不侵权)|合法性保证|确保合规|法律上(有效|安全|允许)", sample: "本平台保证合法性。" },
  ];
  for (const { source, sample } of AFFIRMATIVE) {
    const rule = FORBIDDEN.find(({ pattern }) => pattern.source === source);
    assert.ok(rule, `the sample table names a rule that FORBIDDEN does not have: ${source}`);
    assert.match(sample, new RegExp(source, "i"), `no match, so nothing is refused: ${sample}`);
  }
  assert.deepEqual(
    FORBIDDEN.map(({ pattern }) => pattern.source),
    AFFIRMATIVE.map(({ source }) => source),
    "every rule needs an affirmative sample, and the table must not carry rules that do not exist",
  );
});

test("the words a disclaimer would reach for are refused by design", () => {
  // The constraint the FORBIDDEN comment states, as an example a reader can
  // run: both of these are honest disclaimers, both are refused, and the
  // rewrites beside them are what an author writes instead. A future edit
  // that narrowed the guarantee or warranty rule to make the first pair
  // pass would fail here.
  const refused = [
    {
      sentence: "POST does not guarantee that any declared license is valid.",
      refusedAs: "a guarantee about a use, an outcome or the data",
    },
    { sentence: "No warranty is provided about the declaration.", refusedAs: "a warranty" },
  ];
  for (const { sentence, refusedAs } of refused) {
    const matched = FORBIDDEN.filter(({ pattern }) => pattern.test(sentence));
    assert.ok(
      matched.some(({ claim }) => claim === refusedAs),
      `"${sentence}" must be refused as ${refusedAs}; it was refused as ${matched.map(({ claim }) => claim).join(", ") || "nothing"}`,
    );
  }

  for (const sentence of [
    "POST does not assure you that any declared license is valid.",
    "No assurance about the declaration is given here.",
  ]) {
    for (const { pattern, claim } of FORBIDDEN) {
      assert.ok(!pattern.test(sentence), `the rewrite "${sentence}" was refused as ${claim}`);
    }
  }

  // Why the rule is lexical rather than negation-aware: this sentence makes
  // a claim and carries a negation, and no window around the match could
  // tell it from the disclaimers above.
  assert.ok(
    FORBIDDEN[0].pattern.test("POST does not verify licenses, and it guarantees that the data is open."),
    "a claim that rides along with a negation must still be refused",
  );
});

test("a file that cannot be read fails the scan", () => {
  // The walk must not skip a target it cannot open, or an unreadable file
  // would read as a clean one.
  assert.throws(() => read("lib/no-such-module.ts"), /ENOENT/);
  assert.ok(statSync(join(WEB_ROOT, "lib", "rights.ts")).isFile());
});
