"use client";

import Link from "next/link";
import { useRouter } from "next/navigation";
import { useCallback, useEffect, useMemo, useState } from "react";
import { Spinner } from "@primer/react";

import { useT } from "../../../../i18n-provider";
import { sessionTokenStorage } from "../../../../../lib/auth";
import { useProjectShell } from "../shell-context";
import {
  AUTHORING_KINDS,
  assemblePayload,
  branchObjectHref,
  canAuthor,
  canSaveDraft,
  emptyValues,
  fieldsFor,
  humanizeOption,
  identityField,
  outcomeKey,
  outcomeOf,
  researchBranchHref,
  reviewRequiredFields,
  type AuthoringKind,
  type AuthoringValues,
  type FieldSpec,
} from "./authoring";
import { objectTypeKey } from "./research-data";
import {
  ResearchApiError,
  createResearchClient,
  type ResearchBranch,
  type ResearchObjectRef,
  type ResearchSlice,
} from "./research-client";
import "./research.css";

/**
 * The Research tab's authoring surface (WEB-06a, U03 slice a).
 *
 * WHAT IT DOES, IN THE ORDER A MEMBER NEEDS IT:
 *
 *   1. lists the project's branches and lets a member switch, so every write
 *      names the research path it lands on (the branch is in the URL, so a
 *      reload returns to the same path);
 *   2. `New research branch` forks one (the first branch a fresh project has);
 *   3. `Save draft` writes ONE object of the four slice-a families — a draft
 *      may carry only its identity field (D15), and the reader continues it on
 *      its own page;
 *   4. lists what the selected branch holds, with a failed result still
 *      reading as a failed result.
 *
 * WHAT IT DOES NOT DO. It never claims a review submission: checking the
 * review gate is the object page's job and it only CHECKS
 * (`POST …:validate`) — this panel does not submit anything. It authors no
 * Dataset/Protocol/Material/Sample or Claim/Finding/ExternalReference (those
 * are T1336/T1337). And it writes no copy outside the catalog.
 */
