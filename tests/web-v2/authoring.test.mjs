/**
 * Unit tests for the Research tab's authoring model (WEB-06a / T1307).
 *
 * WHY A UNIT TEST ON TOP OF THE BROWSER JOURNEY. The journey proves the four
 * forms reach the API and their versions survive a reload; it can only do
 * that for the values it happens to type. The conversions below are where a
 * silent lie would live — an empty draft field sent as "", an outcome stored
 * top-level (which `additionalProperties: false` would refuse), a JSON field
 * accepted as `[1,2]`, a "not linked" target silently dropped, or a gate
 * report whose warnings were counted as blocks. Each of those is cheap to
 * pin here and expensive to notice in a browser.
 *
 * The module under test is the app's own `authoring.ts` — no copy of the
 * field tables lives in this file, so a field renamed in the product fails
 * these tests instead of being shadowed by a stale duplicate here.
 *
 * Run: node --test tests/web-v2/authoring.test.mjs
 */

import test from "node:test";
import assert from "node:assert/strict";

import {
  AUTHORING_KINDS,
  OUTCOME_METADATA_KEY,
  assemblePayload,
  branchObjectHref,
  canAuthor,
  canSaveDraft,
  emptyValues,
  fieldsFor,
  gateRows,
  gateSummary,
  humanizeOption,
  identityField,
  isAuthoringKind,
  mayContinueVersion,
  outcomeKey,
  outcomeOf,
  researchBranchHref,
  reviewRequiredFields,
  valuesFromPayload,
} from "../../apps/web/app/(main)/projects/[id]/research/authoring.ts";
import {
  createResearchClient,
  newestVersionPerObject,
} from "../../apps/web/app/(main)/projects/[id]/research/research-client.ts";

test("exactly the four slice-a families are authorable, and no others", () => {
  assert.deepEqual([...AUTHORING_KINDS], ["research_question", "hypothesis", "experiment", "calculation"]);
  for (const kind of AUTHORING_KINDS) assert.equal(isAuthoringKind(kind), true);
  // The other eight canonical types belong to T1336/T1337: this slice must
  // not quietly grow a form for them.
  for (const other of ["dataset", "protocol", "material", "sample", "claim", "finding", "external_reference"]) {
    assert.equal(isAuthoringKind(other), false, `${other} must not be authorable in this slice`);
  }
});

test("a draft needs only its identity field", () => {
  for (const kind of AUTHORING_KINDS) {
    const values = emptyValues(kind);
    assert.equal(canSaveDraft(kind, values), false, `${kind} must not save with nothing typed`);
    values[identityField(kind)] = "   ";
    assert.equal(canSaveDraft(kind, values), false, `${kind} must not treat whitespace as a statement`);
    values[identityField(kind)] = "a first sentence";
    assert.equal(canSaveDraft(kind, values), true, `${kind} must save with only its identity field`);
  }
});

test("an empty field is omitted from the payload, never sent as an empty string", () => {
  const { payload, invalidFields } = assemblePayload("research_question", {
    statement: "  Does X adsorb Y?  ",
    purpose: "",
    question_state: "",
    scope: "",
  });
  assert.deepEqual(invalidFields, []);
  // The gate must see a MISSING field, not an answered one.
  assert.deepEqual(payload, { statement: "Does X adsorb Y?" });
});

test("list fields split on commas, and an all-blank list is omitted", () => {
  const { payload } = assemblePayload("experiment", {
    objective: "Measure the isotherm",
    operator_ids: "alice, bob ,, carol",
    sample_ids: " , ",
  });
  assert.deepEqual(payload.operator_ids, ["alice", "bob", "carol"]);
  assert.equal("sample_ids" in payload, false);
});

test("a json field must be an object: an array or invalid text is a field error and sends nothing", () => {
  for (const bad of ["[1, 2]", "not json", "42", "null"]) {
    const { payload, invalidFields } = assemblePayload("experiment", {
      objective: "Measure the isotherm",
      conditions: bad,
    });
    assert.deepEqual(invalidFields, ["conditions"], `${bad} must be refused`);
    assert.equal("conditions" in payload, false);
    assert.equal(payload.objective, "Measure the isotherm", "the valid fields still stand");
  }
  const ok = assemblePayload("calculation", { objective: "Solve", parameters: '{"k": 2}' });
  assert.deepEqual(ok.payload.parameters, { k: 2 });
});

