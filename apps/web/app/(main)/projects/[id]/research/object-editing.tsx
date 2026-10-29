"use client";

import { useEffect, useMemo, useState } from "react";

import { useT } from "../../../../i18n-provider";
import { sessionTokenStorage } from "../../../../../lib/auth";
import { useProjectShell } from "../shell-context";
import {
  assemblePayload,
  branchObjectHref,
  canAuthor,
  fieldsFor,
  gateRows,
  gateSummary,
  humanizeOption,
  isAuthoringKind,
  mayContinueVersion,
  reviewRequiredFields,
  valuesFromPayload,
  type AuthoringKind,
  type AuthoringValues,
  type FieldSpec,
  type GateReport,
} from "./authoring";
import {
  ResearchApiError,
  createResearchClient,
  type ResearchClient,
  type ResearchObjectRef,
} from "./research-client";
import { type ObjectDetail } from "./research-data";
import "./research.css";

/**
 * Editing one object of the four slice-a families (WEB-06a, U03 slice a).
 *
 * Three panels that answer three different questions, kept apart on purpose:
 *
 *   - ObjectEditor: "continue this draft." It saves a NEW immutable version
 *     whose patch is the form's payload, so the version log keeps every step
 *     (docs/09) and no draft is edited in place.
 *   - CompletenessCheck: "is this ready for review?" It runs the server's pr
 *     gate and renders the report's own rows. It CHECKS ONLY — this product
 *     surface has no submit-for-review action in this slice, and the panel
 *     says so, so a reader is never told a submission happened.
 *   - RelationEditor: "connect this to what it answers." Relations pin exact
 *     versions (the source is the version on screen), so a later edit never
 *     rewrites what an edge meant.
 *
 * All three write through the same client and re-read through `onSaved`, so
 * the version log, the relation list and the header all show what the server
 * now holds.
 */
export function ObjectEditor({
  detail,
  branchId,
  onSaved,
}: {
  detail: ObjectDetail;
  branchId: string;
  onSaved: () => void;
}) {
  const shell = useProjectShell();
  const t = useT();
  const kind = detail.object_type;

  // The version this panel last saved. Kept as the VERSION NUMBER, so the
  // confirmation disappears the moment the reader opens another version
  // instead of outliving the save it describes.
  const [savedVersion, setSavedVersion] = useState<number | null>(null);
  const [targets, setTargets] = useState<ResearchObjectRef[]>([]);

  const client = useMemo(
    () =>
      shell === null
        ? null
        : createResearchClient(shell.apiBaseUrl, { csrfToken: () => sessionTokenStorage().get() }),
    [shell],
  );

  const projectId = shell?.project.id ?? "";
  // A `target` field (a hypothesis's question) is a choice of an object on
  // THIS branch: the same list the create form offers, so continuing a draft
  // never asks the reader to type an id they cannot verify.
  useEffect(() => {
    if (client === null || projectId === "" || branchId === "") return;
    let cancelled = false;
    client
      .branchSlice(projectId, branchId)
      .then((slice) => {
        if (!cancelled) setTargets(slice.objects);
      })
      .catch(() => {
        if (!cancelled) setTargets([]);
      });
    return () => {
      cancelled = true;
    };
  }, [client, projectId, branchId]);

  if (shell === null || client === null || !isAuthoringKind(kind)) return null;
  // A viewer reads research state; the server refuses their writes anyway,
  // and offering a form that can only fail is not a permission dialog.
  if (!canAuthor(shell.role)) return null;

  return (
    <section className="research-panel" aria-label={t("research.authoring.edit.title")} data-object-editor={detail.id}>
      <header>
        <p className="research-eyebrow">{t("research.authoring.edit.eyebrow")}</p>
        <h3>{t("research.authoring.edit.title")}</h3>
        <p className="research-panel-note">{t("research.authoring.edit.note", { version: detail.current_version })}</p>
      </header>
      {mayContinueVersion(detail.selected_version_no, detail.current_version) ? (
        /* Keyed by the version on screen, so opening another version (or the
           reload after a save) seeds the form from THAT version's payload. A
           fresh mount is the seed — an effect writing state would render the
           previous version's text for one frame first. */
        <ObjectEditorForm
          key={`${detail.id}:${detail.selected_version_no}`}
          detail={detail}
          kind={kind}
          branchId={branchId}
          client={client}
          targets={targets}
          savedVersion={savedVersion}
          onSaved={(versionNo) => {
            setSavedVersion(versionNo);
            onSaved();
          }}
        />
      ) : (
        // An old version is read-only here: the version endpoint continues
        // the object's CURRENT version, so a form seeded from an older one
        // would write that older state over what the newer version changed.
        <div className="research-authoring-stale" data-object-edit-stale={detail.current_version}>
          <p>
            {t("research.authoring.edit.stale", {
              version: detail.selected_version_no,
              current: detail.current_version,
            })}
          </p>
          <a
            className="research-authoring-button"
            data-object-edit-current
            href={branchObjectHref(detail.project_id, detail.id, branchId, {
              version: detail.current_version,
            })}
          >
            {t("research.authoring.edit.openCurrent", { current: detail.current_version })}
          </a>
        </div>
      )}
    </section>
  );
}