export function ResearchAuthoring({
  projectId,
  branchId,
  view,
}: {
  projectId: string;
  branchId: string;
  view: string;
}) {
  const shell = useProjectShell();
  const t = useT();
  const router = useRouter();

  const client = useMemo(
    () =>
      shell === null
        ? null
        : createResearchClient(shell.apiBaseUrl, { csrfToken: () => sessionTokenStorage().get() }),
    [shell],
  );

  const [branches, setBranches] = useState<ResearchBranch[] | null>(null);
  const [branchesError, setBranchesError] = useState("");
  // The slice is stored WITH the branch it belongs to: a reader who switches
  // branch never sees the previous branch's objects under the new branch's
  // name, and no effect has to clear it synchronously.
  const [slice, setSlice] = useState<{ branchId: string; value: ResearchSlice } | null>(null);
  const [sliceError, setSliceError] = useState("");
  const [reloadToken, setReloadToken] = useState(0);

  const [showBranchForm, setShowBranchForm] = useState(false);
  const [branchName, setBranchName] = useState("");
  const [branchPurpose, setBranchPurpose] = useState("");
  const [creatingBranch, setCreatingBranch] = useState(false);
  const [branchError, setBranchError] = useState("");

  const [kind, setKind] = useState<AuthoringKind>("research_question");
  const [values, setValues] = useState<AuthoringValues>(() => emptyValues("research_question"));
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState("");
  const [invalidFields, setInvalidFields] = useState<string[]>([]);
  const [saved, setSaved] = useState<{ objectId: string; versionNo: number; title: string } | null>(null);

  const loadBranches = useCallback(() => {
    if (client === null) return;
    client
      .listBranches(projectId)
      .then((next) => {
        setBranchesError("");
        setBranches(next);
      })
      .catch((cause: unknown) => {
        setBranchesError(cause instanceof ResearchApiError ? t("research.authoring.branch.unavailable") : t("research.error.fallback"));
      });
  }, [client, projectId, t]);

  useEffect(() => {
    loadBranches();
  }, [loadBranches]);

  // No branch in the URL yet: land on the project's first branch (main when
  // it has one) instead of leaving the selector showing a path that is not
  // selected. `replace` keeps this out of the back-button history.
  useEffect(() => {
    if (branchId !== "" || branches === null || branches.length === 0) return;
    router.replace(researchBranchHref(projectId, view, branches[0].id));
  }, [branchId, branches, projectId, router, view]);

  useEffect(() => {
    if (client === null || branchId === "") return;
    let cancelled = false;
    client
      .branchSlice(projectId, branchId)
      .then((next) => {
        if (cancelled) return;
        setSliceError("");
        setSlice({ branchId, value: next });
      })
      .catch(() => {
        if (!cancelled) setSliceError(t("research.error.fallback"));
      });
    return () => {
      cancelled = true;
    };
  }, [client, projectId, branchId, reloadToken, t]);

  if (shell === null || client === null) return null;

  const selectedBranch = branches?.find((branch) => branch.id === branchId) ?? null;
  const branchNameForDisplay = selectedBranch?.name ?? "";
  // Positive role check: `role` is null for an anonymous reader and for a
  // signed-in non-member of a readable project, and those callers must not be
  // offered a form (see canAuthor in authoring.ts).
  const canWrite = canAuthor(shell.role);
  const currentSlice = slice !== null && slice.branchId === branchId ? slice.value : null;
  const objects = currentSlice?.objects ?? [];

  const switchBranch = (nextBranchId: string): void => {
    router.push(researchBranchHref(projectId, view, nextBranchId));
  };

  const createBranch = (): void => {
    setBranchError("");
    setCreatingBranch(true);
    client
      .createBranch(projectId, { name: branchName.trim(), purpose: branchPurpose.trim() })
      .then((branch) => {
        setBranchName("");
        setBranchPurpose("");
        setShowBranchForm(false);
        setBranches((current) => (current === null ? [branch] : [...current, branch]));
        router.push(researchBranchHref(projectId, view, branch.id));
      })
      .catch((cause: unknown) => {
        setBranchError(
          cause instanceof ResearchApiError && cause.code === "BRANCH_NAME_TAKEN"
            ? t("research.authoring.branch.nameTaken")
            : cause instanceof ResearchApiError && cause.message !== ""
              ? cause.message
              : t("research.authoring.branch.error"),
        );
      })
      .finally(() => setCreatingBranch(false));
  };

  const switchKind = (next: AuthoringKind): void => {
    setKind(next);
    setValues(emptyValues(next));
    setInvalidFields([]);
    setSaveError("");
    setSaved(null);
  };

  const saveDraft = (): void => {
    setSaveError("");
    setInvalidFields([]);
    setSaved(null);
    const assembled = assemblePayload(kind, values);
    if (assembled.invalidFields.length > 0) {
      setInvalidFields(assembled.invalidFields);
      return;
    }
    if (branchId === "") {
      setSaveError(t("research.authoring.noBranch"));
      return;
    }
    setSaving(true);
    client
      .createObject(projectId, branchId, { objectType: kind, payload: assembled.payload })
      .then((object) => {
        setSaved({ objectId: object.id, versionNo: object.version_no, title: object.title });
        setValues(emptyValues(kind));
        setReloadToken((token) => token + 1);
      })
      .catch((cause: unknown) => {
        setSaveError(cause instanceof ResearchApiError && cause.message !== "" ? cause.message : t("research.authoring.saveError"));
      })
      .finally(() => setSaving(false));
  };

  const targetObjects = (targetType: string): ResearchObjectRef[] =>
    objects.filter((object) => object.object_type === targetType);

  return (
    <section className="research-panel research-authoring" aria-label={t("research.authoring.title")} data-research-authoring>
      <header>
        <p className="research-eyebrow">{t("research.authoring.eyebrow")}</p>
        <h3>{t("research.authoring.title")}</h3>
        <p className="research-panel-note">{t("research.authoring.note")}</p>
      </header>

      <div className="research-branch-bar">
        <label htmlFor="research-branch-select">{t("research.authoring.branch.label")}</label>
        <select
          id="research-branch-select"
          data-authoring-branch-select
          value={branchId}
          onChange={(event) => switchBranch(event.target.value)}
          disabled={branches === null || branches.length === 0}
        >
          {branches === null ? <option value="">{t("research.loading")}</option> : null}
          {branches !== null && branches.length === 0 ? <option value="">{t("research.authoring.branch.none")}</option> : null}
          {(branches ?? []).map((branch) => (
            <option key={branch.id} value={branch.id} data-authoring-branch-option={branch.name}>
              {branch.is_main ? t("research.authoring.branch.mainOption", { name: branch.name }) : branch.name}
            </option>
          ))}
        </select>
        {canWrite ? (
          <button
            type="button"
            className="research-authoring-button"
            data-authoring-new-branch-toggle
            aria-expanded={showBranchForm}
            onClick={() => setShowBranchForm((open) => !open)}
          >
            {t("research.authoring.branch.new")}
          </button>
        ) : (
          <span className="research-authoring-note">{t("research.authoring.readOnly")}</span>
        )}
        {branchesError === "" ? null : <span className="research-authoring-error" data-authoring-branches-error>{branchesError}</span>}
      </div>

      {showBranchForm && canWrite ? (
        <form
          className="research-authoring-form research-branch-form"
          data-authoring-new-branch
          onSubmit={(event) => {
            event.preventDefault();
            createBranch();
          }}
        >
          <label>
            <span>{t("research.authoring.branch.name")}</span>
            <input
              type="text"
              value={branchName}
              onChange={(event) => setBranchName(event.target.value)}
              placeholder={t("research.authoring.branch.namePlaceholder")}
              data-authoring-branch-name
              required
            />
          </label>
          <label>
            <span>{t("research.authoring.branch.purpose")}</span>
            <input
              type="text"
              value={branchPurpose}
              onChange={(event) => setBranchPurpose(event.target.value)}
              data-authoring-branch-purpose
            />
          </label>
          <div className="research-authoring-actions">
            <button type="submit" className="research-authoring-button primary" disabled={creatingBranch || branchName.trim() === ""} data-authoring-branch-submit>
              {creatingBranch ? t("research.authoring.branch.creating") : t("research.authoring.branch.create")}
            </button>
            <p className="research-authoring-note">{t("research.authoring.branch.hint")}</p>
          </div>
          {branchError === "" ? null : <p className="research-authoring-error" data-authoring-branch-error>{branchError}</p>}
        </form>
      ) : null}

      <div className="research-authoring-grid">
        <section aria-label={t("research.authoring.inventory.label")} data-authoring-inventory>
          <h4>{branchNameForDisplay === "" ? t("research.authoring.inventory.label") : t("research.authoring.inventory.title", { name: branchNameForDisplay })}</h4>
          {sliceError === "" ? null : <p className="research-authoring-error" data-authoring-inventory-error>{sliceError}</p>}
          {currentSlice === null && sliceError === "" ? (
            <Spinner aria-label={t("research.loading")} />
          ) : objects.length === 0 ? (
            <p className="research-authoring-note" data-authoring-inventory-empty>{t("research.authoring.inventory.empty")}</p>
          ) : (
            <ul className="research-authoring-objects" data-authoring-objects>
              {objects.map((object) => (
                <li key={object.id} data-authoring-object={object.id} data-object-type={object.object_type}>
                  <span className={`research-pill type-${object.object_type}`}>{typeLabel(t, object.object_type)}</span>
                  <strong>{object.title}</strong>
                  <span className="research-authoring-version" data-authoring-object-version={object.version_no}>
                    {t("research.authoring.object.version", { version: object.version_no })}
                  </span>
                  {outcomeOf(object.payload) === "" ? null : (
                    <span
                      className={`research-authoring-outcome ${outcomeOf(object.payload) === "failed" ? "failed" : ""}`}
                      data-authoring-object-outcome={outcomeOf(object.payload)}
                    >
                      {statusLabel(t, outcomeOf(object.payload))}
                    </span>
                  )}
                  <Link
                    href={branchObjectHref(projectId, object.id, branchId)}
                    className="research-authoring-open"
                    data-authoring-object-open={object.id}
                  >
                    {t("research.authoring.object.open")}
                  </Link>
                </li>
              ))}
            </ul>
          )}
        </section>

        {canWrite ? (
          <section aria-label={t("research.authoring.form.label")} data-authoring-form-host>
            <h4>{t("research.authoring.form.title")}</h4>
            <p className="research-authoring-note">{t("research.authoring.form.note")}</p>
            <div className="research-authoring-kinds" role="group" aria-label={t("research.authoring.form.kindLabel")}>
              {AUTHORING_KINDS.map((candidate) => (
                <button
                  key={candidate}
                  type="button"
                  className="research-authoring-kind"
                  aria-pressed={kind === candidate}
                  data-authoring-kind={candidate}
                  onClick={() => switchKind(candidate)}
                >
                  {typeLabel(t, candidate)}
                </button>
              ))}
            </div>
            <form
              className="research-authoring-form"
              data-authoring-form={kind}
              onSubmit={(event) => {
                event.preventDefault();
                saveDraft();
              }}
            >
              {fieldsFor(kind).map((field) => (
                <Field
                  key={field.name}
                  field={field}
                  value={values[field.name] ?? ""}
                  invalid={invalidFields.includes(field.name)}
                  requiredForReview={reviewRequiredFields(kind).includes(field.name)}
                  targets={field.targetType === undefined ? [] : targetObjects(field.targetType)}
                  onChange={(next) => setValues((current) => ({ ...current, [field.name]: next }))}
                />
              ))}
              <div className="research-authoring-actions">
                <button
                  type="submit"
                  className="research-authoring-button primary"
                  disabled={saving || !canSaveDraft(kind, values) || branchId === ""}
                  data-authoring-save={kind}
                >
                  {saving ? t("research.authoring.saving") : t("research.authoring.saveDraft")}
                </button>
                <p className="research-authoring-note">
                  {t("research.authoring.draftHint", { field: t(fieldLabelKey(kind, identityField(kind))) })}
                </p>
              </div>
              {invalidFields.length === 0 ? null : (
                <p className="research-authoring-error" data-authoring-invalid>
                  {t("research.authoring.invalidJson", { fields: invalidFields.map(humanizeOption).join(", ") })}
                </p>
              )}
              {saveError === "" ? null : (
                <p className="research-authoring-error" data-authoring-save-error>{saveError}</p>
              )}
              {saved === null ? null : (
                <p className="research-authoring-saved" data-authoring-saved={saved.objectId}>
                  {t("research.authoring.saved", { version: saved.versionNo })}{" "}
                  <Link href={branchObjectHref(projectId, saved.objectId, branchId)} data-authoring-open-draft={saved.objectId}>
                    {t("research.authoring.openDraft")}
                  </Link>
                </p>
              )}
            </form>
          </section>
        ) : null}
      </div>
    </section>
  );
}