test("a failed result is stored as a failed result, in schema-permitted metadata", () => {
  const { payload } = assemblePayload("experiment", {
    objective: "Measure the isotherm at 298 K",
    result_summary: "The instrument lost vacuum mid-run; no isotherm was recorded.",
    result_outcome: "failed",
  });
  // The experiment schema has no top-level outcome field and forbids extra
  // properties; `metadata` is the open object it does allow.
  assert.equal("result_outcome" in payload, false);
  assert.deepEqual(payload.metadata, { [OUTCOME_METADATA_KEY]: "failed" });
  assert.equal(outcomeOf(payload), "failed");
  assert.equal(outcomeKey("failed"), "research.authoring.outcome.failed");
  assert.equal(outcomeKey("a-code-this-build-predates"), null);
  // An outcome that is not one of the four known codes is not invented.
  const unknown = assemblePayload("calculation", { objective: "Solve", result_outcome: "exploded" });
  assert.equal("metadata" in unknown.payload, false);
});

test("a second version changes only what was typed: the rest of the payload is carried over", () => {
  // The editor is seeded from the head version and submits the whole form, so
  // "the whole form" must mean "the head payload plus this edit". A field the
  // form shows but the reader did not touch is re-sent as it was; a field the
  // form does not own (metadata a previous producer wrote) is carried too.
  const v1 = {
    objective: "Measure the CO2 isotherm at 298 K",
    operator_ids: ["operator-alpha", "operator-beta"],
    conditions: { temperature_k: 298, pressure_range_bar: "1-40" },
    result_summary: "The instrument lost vacuum mid-run.",
    metadata: { [OUTCOME_METADATA_KEY]: "failed", lab_note: "written by another producer" },
  };
  const values = valuesFromPayload("experiment", v1);
  values.result_summary = "The second run stopped at 12 bar.";
  const { payload, invalidFields } = assemblePayload("experiment", values, v1);
  assert.deepEqual(invalidFields, []);
  // Exactly one field differs, and nothing was dropped.
  assert.deepEqual(payload, { ...v1, result_summary: "The second run stopped at 12 bar." });
  assert.equal(outcomeOf(payload), "failed", "an unedited outcome must not revert");
});

test("metadata keys this build does not own survive an edit, and clearing the outcome removes only the code", () => {
  const base = {
    objective: "Fit the isotherm",
    metadata: { [OUTCOME_METADATA_KEY]: "inconclusive", imported_from: "rsg-export-v3" },
  };
  const values = valuesFromPayload("calculation", base);
  values.result_outcome = "failed";
  const changed = assemblePayload("calculation", values, base).payload;
  assert.deepEqual(changed.metadata, { [OUTCOME_METADATA_KEY]: "failed", imported_from: "rsg-export-v3" });

  // Clearing the select removes the outcome code and leaves the rest alone.
  values.result_outcome = "";
  const cleared = assemblePayload("calculation", values, base).payload;
  assert.deepEqual(cleared.metadata, { imported_from: "rsg-export-v3" });
  assert.equal(outcomeOf(cleared), "");
});

test("only a writing role may author: null (anonymous / non-member) and viewer are read-only", () => {
  // A POSITIVE role list, not `role !== "viewer"`: the shell hands `null` to
  // an anonymous reader and to a signed-in non-member of a readable project,
  // and a "not a viewer" gate would fail OPEN for exactly those callers.
  for (const role of ["owner", "maintainer", "contributor"]) {
    assert.equal(canAuthor(role), true, `${role} may write research state`);
  }
  for (const role of ["viewer", null]) {
    assert.equal(canAuthor(role), false, `${String(role)} must be read-only`);
  }
});

test("clearing a nullable field on an edit is sent as an explicit null, so the clear survives the shallow merge", () => {
  // The version patch is a shallow merge and an omitted field keeps its old
  // value: omitting a cleared field is exactly how "Saved as version N" ends
  // up meaning nothing. The schemas type these fields ["string", "null"], so
  // the clear is expressible.
  const base = {
    objective: "Fit the isotherm",
    method: "Least squares",
    software: "python-scipy 1.14",
    result_summary: "Inconclusive: too few points.",
    parameters: { qmax: 7.2 },
    metadata: { [OUTCOME_METADATA_KEY]: "inconclusive" },
  };
  const values = valuesFromPayload("calculation", base);
  values.software = "";
  values.result_summary = "";
  const { payload, invalidFields } = assemblePayload("calculation", values, base);
  assert.deepEqual(invalidFields, []);
  assert.equal(payload.software, null, "a cleared nullable field must be null, not absent");
  assert.equal(payload.result_summary, null);
  assert.equal("software" in payload, true);
  // The fields that were not cleared are still carried.
  assert.equal(payload.method, "Least squares");
  assert.deepEqual(payload.parameters, { qmax: 7.2 });
  assert.equal(outcomeOf(payload), "inconclusive");

  // After the save the form re-seeds from the saved version: a null payload
  // field reads back as an empty input (nothing left to type over).
  assert.equal(valuesFromPayload("calculation", payload).software, "");
  assert.equal(valuesFromPayload("calculation", payload).result_summary, "");
});

