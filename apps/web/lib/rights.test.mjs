/**
 * Tests for the rights presentation module (apps/web/lib/rights.ts).
 *
 * Plain-ESM test file like milestones.test.mjs: Node ≥ 23.6 runs the
 * imported .ts through type stripping (rights.ts is import-free by
 * construction); the module is fully typechecked by
 * `pnpm --filter @post/web typecheck`.
 *
 * What is pinned here, in the order the module's doc comment states it:
 * the reader invents no value (absent -> NOT_STATED, unknown -> verbatim),
 * it keeps the two access axes apart, and every usage axis the acceptance
 * criteria name is expressible and rendered. The Go half of the same
 * document — the vocabularies, the rules, the stored shape — is pinned by
 * internal/rights and tests/integration/rights_model_test.go; the copy
 * boundary is pinned by rights-copy.test.mjs.
 *
 * Run: node --test "apps/web/lib/rights.test.mjs"
 */
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";

import {
  NOT_STATED,
  RIGHTS_ACCESS_NOTICE,
  RIGHTS_LEGAL_NOTICE,
  RIGHTS_TEMPLATE_VERSION,
  USAGE_AXES,
  agreementLine,
  dataAccessLabel,
  fieldName,
  licenseLine,
  metadataLabel,
  parseRightsDocument,
  unstatedNotice,
  usageLabel,
  usageRows,
} from "./rights.ts";

/**
 * specs/policies/rights-template.yaml version 1 with its default values,
 * as JSON. It is the same document internal/rights models, and
 * `template matches the Go model` below reads the Go suite's own copy of it
 * so the two cannot drift.
 */
const TEMPLATE_DOCUMENT = {
  version: RIGHTS_TEMPLATE_VERSION,
  standard_license_id: null,
  custom_agreement_ref: null,
  usage: {
    commercial_use: "unspecified",
    derivatives: "unspecified",
    redistribution: "unspecified",
    model_training: "unspecified",
    attribution: "required",
    patent_grant: "none",
  },
  visibility: { metadata: "project_policy", data_access: "restricted" },
  notes: null,
};

/** A declaration that states every field, for the rendering tests. */
function fullDeclaration(overrides = {}) {
  return {
    version: 1,
    standard_license_id: "CC-BY-4.0",
    custom_agreement_ref: "agreements/mof-2026.pdf",
    usage: {
      commercial_use: "restricted",
      derivatives: "allowed",
      redistribution: "restricted",
      model_training: "restricted",
      attribution: "required",
      patent_grant: "see_agreement",
    },
    visibility: { metadata: "project_policy", data_access: "restricted" },
    notes: "Covers the 2026 release only.",
    ...overrides,
  };
}

/** Reads a declaration that must be readable, failing loudly when it is not. */
function readDeclaration(raw) {
  const read = parseRightsDocument(raw);
  assert.equal(read.kind, "declaration", `expected a declaration, got ${JSON.stringify(read)}`);
  return read;
}

/* ------------------------------------------------------------------ */
/* The template anchor                                                 */
/* ------------------------------------------------------------------ */

