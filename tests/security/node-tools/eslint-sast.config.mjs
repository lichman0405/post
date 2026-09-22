// The SAST rule surface for the Node/TypeScript face of the Master
// Security/Quality Gate (tests/security/sast.sh node).
//
// WHAT THIS FILE IS
// -----------------
// One flat ESLint config, the whole of it, applied with
// `--no-config-lookup --config <this file>`: the app's own
// apps/web/eslint.config.mjs (Next.js correctness rules) is NOT in the chain
// here, and this file's rules are NOT in the chain there. "pnpm lint" answers
// "is this code correct"; this answers "does a security rule fire on it", and
// the two must not be able to hide each other's findings.
//
// WHAT IT DELIBERATELY DOES NOT DO
// --------------------------------
//   * no rule is disabled, no rule's severity is lowered, and no directory is
//     excluded from the rule set: eslint-plugin-security's `recommended` set
//     is taken whole and every one of its rules is forced to "error". The
//     plugin ships most of them as warnings, and a warning is a green gate —
//     the exact failure mode this gate exists to refuse. A finding nobody has
//     triaged must be red; a finding that HAS been triaged goes to
//     ops/ci/eslint-security-baseline.txt, one line per finding, with the
//     verdict written beside it.
//   * no parser-less run. The workspace is TypeScript, and ESLint without a
//     TS parser does not lint a .ts file — it fails to parse it. That failure
//     used to be invisible here: an earlier revision of this row scored 103
//     fatal parse errors as "messages" and 22 real security findings, and the
//     config below is the fix. If the parser ever goes missing, the parse
//     errors come back, and the row counts them as findings and goes red.
//
// The two guards at the bottom are the file's own honesty check: a config that
// resolved to an empty rule set would lint every file and report nothing.
import security from "eslint-plugin-security";
import tseslint from "typescript-eslint";

const recommended = security.configs?.recommended?.rules ?? {};

// A config that lost its rules is worse than no config: it would report a
// clean tree forever. Refuse to run at all rather than run empty.
const RULE_FLOOR = 10;
const ruleCount = Object.keys(recommended).length;
if (ruleCount < RULE_FLOOR) {
  throw new Error(
    `eslint-sast.config: eslint-plugin-security resolved ${ruleCount} rule(s), the floor is ${RULE_FLOOR}. ` +
      "Refusing to lint with a rule set this small — install the pinned plugin " +
      "(`make security-tools`) or fix the import; do not lower this floor.",
  );
}

// Every recommended rule at "error". The spread keeps the plugin's own rule
// list authoritative (a plugin upgrade adds its new rules automatically);
// the map is what makes them block.
const rules = Object.fromEntries(Object.keys(recommended).map((name) => [name, "error"]));

export default [
  {
    // Build output is not source. This is not a rule exclusion: nothing here
    // is hand-written code, and `pnpm lint` (apps/web/eslint.config.mjs) draws
    // the same line.
    ignores: ["**/node_modules/**", "**/.next/**", "**/out/**", "**/dist/**", "**/coverage/**", "**/*.d.ts"],
  },
  {
    files: ["**/*.{js,mjs,cjs,jsx,ts,tsx}"],
    plugins: { security },
    languageOptions: {
      parser: tseslint.parser,
      ecmaVersion: "latest",
      sourceType: "module",
      parserOptions: { ecmaFeatures: { jsx: true } },
    },
    rules,
  },
];

// Read by tests/security/sast.sh: the row prints the rule count it ran with as
// part of its evidence, so "0 unbaselined findings" is a statement about a
// rule set of a known size rather than about whatever happened to load.
export const sastRuleCount = ruleCount;
export const sastRuleSet = "eslint-plugin-security/recommended";
