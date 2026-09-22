/**
 * Unit tests for the i18n mechanism (T1105, docs/28 §3).
 *
 * These run in CI's `web` job through scripts/web-unit-tests.sh, which globs
 * apps/web/lib/*.test.mjs — that is why lib/i18n.ts has no imports (an import
 * would take it out of Node's type-stripping reach and out of the gate).
 *
 * What is pinned here is everything a browser suite is bad at: the exhaustive
 * normalization table, the missing-key policy, the interpolation rules, the
 * catalog parity invariant, and the date format's independence from BOTH the
 * locale and the time zone. The browser suite (tests/web-smoke/i18n-smoke.mjs)
 * covers what this cannot: that the pages actually use the mechanism.
 */
import test from "node:test";
import assert from "node:assert/strict";

import {
  DEFAULT_LOCALE,
  LOCALES,
  LOCALE_COOKIE,
  MESSAGES,
  MISSING_KEY_PREFIX,
  formatCalendarDate,
  isLocale,
  isMissingKeyText,
  normalizeLocale,
  translate,
} from "./i18n.ts";

test("the supported set is exactly English + Simplified Chinese (docs/28 §3)", () => {
  assert.deepEqual([...LOCALES], ["en", "zh-CN"]);
  assert.equal(DEFAULT_LOCALE, "en");
});

test("the preference cookie name is the one the browser suite sets", () => {
  // tests/web-smoke/i18n-smoke.mjs restates this string rather than importing
  // it (it runs in Playwright's node, not in the app bundle). If the cookie is
  // ever renamed, that file has to be updated with it — this is the line that
  // says which value it must be updated TO.
  assert.equal(LOCALE_COOKIE, "post_locale");
});

test("isLocale accepts only the supported tags", () => {
  for (const good of ["en", "zh-CN"]) assert.equal(isLocale(good), true);
  for (const bad of ["EN", "zh", "zh-cn", "fr", "", null, undefined, 0]) {
    assert.equal(isLocale(bad), false);
  }
});

test("normalizeLocale maps browser-shaped tags and falls back rather than throwing", () => {
  const table = [
    ["en", "en"],
    ["EN", "en"],
    ["en-GB", "en"],
    ["en-US", "en"],
    ["zh", "zh-CN"],
    ["zh-CN", "zh-CN"],
    ["zh-cn", "zh-CN"],
    ["zh-Hans", "zh-CN"],
    ["  zh-CN  ", "zh-CN"],
    // Anything unrecognized is the default. A cookie is user-writable, and a
    // layout that threw on `post_locale=xx` would turn a typo into a 500 on
    // every route.
    ["fr", "en"],
    ["xx", "en"],
    ["", "en"],
    ["; DROP TABLE", "en"],
    [null, "en"],
    [undefined, "en"],
  ];
  for (const [input, expected] of table) {
    assert.equal(normalizeLocale(input), expected, `normalizeLocale(${JSON.stringify(input)})`);
  }
});

test("the catalog has the same keys in both locales", () => {
  const en = Object.keys(MESSAGES.en).sort();
  const zh = Object.keys(MESSAGES["zh-CN"]).sort();
  // The failure message names the offending keys: "the key sets differ" is a
  // diagnosis the reader cannot act on.
  assert.deepEqual(
    zh.filter((k) => !en.includes(k)),
    [],
    "keys present in zh-CN but not en",
  );
  assert.deepEqual(
    en.filter((k) => !zh.includes(k)),
    [],
    "keys present in en but not zh-CN",
  );
  assert.ok(en.length > 0, "the catalog is empty — every t() would render as a miss");
});

test("no locale has an empty or placeholder value", () => {
  for (const locale of LOCALES) {
    for (const [key, value] of Object.entries(MESSAGES[locale])) {
      assert.equal(typeof value, "string", `${locale}:${key} is not a string`);
      assert.ok(value.trim().length > 0, `${locale}:${key} is blank`);
      // A value that still contains the missing-key marker would make the
      // marker useless as a signal — no catalog value contains ⟦.
      assert.equal(value.includes("⟦"), false, `${locale}:${key} contains the miss marker`);
    }
  }
});

test("every catalog key is looked up by the shape of key the translator accepts", () => {
  for (const key of Object.keys(MESSAGES.en)) {
    assert.match(key, /^[a-z][A-Za-z0-9-]*(\.[A-Za-z0-9-]+)+$/, `key shape: ${key}`);
  }
});

test("translate returns the catalog value and interpolates {name}", () => {
  assert.equal(translate("en", "nav.language"), MESSAGES.en["nav.language"]);
  assert.equal(translate("zh-CN", "nav.language"), MESSAGES["zh-CN"]["nav.language"]);
  assert.equal(translate("en", "nav.signedInAs", { name: "ada" }), "Signed in as ada");
  assert.equal(translate("zh-CN", "nav.signedInAs", { name: "ada" }), "已登录：ada");
  assert.equal(translate("en", "nav.accountMenuLabel", { handle: "ada" }), "Account menu for ada");
});

test("interpolation numbers are stringified and unknown placeholders are left alone", () => {
  assert.equal(translate("en", "project.overview.branchCount", { count: 3 }), "3 branches");
  // A placeholder with no matching variable stays as written rather than
  // becoming "undefined": the sentence is still readable, and the miss is
  // visible in the output instead of hidden.
  assert.equal(translate("en", "project.overview.branchCount", {}), "{count} branches");
});