test("template matches the Go model", () => {
  // internal/rights/document_test.go holds the template as a Go string
  // constant so that the model, the stored column and the spec file cannot
  // drift. This reads that constant and compares it to the object above:
  // the web reader and the Go model must read the same document, and a
  // change to one that the other does not follow fails here.
  const source = readFileSync(
    new URL("../../../internal/rights/document_test.go", import.meta.url),
    "utf8",
  );
  const block = source.match(/const templateDocument = ([\s\S]*?)\n\n/);
  assert.ok(block, "internal/rights/document_test.go no longer declares const templateDocument");
  const segments = [...block[1].matchAll(/`([^`]*)`/g)].map((m) => m[1]);
  assert.ok(segments.length > 0, "const templateDocument holds no string literal");

  assert.deepEqual(
    JSON.parse(segments.join("")),
    TEMPLATE_DOCUMENT,
    "the web reader's template document differs from internal/rights's",
  );
});

test("the template document reads as a declaration with every axis stated", () => {
  const read = readDeclaration(TEMPLATE_DOCUMENT);
  assert.deepEqual(read.unstated, []);
  assert.equal(licenseLine(read.document), "No standard license named");
  assert.equal(agreementLine(read.document), "No separate agreement referenced");
  assert.equal(read.document.notes, null);
});

/* ------------------------------------------------------------------ */
/* Reading: what is not a declaration, and why                         */
/* ------------------------------------------------------------------ */

test("a value that is not a declaration comes back with the reason", () => {
  const cases = [
    { name: "null", raw: null, expect: /No rights declaration is recorded/ },
    { name: "undefined", raw: undefined, expect: /No rights declaration is recorded/ },
    { name: "empty object", raw: {}, expect: /names no template version/ },
    { name: "array", raw: [], expect: /not a declaration \(it is a list\)/ },
    { name: "JSON string", raw: '"MIT"', expect: /not a declaration \(it is a piece of text\)/ },
    { name: "number", raw: 7, expect: /not a declaration \(it is a number\)/ },
    { name: "boolean", raw: true, expect: /not a declaration \(it is a true\/false value\)/ },
    { name: "bare word", raw: "MIT", expect: /not JSON/ },
    { name: "text that is not JSON", raw: "{not json", expect: /not JSON/ },
  ];
  for (const tc of cases) {
    const read = parseRightsDocument(tc.raw);
    assert.equal(read.kind, "none", `${tc.name}: expected none`);
    assert.match(read.reason, tc.expect, `${tc.name}: reason`);
  }
});

test("a document of an unknown template version is refused, not guessed at", () => {
  // The load-bearing refusal: rendering a v2 document with v1's assumptions
  // would attribute a reading the document may not support.
  const read = parseRightsDocument({ version: 2, usage: { commercial_use: "allowed" } });
  assert.equal(read.kind, "none");
  assert.match(read.reason, /template version 2/);
  assert.match(read.reason, /reads version 1/);
});

test("a value the column may hold but this reader does not know is carried verbatim", () => {
  // Migration 00066 deliberately does not constrain the column to the
  // vocabulary, so the reader must survive a word it does not know rather
  // than fail or fall back to a known token. rights_model_test.go stores
  // exactly this document to pin the storage side.
  const read = readDeclaration({
    ...fullDeclaration(),
    usage: { ...fullDeclaration().usage, commercial_use: "banana" },
  });
  assert.equal(read.document.usage.commercial_use, "banana");
  assert.equal(read.unstated.includes("commercial_use"), false);
  assert.equal(usageLabel("commercial_use", "banana"), "banana (a value this view does not know)");
});

test("the reader never fills a missing field with a nearby value", () => {
  const read = readDeclaration({
    version: 1,
    usage: { commercial_use: "allowed" },
    visibility: { metadata: "project_policy" },
  });
  assert.equal(read.document.usage.commercial_use, "allowed");
  // attribution's template DEFAULT is "required"; the document states
  // nothing, so the reader must not hand back the default.
  assert.equal(read.document.usage.attribution, NOT_STATED);
  assert.equal(read.document.visibility.data_access, NOT_STATED);
  assert.deepEqual(read.unstated.sort(), [
    "attribution",
    "data_access",
    "derivatives",
    "model_training",
    "patent_grant",
    "redistribution",
  ]);
});

test("the stored text of a document is read like the document", () => {
  const text = JSON.stringify(fullDeclaration());
  const fromText = readDeclaration(text);
  const fromObject = readDeclaration(JSON.parse(text));
  assert.deepEqual(fromText.document, fromObject.document);
});

test("an unknown top-level field does not disturb the reading", () => {
  // DisallowUnknownFields is the Go model's rule; the web reader shows the
  // fields it knows and does not pretend the document has only those.
  const read = readDeclaration({ ...fullDeclaration(), embargo: "2027-01-01" });
  assert.equal(read.kind, "declaration");
  assert.equal(read.document.usage.derivatives, "allowed");
});

/* ------------------------------------------------------------------ */
/* Expressiveness: the five axes the task names                        */
/* ------------------------------------------------------------------ */

test("every usage axis the task names is expressible and rendered", () => {
  // 可表达 commercial/derivative/redistribution/model training/attribution.
  const rows = usageRows(fullDeclaration());
  assert.deepEqual(
    rows.map((r) => r.axis),
    ["commercial_use", "derivatives", "redistribution", "model_training", "attribution", "patent_grant"],
  );
  assert.deepEqual(
    rows.map((r) => r.heading),
    [
      "Commercial use",
      "Derivatives",
      "Redistribution",
      "Model training",
      "Attribution",
      "Patent grant",
    ],
  );
  // No row renders empty and none renders "undefined": every axis has a
  // label for every token it can hold.
  for (const row of rows) {
    assert.ok(row.rendered.length > 0, `${row.axis} rendered empty`);
    assert.doesNotMatch(row.rendered, /undefined|NaN/);
  }
  assert.deepEqual(
    rows.map((r) => r.rendered),
    ["Restricted", "Allowed", "Restricted", "Restricted", "Required", "See the agreement"],
  );
});

test("every token of every vocabulary has a label", () => {
  const vocabularies = {
    commercial_use: ["allowed", "restricted", "unspecified"],
    derivatives: ["allowed", "restricted", "unspecified"],
    redistribution: ["allowed", "restricted", "unspecified"],
    model_training: ["allowed", "restricted", "unspecified"],
    attribution: ["required", "not_required", "unspecified"],
    patent_grant: ["none", "see_agreement", "explicit"],
  };
  for (const [axis, tokens] of Object.entries(vocabularies)) {
    for (const token of tokens) {
      const rendered = usageLabel(axis, token);
      assert.ok(rendered.length > 0, `${axis}=${token} rendered empty`);
      assert.doesNotMatch(rendered, /does not know/, `${axis}=${token} is a known token`);
    }
  }
  assert.equal(dataAccessLabel("open"), "Open");
  assert.equal(dataAccessLabel("restricted"), "Restricted");
  assert.equal(metadataLabel("project_policy"), "Project policy");
  assert.equal(dataAccessLabel(NOT_STATED), "Not stated");
  assert.equal(metadataLabel(NOT_STATED), "Not stated");
});

test("the token list the reader walks is the template's six axes", () => {
  assert.deepEqual([...USAGE_AXES], [
    "commercial_use",
    "derivatives",
    "redistribution",
    "model_training",
    "attribution",
    "patent_grant",
  ]);
});

/* ------------------------------------------------------------------ */
/* The two access axes stay apart                                      */
/* ------------------------------------------------------------------ */

test("metadata visibility and data access are read separately", () => {
  // Invariant 7. A public metadata record with restricted bytes is a state
  // the model has to be able to express, and the reader must not collapse
  // the two into one value.
  const read = readDeclaration(
    fullDeclaration({ visibility: { metadata: "public", data_access: "restricted" } }),
  );
  assert.equal(metadataLabel(read.document.visibility.metadata), "public (a value this view does not know)");
  // An open token outside the template's set is shown verbatim rather than
  // mapped to project_policy — metadata visibility is an open vocabulary.
  assert.equal(read.document.visibility.data_access, "restricted");
  assert.equal(dataAccessLabel(read.document.visibility.data_access), "Restricted");
});

/* ------------------------------------------------------------------ */
/* References and the unstated line                                    */
/* ------------------------------------------------------------------ */

test("the license id is shown as declared, with no expansion", () => {
  const doc = readDeclaration(fullDeclaration()).document;
  assert.equal(licenseLine(doc), "CC-BY-4.0");
  // No link, no name, no terms: the id is the declared text and nothing
  // more. A name table would be a registry this project does not keep.
  assert.equal(licenseLine({ ...doc, standard_license_id: "NOT-A-REAL-LICENSE-1.0" }),
    "NOT-A-REAL-LICENSE-1.0");
  assert.equal(licenseLine({ ...doc, standard_license_id: null }), "No standard license named");
  assert.equal(agreementLine({ ...doc, custom_agreement_ref: null }), "No separate agreement referenced");
});

test("a partial declaration says which fields it does not state", () => {
  const read = readDeclaration({ version: 1, usage: { commercial_use: "allowed" } });
  const notice = unstatedNotice(read.unstated);
  assert.ok(notice, "a partial declaration must produce a line");
  assert.match(notice, /does not state/);
  assert.match(notice, /Derivatives/);
  assert.match(notice, /Data access/);
  assert.match(notice, /unstated rather than as permitted or refused/);
  assert.equal(unstatedNotice([]), null);
  assert.equal(fieldName("model_training"), "Model training");
  assert.equal(fieldName("metadata"), "Metadata visibility");
});

/* ------------------------------------------------------------------ */
/* The boundary copy                                                   */
/* ------------------------------------------------------------------ */

test("the boundary notices exist, are sentences, and name what POST does not do", () => {
  // 不声称法律保证. This asserts the notice says the platform does not
  // interpret the declaration and does not give legal advice. The phrasing
  // that would break it is refused by rights-copy.test.mjs across the whole
  // web tree, so this file never has to spell it out.
  assert.ok(RIGHTS_LEGAL_NOTICE.length > 80, "the legal notice must be a full sentence, not a token");
  assert.match(RIGHTS_LEGAL_NOTICE, /publisher's own declaration/);
  assert.match(RIGHTS_LEGAL_NOTICE, /does not interpret it/);
  assert.match(RIGHTS_LEGAL_NOTICE, /does not give legal advice/);

  assert.ok(RIGHTS_ACCESS_NOTICE.length > 80, "the access notice must be a full sentence");
  assert.match(RIGHTS_ACCESS_NOTICE, /separate/);
  assert.match(RIGHTS_ACCESS_NOTICE, /does not make the files downloadable/);
});

/* ------------------------------------------------------------------ */
/* Absent is not a value                                               */
/* ------------------------------------------------------------------ */

test("a declared value is never read as an absent field", () => {
  // The two cases side by side, because the bug this pins is precisely that
  // they were indistinguishable.
  //
  //   A. the field is absent          -> "Not stated", and it is unstated
  //   C. the field declares a value   -> that value, verbatim, whatever it
  //      happens to spell
  //
  // Whether a field was declared has to be independent of what it was
  // declared to be. The declaration is stored in a column that migration
  // 00066 deliberately does not constrain to a vocabulary, and
  // visibility.metadata is an open token whose alphabet
  // (internal/rights.ValidMetadataVisibility: [A-Za-z0-9_.:-]) makes any
  // underscore spelling a value a publisher may legally declare — so the
  // reader cannot use a value in that space to mean "nothing was said".
  const absent = readDeclaration({ version: 1, usage: {}, visibility: {} });
  const declared = readDeclaration({
    version: 1,
    usage: { commercial_use: "not_stated" },
    visibility: { metadata: "not_stated" },
  });

  const absentRow = usageRows(absent.document)[0];
  const declaredRow = usageRows(declared.document)[0];

  // A: absent keeps its treatment.
  assert.equal(absentRow.rendered, "Not stated");
  assert.equal(absentRow.declared, NOT_STATED);
  assert.ok(absent.unstated.includes("commercial_use"), "an absent field is listed as unstated");

  // C: declared is verbatim — NOT "Not stated", and NOT unstated.
  assert.notEqual(
    declaredRow.rendered,
    absentRow.rendered,
    "a declared value rendered exactly like an absent field",
  );
  assert.equal(declaredRow.rendered, "not_stated (a value this view does not know)");
  assert.equal(declaredRow.declared, "not_stated");
  assert.equal(
    declared.unstated.includes("commercial_use"),
    false,
    "a field the document states was reported as unstated",
  );

  // D: the same on the metadata axis, which is the one the alphabet above
  // makes reachable in a real declaration.
  assert.equal(metadataLabel(absent.document.visibility.metadata), "Not stated");
  assert.equal(
    metadataLabel(declared.document.visibility.metadata),
    "not_stated (a value this view does not know)",
  );
  assert.equal(metadataLabel(declared.document.visibility.metadata) === "Not stated", false);

  // The panel writes `declared` into data-declared, so the node that shows
  // "Not stated" carries no data-declared attribute while the node that
  // shows a declared value carries that value — one story per node.
  assert.equal(absentRow.declared, null);
  assert.equal(declaredRow.declared, "not_stated");
});