test("clearing a list field on an edit sends an empty list; a field the version never had stays out", () => {
  const base = {
    objective: "Measure the isotherm",
    operator_ids: ["operator-alpha", "operator-beta"],
    conditions: { temperature_k: 298 },
  };
  const values = valuesFromPayload("experiment", base);
  values.operator_ids = "";
  values.sample_ids = " , ";
  values.protocol_version_id = "";
  const { payload } = assemblePayload("experiment", values, base);
  assert.deepEqual(payload.operator_ids, [], "a cleared list must be an empty list, not the old one");
  // protocol_version_id was never stored, so there is nothing to clear: an
  // empty field on a version that never had it stays absent (draft rule).
  assert.equal("protocol_version_id" in payload, false);
  assert.equal("sample_ids" in payload, false);

  // A field whose schema allows neither null nor an empty list cannot be
  // emptied through the API; it is left unnamed rather than sent as a lie.
  const questionBase = { statement: "Does X adsorb Y?", scope: { temperature: 298 } };
  const questionValues = valuesFromPayload("research_question", questionBase);
  questionValues.statement = "";
  questionValues.scope = "";
  const question = assemblePayload("research_question", questionValues, questionBase).payload;
  assert.equal("statement" in question, false);
  assert.equal("scope" in question, false);
});

test("the editor is offered only on the head version, never on an older one", () => {
  // The version endpoint continues the object's CURRENT version, so a form
  // seeded from an older version would write that older state over the head.
  assert.equal(mayContinueVersion(2, 2), true);
  assert.equal(mayContinueVersion(1, 2), false, "version 1 of an object at version 2 is read-only");
  assert.equal(mayContinueVersion(1, 1), true);
});

test("continuing a draft reads back exactly what the stored payload holds", () => {
  const payload = {
    statement: "Does X adsorb Y?",
    purpose: "Compare two samples",
    question_state: "open",
    scope: { temperature: 298 },
    metadata: { [OUTCOME_METADATA_KEY]: "inconclusive" },
  };
  const values = valuesFromPayload("research_question", payload);
  assert.equal(values.statement, "Does X adsorb Y?");
  assert.equal(values.question_state, "open");
  assert.equal(values.scope, JSON.stringify({ temperature: 298 }));
  // The outcome belongs to the experiment/calculation forms only; the
  // question form must not surface it.
  assert.equal("result_outcome" in values, false);
  // And the round trip is stable: what was read back assembles to the same
  // payload fields.
  const { payload: again } = assemblePayload("research_question", values);
  assert.deepEqual(again, {
    statement: "Does X adsorb Y?",
    purpose: "Compare two samples",
    question_state: "open",
    scope: { temperature: 298 },
  });
});

test("the gate report is rendered as the server states it: failures only, severities kept", () => {
  const report = {
    gate: "pr",
    verdict: "blocked",
    explanation: "",
    results: [
      { check: "schema_typed", severity: "blocking", passed: false, subject: "hypothesis v1 (obj-1)", detail: "question_id is required", why: "why-1" },
      { check: "required_fields", severity: "warning", passed: false, subject: "hypothesis v1 (obj-1)", detail: "scope", why: "why-2" },
      { check: "schema_known", severity: "blocking", passed: true, subject: "hypothesis v1 (obj-1)", why: "why-3" },
    ],
  };
  const rows = gateRows(report);
  assert.equal(rows.length, 2, "a passed check needs no reader's attention");
  assert.deepEqual(rows.map((row) => row.check), ["schema_typed", "required_fields"]);
  assert.equal(rows[0].severity, "blocking");
  assert.equal(rows[0].detail, "question_id is required");

  const blocked = gateSummary(report);
  assert.deepEqual(blocked, { blocked: true, blocking: 1, warnings: 1, verdict: "blocked" });
  // A warning is not a block: a branch the server calls pass_with_warnings
  // must not be rendered as refused, and vice versa.
  const warned = gateSummary({ ...report, verdict: "pass_with_warnings" });
  assert.equal(warned.blocked, false);
  assert.equal(warned.warnings, 1);
});