/** The type's catalog label, or its own code humanized for a type this
 *  build predates (data is shown as it arrived). */
function typeLabel(t: ReturnType<typeof useT>, objectType: string): string {
  const key = objectTypeKey(objectType);
  return key === null ? objectType.replaceAll("_", " ") : t(key);
}

function fieldLabelKey(kind: AuthoringKind, fieldName: string): string {
  const field = fieldsFor(kind).find((candidate) => candidate.name === fieldName);
  return field?.labelKey ?? "research.authoring.field.statement";
}

/** The outcome code's catalog key when the catalog has one, else the code
 *  humanized — a code this build predates is data, shown as it arrived. */
function statusLabel(t: ReturnType<typeof useT>, code: string): string {
  const key = outcomeKey(code);
  return key === null ? code.replaceAll("_", " ") : t(key);
}

function Field({
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
  const label = (
    <span>
      {t(field.labelKey)}
      {requiredForReview ? (
        <span className="research-authoring-required" title={t("research.authoring.requiredForReview")}>
          {" "}
          *
        </span>
      ) : null}
    </span>
  );
  const shared = {
    "data-authoring-input": field.name,
    "aria-invalid": invalid ? true : undefined,
  } as const;

  if (field.control === "textarea") {
    return (
      <label className={invalid ? "invalid" : undefined}>
        {label}
        <textarea rows={3} value={value} onChange={(event) => onChange(event.target.value)} {...shared} />
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
    return (
      <label className={invalid ? "invalid" : undefined}>
        {label}
        <select value={value} onChange={(event) => onChange(event.target.value)} {...shared}>
          <option value="">{t("research.authoring.target.choose")}</option>
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
  if (field.control === "json") {
    return (
      <label className={invalid ? "invalid" : undefined}>
        {label}
        <textarea
          rows={2}
          value={value}
          placeholder={t("research.authoring.field.jsonHint")}
          onChange={(event) => onChange(event.target.value)}
          {...shared}
        />
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