test("a missing key renders the key, visibly, and never the English fallback", () => {
  const out = translate("zh-CN", "no.such.key");
  assert.equal(out, `${MISSING_KEY_PREFIX}no.such.key⟧`);
  assert.equal(isMissingKeyText(out), true);
  // The policy's whole point: a zh-CN miss must NOT silently become the
  // English string, because that is the failure nobody can see.
  assert.equal(out.includes("⟦"), true);
  assert.equal(isMissingKeyText(translate("en", "nav.language")), false);
});

test("the missing-key marker cannot appear in any catalog value", () => {
  // Stated over the whole catalog rather than one sample, because the marker
  // is what the browser suite (and a reader looking at a screenshot) uses to
  // tell "untranslated" apart from "deliberate".
  for (const locale of LOCALES) {
    for (const [key, value] of Object.entries(MESSAGES[locale])) {
      assert.equal(isMissingKeyText(value), false, `${locale}:${key}`);
    }
  }
});

test("formatCalendarDate is a fixed YYYY-MM-DD over the UTC calendar fields", () => {
  assert.equal(formatCalendarDate("2026-09-22T12:00:00Z"), "2026-09-22");
  // Both ends of the same UTC day, and the instants that are a different day
  // in UTC+14 / UTC-11 but the SAME day here.
  assert.equal(formatCalendarDate("2026-09-22T00:00:00Z"), "2026-09-22");
  assert.equal(formatCalendarDate("2026-09-22T23:59:59Z"), "2026-09-22");
  assert.equal(formatCalendarDate("2026-01-05T00:00:00Z"), "2026-01-05");
  assert.equal(formatCalendarDate(new Date(Date.UTC(2026, 11, 31))), "2026-12-31");
  assert.equal(formatCalendarDate(0), "1970-01-01");
});

test("formatCalendarDate does not read the host locale or the host time zone", () => {
  // THE BUG THIS REPLACES. `new Date(x).toLocaleDateString()` with no
  // arguments answered differently per host: 9/22/2026 on a US-configured
  // machine, 22.09.2026 on a German one, and the day itself moved with the
  // host's time zone. The two assertions below are the two halves of that:
  //   - no host-dependent digits or separators can appear, and
  //   - the SAME instant yields the SAME string whatever the process's zone
  //     is set to.
  const value = "2026-09-22T23:30:00Z";
  const formatted = formatCalendarDate(value);
  assert.equal(formatted, "2026-09-22");
  assert.match(formatted, /^\d{4}-\d{2}-\d{2}$/);
  // 23:30Z is the 23rd in UTC+2 and the 22nd in UTC — the explicit format
  // must not follow either of them.
  assert.notEqual(formatted, "22.09.2026");
  assert.notEqual(formatted, "9/22/2026");
});

test("formatCalendarDate returns an empty string for an unparseable value", () => {
  assert.equal(formatCalendarDate("not a date"), "");
});

test("scientific quantities and units are byte-identical across locales", () => {
  // docs/28 §4. The task's acceptance criterion is that a value and its unit
  // survive translation unchanged; this pins the three the UI actually
  // renders. A localization that "helpfully" rewrote a unit, or that spaced
  // it differently, reds here rather than in a screenshot comparison someone
  // has to eyeball.
  const pairs = [
    ["project.overview.kicker", ["298 K"]],
    ["project.overview.decision", ["40% RH"]],
    ["search.introExample", ["298 K", "3 mmol/g"]],
  ];
  for (const [key, tokens] of pairs) {
    for (const token of tokens) {
      assert.ok(
        MESSAGES.en[key].includes(token),
        `en:${key} lost ${token}: ${JSON.stringify(MESSAGES.en[key])}`,
      );
      assert.ok(
        MESSAGES["zh-CN"][key].includes(token),
        `zh-CN:${key} lost ${token}: ${JSON.stringify(MESSAGES["zh-CN"][key])}`,
      );
    }
  }
});

test("labels change with the locale while domain codes do not", () => {
  // docs/28 §3: "领域对象 ID/enum 使用英文稳定 code，显示 label 本地化".
  // `frozen main` is the case in this catalog where the LABEL is the code —
  // it names the branch CLAUDE.md §9 invariant 3 protects, and no gloss of it
  // is more accurate than the name itself. The English keeps the string this
  // app already shipped ("Frozen main", sentence-cased like the other three
  // badges); the Chinese keeps the branch's own name verbatim, because the
  // reader who has to type or match `main` should see `main`.
  assert.equal(MESSAGES.en["projects.badge.frozenMain"], "Frozen main");
  assert.equal(MESSAGES["zh-CN"]["projects.badge.frozenMain"], "frozen main");
  // The enum-shaped codes the UI passes through (`main`, `en`, `zh-CN`, the
  // `data-*` anchors) are not catalog values at all, which is the other half
  // of the same rule: they never reach the translator.
  assert.equal(Object.keys(MESSAGES.en).some((k) => k.includes("data-")), false);
  assert.equal(MESSAGES.en["project.space.filesStatus"], "main");
});