test("the field tables cover every input the forms render, and mark the review fields", () => {
  // Marked fields are the form's own subset of the server's pr-gate table.
  assert.deepEqual([...reviewRequiredFields("research_question")], ["purpose", "question_state"]);
  assert.deepEqual([...reviewRequiredFields("hypothesis")], ["hypothesis_type", "scope"]);
  assert.deepEqual(
    [...reviewRequiredFields("experiment")],
    ["operator_ids", "sample_ids", "protocol_version_id", "conditions", "result_summary"],
  );
  assert.deepEqual(
    [...reviewRequiredFields("calculation")],
    ["software", "input_object_ids", "parameters", "result_summary"],
  );
  for (const kind of AUTHORING_KINDS) {
    const names = fieldsFor(kind).map((field) => field.name);
    assert.equal(names[0], identityField(kind), `${kind}'s first field must be its identity`);
    assert.equal(new Set(names).size, names.length, `${kind} repeats a field`);
    for (const field of fieldsFor(kind)) {
      // Catalog keys carry no underscores (a key built from a schema field
      // name would render as a visible miss marker).
      assert.equal(field.labelKey.includes("_"), false, `${field.labelKey} uses an underscore`);
      if (field.control === "select") assert.ok((field.options ?? []).length > 0, `${field.name} has no options`);
    }
  }
  // The hypothesis's question is a choice of the slice's question objects.
  const question = fieldsFor("hypothesis").find((field) => field.name === "question_id");
  assert.equal(question.control, "target");
  assert.equal(question.targetType, "research_question");
  assert.equal(humanizeOption("not_run"), "not run");
});

test("the branch inventory lists one row per object, at its newest version", async () => {
  // /query is a graph read: it returns each object at its as-of version AND
  // the exact version a relation pins. After a hypothesis is continued past
  // the version a relation points at, the same object arrives twice. The
  // inventory asks "which objects does this branch hold", so the pinned
  // older row must collapse into the as-of one (otherwise the object is
  // listed once per version and offered twice as a select target).
  const hypothesisV1 = {
    id: "hyp-1", object_type: "hypothesis", current_version: 2, version_id: "hyp-1-v1",
    version_no: 1, title: "H", lifecycle_state: "active", payload: {},
  };
  const hypothesisV2 = {
    ...hypothesisV1, version_id: "hyp-1-v2", version_no: 2, payload: { hypothesis_type: "mechanistic" },
  };
  const question = {
    id: "q-1", object_type: "research_question", current_version: 1, version_id: "q-1-v1",
    version_no: 1, title: "Q", lifecycle_state: "active", payload: {},
  };

  const deduped = newestVersionPerObject([question, hypothesisV1, hypothesisV1, hypothesisV2]);
  assert.deepEqual(
    deduped.map((object) => `${object.id}@${object.version_id}`),
    ["q-1@q-1-v1", "hyp-1@hyp-1-v2"],
  );

  // And the client applies it: a slice carrying the relation-pinned v1 row
  // answers the inventory with one hypothesis row, the newest one.
  const client = createResearchClient("http://api.test", {
    fetch: async () => ({
      ok: true,
      status: 200,
      json: async () => ({
        project_id: "p-1",
        objects: [question, hypothesisV1, hypothesisV2],
        relations: [
          {
            id: "rel-1",
            relation_type: "addresses_question",
            source_object_version_id: "hyp-1-v1",
            target_object_version_id: "q-1-v1",
          },
        ],
      }),
    }),
  });
  const slice = await client.branchSlice("p-1", "branch-1");
  assert.equal(slice.objects.length, 2);
  assert.equal(slice.objects.find((object) => object.id === "hyp-1")?.version_id, "hyp-1-v2");
});

test("the hrefs carry the branch, so a reload returns to the same research path", () => {
  const research = researchBranchHref("p1", "questions", "b1", "obj-9");
  const researchUrl = new URL(research, "http://x");
  assert.equal(researchUrl.pathname, "/projects/p1/research");
  assert.equal(researchUrl.searchParams.get("view"), "questions");
  assert.equal(researchUrl.searchParams.get("branch"), "b1");
  assert.equal(researchUrl.searchParams.get("focus"), "obj-9");

  const object = branchObjectHref("p1", "obj-1", "b1", { version: 2, from: "obj-2", fromVersion: 1 });
  const objectUrl = new URL(object, "http://x");
  assert.equal(objectUrl.pathname, "/projects/p1/research/objects/obj-1");
  assert.equal(objectUrl.searchParams.get("branch"), "b1");
  assert.equal(objectUrl.searchParams.get("version"), "2");
  assert.equal(objectUrl.searchParams.get("from"), "obj-2");
  assert.equal(objectUrl.searchParams.get("fromVersion"), "1");
  // No branch, no parameter: a browse link never invents a coordinate.
  assert.equal(new URL(branchObjectHref("p1", "obj-1", ""), "http://x").search, "");
});