/** The form half of the editor: seeded once per version, dismounted when the
 *  version changes (see the `key` above). */
function ObjectEditorForm({
  detail,
  kind,
  branchId,
  client,
  targets,
  savedVersion,
  onSaved,
}: {
  detail: ObjectDetail;
  kind: AuthoringKind;
  branchId: string;
  client: ResearchClient;
  targets: ResearchObjectRef[];
  savedVersion: number | null;
  onSaved: (versionNo: number) => void;
}) {
  const t = useT();
  const [values, setValues] = useState<AuthoringValues>(() => valuesFromPayload(kind, detail.payload));
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const [invalid, setInvalid] = useState<string[]>([]);

  const save = (): void => {
    setError("");
    setInvalid([]);
    // `detail.payload` is the CURRENT version's payload (the editor is only
    // offered on it), so the form's payload continues that version rather
    // than an older one, and metadata keys this form does not own survive.
    const assembled = assemblePayload(kind, values, detail.payload);
    if (assembled.invalidFields.length > 0) {
      setInvalid(assembled.invalidFields);
      return;
    }
    setSaving(true);
    client
      .createVersion(detail.project_id, branchId, detail.id, {
        expectedVersion: detail.current_version,
        patch: assembled.payload,
      })
      .then((object) => onSaved(object.version_no))
      .catch((cause: unknown) => {
        setError(
          cause instanceof ResearchApiError && cause.status === 409
            ? t("research.authoring.edit.conflict")
            : cause instanceof ResearchApiError && cause.message !== ""
              ? cause.message
              : t("research.authoring.edit.error"),
        );
      })
      .finally(() => setSaving(false));
  };

  return (
    <form
      className="research-authoring-form"
      data-object-edit-form={kind}
      onSubmit={(event) => {
        event.preventDefault();
        save();
      }}
    >
      {fieldsFor(kind).map((field) => (
        <EditField
          key={field.name}
          field={field}
          value={values[field.name] ?? ""}
          invalid={invalid.includes(field.name)}
          requiredForReview={reviewRequiredFields(kind).includes(field.name)}
          targets={
            field.targetType === undefined
              ? []
              : targets.filter((candidate) => candidate.object_type === field.targetType && candidate.id !== detail.id)
          }
          onChange={(next) => setValues((current) => ({ ...current, [field.name]: next }))}
        />
      ))}
      <div className="research-authoring-actions">
        <button type="submit" className="research-authoring-button primary" disabled={saving} data-object-edit-save>
          {saving ? t("research.authoring.edit.saving") : t("research.authoring.edit.save")}
        </button>
      </div>
      {invalid.length === 0 ? null : (
        <p className="research-authoring-error" data-object-edit-invalid>
          {t("research.authoring.invalidJson", { fields: invalid.map(humanizeOption).join(", ") })}
        </p>
      )}
      {error === "" ? null : <p className="research-authoring-error" data-object-edit-error>{error}</p>}
      {savedVersion === null || savedVersion !== detail.selected_version_no ? null : (
        <p className="research-authoring-saved" data-object-edit-saved={savedVersion}>
          {t("research.authoring.edit.saved", { version: savedVersion })}
        </p>
      )}
    </form>
  );
}

/** The review-completeness panel: the server's pr-gate report, nothing else. */
export function CompletenessCheck({ branchId }: { branchId: string }) {
  const shell = useProjectShell();
  const t = useT();
  const [running, setRunning] = useState(false);
  const [report, setReport] = useState<GateReport | null>(null);
  const [error, setError] = useState("");

  const client = useMemo(
    () =>
      shell === null
        ? null
        : createResearchClient(shell.apiBaseUrl, { csrfToken: () => sessionTokenStorage().get() }),
    [shell],
  );

  if (shell === null || client === null) return null;
  // The check is a write-gated call on the server; a viewer is offered the
  // read-only surfaces only.
  if (!canAuthor(shell.role)) return null;

  const run = (): void => {
    setError("");
    setRunning(true);
    client
      .validateBranch(shell.project.id, branchId, "pr")
      .then(setReport)
      .catch((cause: unknown) => {
        setError(cause instanceof ResearchApiError && cause.message !== "" ? cause.message : t("research.authoring.check.error"));
      })
      .finally(() => setRunning(false));
  };

  const rows = report === null ? [] : gateRows(report);
  const summary = report === null ? null : gateSummary(report);

  return (
    <section className="research-panel" aria-label={t("research.authoring.check.title")} data-gate-check-panel>
      <header>
        <p className="research-eyebrow">{t("research.authoring.check.eyebrow")}</p>
        <h3>{t("research.authoring.check.title")}</h3>
        <p className="research-panel-note">{t("research.authoring.check.note")}</p>
      </header>
      <div className="research-authoring-actions">
        <button type="button" className="research-authoring-button" onClick={run} disabled={running} data-gate-check>
          {running ? t("research.authoring.check.running") : t("research.authoring.check.run")}
        </button>
      </div>
      {error === "" ? null : <p className="research-authoring-error" data-gate-check-error>{error}</p>}
      {report === null || summary === null ? null : (
        <div className="research-gate-report" data-gate-report data-gate-verdict={report.verdict}>
          {summary.blocked ? (
            <p className="research-gate-verdict blocked" data-gate-blocked={summary.blocking}>
              {t("research.authoring.check.blocked", { count: summary.blocking })}
            </p>
          ) : (
            <p className="research-gate-verdict passed" data-gate-passed>
              {t("research.authoring.check.passed")}
            </p>
          )}
          {summary.warnings === 0 ? null : (
            <p className="research-authoring-note" data-gate-warnings={summary.warnings}>
              {t("research.authoring.check.warnings", { count: summary.warnings })}
            </p>
          )}
          {rows.length === 0 ? (
            <p className="research-authoring-note" data-gate-no-gaps>{t("research.authoring.check.noGaps")}</p>
          ) : (
            <ul className="research-gate-rows" data-gate-rows>
              {rows.map((row) => (
                <li
                  key={`${row.check}:${row.subject}:${row.detail}`}
                  data-gate-row={row.check}
                  data-gate-severity={row.severity}
                >
                  <span className={`research-gate-severity ${row.severity}`}>{row.severity}</span>
                  <strong>{humanizeOption(row.check)}</strong>
                  {row.subject === "" ? null : <span className="research-gate-subject">{row.subject}</span>}
                  {row.detail === "" ? null : <span className="research-gate-detail">{row.detail}</span>}
                  <small>{row.why}</small>
                </li>
              ))}
            </ul>
          )}
          <p className="research-gate-notice" data-gate-not-submitted>
            {t("research.authoring.check.notSubmitted")}
          </p>
        </div>
      )}
    </section>
  );
}

/** The typed-relation form: source = the version on screen, target = a
 *  current version of the branch. */
export function RelationEditor({
  detail,
  branchId,
  onSaved,
}: {
  detail: ObjectDetail;
  branchId: string;
  onSaved: () => void;
}) {
  const shell = useProjectShell();
  const t = useT();
  const [relationType, setRelationType] = useState("addresses_question");
  const [targetId, setTargetId] = useState("");
  const [objects, setObjects] = useState<ResearchObjectRef[]>([]);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");

  const client = useMemo(
    () =>
      shell === null
        ? null
        : createResearchClient(shell.apiBaseUrl, { csrfToken: () => sessionTokenStorage().get() }),
    [shell],
  );

  const projectId = shell?.project.id ?? "";
  useEffect(() => {
    if (client === null || projectId === "") return;
    let cancelled = false;
    client
      .branchSlice(projectId, branchId)
      .then((slice) => {
        if (!cancelled) setObjects(slice.objects);
      })
      .catch(() => {
        if (!cancelled) setObjects([]);
      });
    return () => {
      cancelled = true;
    };
  }, [client, projectId, branchId]);

  if (shell === null || client === null) return null;
  if (!canAuthor(shell.role)) return null;

  const targetType = RELATION_TARGETS.get(relationType) ?? "";
  const targets = objects.filter(
    (object) => object.object_type === targetType && object.id !== detail.id,
  );
  const target = targets.find((object) => object.id === targetId) ?? null;

  const add = (): void => {
    if (target === null) return;
    setError("");
    setNotice("");
    setSaving(true);
    client
      .createRelation(shell.project.id, branchId, {
        relationType,
        sourceVersionId: detail.version_id,
        targetVersionId: target.version_id,
      })
      .then(() => {
        setNotice(t("research.authoring.relation.added", { title: target.title }));
        setTargetId("");
        onSaved();
      })
      .catch((cause: unknown) => {
        setError(cause instanceof ResearchApiError && cause.message !== "" ? cause.message : t("research.authoring.relation.error"));
      })
      .finally(() => setSaving(false));
  };

  return (
    <section className="research-panel" aria-label={t("research.authoring.relation.title")} data-relation-editor>
      <header>
        <p className="research-eyebrow">{t("research.authoring.relation.eyebrow")}</p>
        <h3>{t("research.authoring.relation.title")}</h3>
        <p className="research-panel-note">{t("research.authoring.relation.note")}</p>
      </header>
      <form
        className="research-authoring-form"
        data-relation-form
        onSubmit={(event) => {
          event.preventDefault();
          add();
        }}
      >
        <label>
          <span>{t("research.authoring.relation.type")}</span>
          <select
            value={relationType}
            data-authoring-relation-type
            onChange={(event) => {
              setRelationType(event.target.value);
              setTargetId("");
            }}
          >
            {[...RELATION_TARGETS.keys()].map((type) => (
              <option key={type} value={type}>
                {relationLabel(t, type)}
              </option>
            ))}
          </select>
        </label>
        <label>
          <span>{t("research.authoring.relation.target")}</span>
          <select value={targetId} data-authoring-relation-target onChange={(event) => setTargetId(event.target.value)}>
            <option value="">{t("research.authoring.target.choose")}</option>
            {targets.map((object) => (
              <option key={object.id} value={object.id}>
                {object.title}
              </option>
            ))}
          </select>
          {targets.length === 0 ? <small className="research-authoring-note">{t("research.authoring.target.none")}</small> : null}
        </label>
        <div className="research-authoring-actions">
          <button type="submit" className="research-authoring-button" disabled={saving || target === null} data-relation-add>
            {saving ? t("research.authoring.relation.adding") : t("research.authoring.relation.add")}
          </button>
        </div>
        {error === "" ? null : <p className="research-authoring-error" data-relation-error>{error}</p>}
        {notice === "" ? null : <p className="research-authoring-saved" data-relation-saved>{notice}</p>}
      </form>
    </section>
  );
}

/** The relation types this slice writes and the object family each targets
 *  (internal/rsg/relationcatalog). Anything else is not offered. */
const RELATION_TARGETS: ReadonlyMap<string, string> = new Map([
  ["addresses_question", "research_question"],
  ["tests_hypothesis", "hypothesis"],
]);

function relationLabel(t: ReturnType<typeof useT>, relationType: string): string {
  switch (relationType) {
    case "addresses_question":
      return t("research.relation.addresses-question");
    case "tests_hypothesis":
      return t("research.relation.tests-hypothesis");
    default:
      return humanizeOption(relationType);
  }
}

function EditField({
  field,
  value,
  invalid,
  requiredForReview,
  targets,
  onChange,
}: {
  field: FieldSpec;
  value: string;
  invalid: boolean;
  requiredForReview: boolean;
  targets: ResearchObjectRef[];
  onChange: (next: string) => void;
}) {
  const t = useT();
  const shared = {
    "data-object-edit-input": field.name,
    "aria-invalid": invalid ? true : undefined,
  } as const;
  const label = (
    <span>
      {t(field.labelKey)}
      {requiredForReview ? <span className="research-authoring-required"> *</span> : null}
    </span>
  );
  if (field.control === "textarea" || field.control === "json") {
    return (
      <label className={invalid ? "invalid" : undefined}>
        {label}
        <textarea
          rows={field.control === "json" ? 2 : 3}
          value={value}
          placeholder={field.control === "json" ? t("research.authoring.field.jsonHint") : undefined}
          onChange={(event) => onChange(event.target.value)}
          {...shared}
        />
      </label>
    );
  }
  if (field.control === "select") {
    return (
      <label className={invalid ? "invalid" : undefined}>
        {label}
        <select value={value} onChange={(event) => onChange(event.target.value)} {...shared}>
          <option value="">{t("research.authoring.option.none")}</option>
          {(field.options ?? []).map((option) => (
            <option key={option} value={option}>
              {humanizeOption(option)}
            </option>
          ))}
        </select>
      </label>
    );
  }
  if (field.control === "target") {
    // The same choice the create form offers: the objects of that family on
    // THIS branch, by title. A stored reference the branch does not hold
    // (an object of an older branch) stays selectable as itself rather than
    // being silently dropped from the next version.
    const known = targets.some((target) => target.id === value);
    return (
      <label className={invalid ? "invalid" : undefined}>
        {label}
        <select value={value} onChange={(event) => onChange(event.target.value)} {...shared}>
          <option value="">{t("research.authoring.target.choose")}</option>
          {value !== "" && !known ? <option value={value}>{t("research.authoring.target.outside")}</option> : null}
          {targets.map((target) => (
            <option key={target.id} value={target.id}>
              {target.title}
            </option>
          ))}
        </select>
        {targets.length === 0 ? (
          <small className="research-authoring-note">{t("research.authoring.target.none")}</small>
        ) : null}
      </label>
    );
  }
  return (
    <label className={invalid ? "invalid" : undefined}>
      {label}
      <input
        type="text"
        value={value}
        placeholder={field.control === "list" ? t("research.authoring.field.listHint") : undefined}
        onChange={(event) => onChange(event.target.value)}
        {...shared}
      />
    </label>
  );
}
