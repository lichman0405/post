"use client";

import { Suspense, useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useSearchParams } from "next/navigation";
import { AlertIcon, CheckIcon, GitPullRequestIcon } from "@primer/octicons-react";
import { Spinner } from "@primer/react";

import { useT } from "../../../../i18n-provider";

import {
  ApiError,
  RESOLUTION_KINDS,
  createConflictsClient,
  messageForConflictCode,
  type ConflictDetail,
  type ConflictTriple,
  type ConflictView,
  type DecisionInput,
  type EvidenceItem,
  type ObjectChange,
  type ObjectEvidence,
  type ObjectVersion,
  type RelationChange,
  type RelationVersion,
  type ResolutionKind,
  type ResolutionRecord,
} from "../../../../../lib/conflicts";
import { Diff, StateLabel, Table } from "@post/ui";
import type { DiffSide } from "@post/ui";
import { useProjectShell } from "../shell-context";
import "./conflicts.css";

/**
 * Conflicts page (T0407): the Scientific Conflict Resolution UI over the
 * GET conflicts / PUT resolutions surface (cmd/api/conflicthttp). It is
 * deep-linked from a pull request with the three states of the three-way
 * comparison in the query — base_state_id, source_state_id,
 * target_state_id — so the page has no state picker of its own.
 *
 * What the page does: shows, per classified conflict, the three-way
 * context (the base value, the source = A value, the target = B value),
 * the per-side evidence context, the detector's explanation — labeled
 * 仅建议 (advisory only, docs/09 §7: the agent resolves structural
 * conflicts, humans resolve scientific conflicts; the machine never
 * decides for the human) — and the explicit decision form with exactly
 * five options: Accept A (source) / Accept B (target) / Keep both
 * versions / Create validation branch / Keep unresolved. Nothing is
 * recorded until the human presses Save; nothing on the page computes a
 * merged or averaged value (docs/09 §7: Protocol 冲突数值折中禁止自动) —
 * the decision kinds are the exhaustive wire vocabulary, and the API
 * refuses anything else.
 *
 * Authorization truth stays in the API's answers: the shell already
 * answered the existence-hiding 404, and a caller without the write role
 * sees the save refusal (403) rendered inline, never guessed client-side.
 */

/** The five decision options, in the docs/09 §8 order. */
const OPTIONS: { kind: ResolutionKind; label: string; hint: string }[] = [
  { kind: "accept_source", label: "Accept A (source)", hint: "Take the source branch's value." },
  { kind: "accept_target", label: "Accept B (target)", hint: "Take the target branch's value." },
  { kind: "keep_both", label: "Keep both versions", hint: "Both values stay, recorded separately." },
  {
    kind: "validation_branch",
    label: "Create validation branch",
    hint: "Keep both and fork a branch to validate — name it in the note.",
  },
  { kind: "unresolved", label: "Keep unresolved", hint: "Record that this stays contested for now." },
];

export default function ConflictsPage() {
  // useSearchParams requires a Suspense boundary during prerender;
  // the inner component owns the triple read.
  return (
    <Suspense
      fallback={
        <div className="conflicts-state">
          <Spinner aria-label="Loading conflicts" />
        </div>
      }
    >
      <ConflictsPageInner />
    </Suspense>
  );
}

function ConflictsPageInner() {
  const shell = useProjectShell();
  const searchParams = useSearchParams();
  const client = useMemo(
    () => (shell === null ? null : createConflictsClient(shell.apiBaseUrl)),
    [shell],
  );
  const projectId = shell?.project.id ?? null;

  // The triple pins the comparison; every read and write carries it.
  const triple: ConflictTriple | null = useMemo(() => {
    const base = searchParams.get("base_state_id");
    const source = searchParams.get("source_state_id");
    const target = searchParams.get("target_state_id");
    if (base === null || source === null || target === null) return null;
    if (base === "" || source === "" || target === "") return null;
    return { base_state_id: base, source_state_id: source, target_state_id: target };
  }, [searchParams]);

  const [view, setView] = useState<ConflictView | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);

  // All conflicts state belongs to one (project, triple); when the shell
  // or the query hands us a different one, reset it during render — the
  // sanctioned alternative to setState-in-effect — so the previous
  // comparison can never flash into the new one.
  const scopeKey =
    projectId === null || triple === null
      ? null
      : `${projectId}|${triple.base_state_id}|${triple.source_state_id}|${triple.target_state_id}`;
  const [loadedFor, setLoadedFor] = useState<string | null>(null);
  if (scopeKey !== null && loadedFor !== scopeKey) {
    setLoadedFor(scopeKey);
    setView(null);
    setLoadError(null);
  }

  // The latest request wins; a stale answer (an old triple's in-flight
  // read resolving after a navigation) is dropped by the sequence.
  const loadSeq = useRef(0);
  const load = useCallback(() => {
    if (client === null || projectId === null || triple === null) return;
    const seq = ++loadSeq.current;
    client
      .get(projectId, triple)
      .then((next) => {
        if (seq === loadSeq.current) setView(next);
      })
      .catch((err: unknown) => {
        if (seq !== loadSeq.current) return;
        setLoadError(
          err instanceof ApiError ? messageForConflictCode(err.code) : "Could not load conflicts.",
        );
      });
  }, [client, projectId, triple]);

  useEffect(() => {
    load();
  }, [load]);

  if (shell === null || client === null || projectId === null) {
    return null;
  }

  if (triple === null) {
    return (
      <div className="conflicts-page" data-conflicts-page>
        <div className="conflicts-state" data-conflicts-missing>
          <div className="conflicts-state-icon" aria-hidden="true">
            <GitPullRequestIcon size={24} />
          </div>
          <h2 className="conflicts-state-title">Conflicts need a comparison</h2>
          <p className="conflicts-state-desc">
            This page compares three research states. Open it from a pull request, which
            supplies base_state_id, source_state_id and target_state_id.
          </p>
        </div>
      </div>
    );
  }

  return (
    <div className="conflicts-page" data-conflicts-page>
      <header className="conflicts-header">
        <h2 className="conflicts-title">
          <AlertIcon size={16} aria-hidden="true" /> Conflicts
        </h2>
        <span className="conflicts-triple" data-conflicts-triple>
          base <code>{short(triple.base_state_id)}</code> ← source{" "}
          <code>{short(triple.source_state_id)}</code> (A) · target{" "}
          <code>{short(triple.target_state_id)}</code> (B)
        </span>
      </header>

      {loadError !== null ? (
        <div className="conflicts-error" data-conflicts-error>
          <p>{loadError}</p>
          <button
            type="button"
            className="conflicts-retry"
            onClick={() => {
              setLoadError(null);
              load();
            }}
          >
            Try again
          </button>
        </div>
      ) : null}

      {view === null && loadError === null ? (
        <div className="conflicts-state">
          <Spinner aria-label="Loading conflicts" />
        </div>
      ) : null}

      {view !== null ? (
        <ConflictsBody
          view={view}
          onPlan={(plan) =>
            setView((prev) => (prev === null ? prev : { ...prev, resolutions: plan }))
          }
          onSave={(decision) => client.save(projectId, triple, [decision])}
        />
      ) : null}
    </div>
  );
}

/** The loaded view: summary, then one card per classified conflict. */
function ConflictsBody({
  view,
  onSave,
  onPlan,
}: {
  view: ConflictView;
  onSave: (decision: DecisionInput) => Promise<ResolutionRecord[]>;
  onPlan: (plan: ResolutionRecord[]) => void;
}) {
  const report = view.report;
  const conflicted =
    report.summary.objects_conflicted + report.summary.relations_conflicted;
  const totalConflicts =
    report.object_verdicts.reduce((n, v) => n + v.conflicts.length, 0) +
    report.relation_verdicts.reduce((n, v) => n + v.conflicts.length, 0);

  const objectChange = (id: string): ObjectChange | null =>
    report.diff.object_changes.find((c) => c.object_id === id) ?? null;
  const relationChange = (id: string): RelationChange | null =>
    report.diff.relation_changes.find((c) => c.relation_id === id) ?? null;
  const evidence = (id: string): ObjectEvidence | null =>
    view.evidence.find((e) => e.object_id === id) ?? null;

  return (
    <div className="conflicts-body">
      <section className="conflicts-summary" data-conflicts-summary>
        <p className="conflicts-summary-line">
          {report.auto_mergeable ? (
            <StateLabel shape="chip" tone="success" icon={CheckIcon} data-conflicts-clean>
              Nothing needs a human decision
            </StateLabel>
          ) : (
            <StateLabel shape="chip" tone="attention" data-conflicts-total>
              {totalConflicts} conflict{totalConflicts === 1 ? "" : "s"} need
              {totalConflicts === 1 ? "s" : ""} a human decision
            </StateLabel>
          )}
          <span className="conflicts-summary-muted">
            {conflicted} conflicted change{conflicted === 1 ? "" : "s"} across{" "}
            {report.summary.conflicts_by_category
              .map((c) => `${c.category}×${c.count}`)
              .join(", ")}
          </span>
        </p>
      </section>

      {totalConflicts === 0 ? (
        <p className="conflicts-empty" data-conflicts-empty>
          The detector found no conflict for this triple — every change merges without a human
          decision.
        </p>
      ) : null}

      <ul className="conflicts-list">
        {report.object_verdicts.flatMap((verdict) =>
          verdict.conflicts.map((conflict, i) => (
            <ConflictCard
              key={`object-${verdict.object_id}-${conflict.code}-${i}`}
              targetKind="object"
              targetId={verdict.object_id}
              change={objectChange(verdict.object_id)}
              conflict={conflict}
              evidence={evidence(verdict.object_id)}
              record={findRecord(view.resolutions, "object", verdict.object_id, conflict)}
              onSave={onSave}
              onPlan={onPlan}
            />
          )),
        )}
        {report.relation_verdicts.flatMap((verdict) =>
          verdict.conflicts.map((conflict, i) => (
            <ConflictCard
              key={`relation-${verdict.relation_id}-${conflict.code}-${i}`}
              targetKind="relation"
              targetId={verdict.relation_id}
              change={relationChange(verdict.relation_id)}
              conflict={conflict}
              evidence={null}
              record={findRecord(view.resolutions, "relation", verdict.relation_id, conflict)}
              onSave={onSave}
              onPlan={onPlan}
            />
          )),
        )}
      </ul>
    </div>
  );
}

/** One conflict's card: context, evidence, advisory explanation, decision. */
function ConflictCard({
  targetKind,
  targetId,
  change,
  conflict,
  evidence,
  record,
  onSave,
  onPlan,
}: {
  targetKind: "object" | "relation";
  targetId: string;
  change: ObjectChange | RelationChange | null;
  conflict: ConflictDetail;
  evidence: ObjectEvidence | null;
  record: ResolutionRecord | null;
  onSave: (decision: DecisionInput) => Promise<ResolutionRecord[]>;
  onPlan: (plan: ResolutionRecord[]) => void;
}) {
  const t = useT();
  const initialKind =
    record !== null && (RESOLUTION_KINDS as readonly string[]).includes(record.kind)
      ? (record.kind as ResolutionKind)
      : null;
  const [selected, setSelected] = useState<ResolutionKind | null>(initialKind);
  const [note, setNote] = useState(record?.note ?? "");
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);
  const [saved, setSaved] = useState<ResolutionRecord | null>(record);

  const decide = async () => {
    if (selected === null || saving) return;
    setSaving(true);
    setSaveError(null);
    try {
      const plan = await onSave({
        target_kind: targetKind,
        target_id: targetId,
        code: conflict.code,
        fields: conflict.fields,
        payload_keys: conflict.payload_keys,
        other_object_id: conflict.other_object_id === "" ? null : conflict.other_object_id,
        kind: selected,
        note,
      });
      onPlan(plan);
      setSaved(
        plan.find((r) => r.target_kind === targetKind && r.target_id === targetId && r.code === conflict.code) ?? null,
      );
    } catch (err: unknown) {
      setSaveError(
        err instanceof ApiError ? messageForConflictCode(err.code) : "Could not save the decision.",
      );
    } finally {
      setSaving(false);
    }
  };

  return (
    <li
      className="conflicts-card"
      data-conflict-card={targetId}
      data-conflict-code={conflict.code}
    >
      <header className="conflicts-card-header">
        <span className="conflicts-category" data-conflict-category={conflict.category}>
          {conflict.category}
        </span>
        <code className="conflicts-code">{conflict.code}</code>
        <span className="conflicts-target">
          {targetKind === "object" ? "object" : "relation"}{" "}
          <code title={targetId}>{short(targetId)}</code>
        </span>
        {/* T1104: recording a resolution is a write; the chip that appears in
            the card header is its only success report. Polite — the reader
            just asked for it, and nothing else on the page is disrupted. */}
        {saved !== null ? (
          <StateLabel shape="chip" tone="success" icon={CheckIcon} data-conflict-saved role="status">
            {saved.kind}
          </StateLabel>
        ) : null}
      </header>

      {/* T1105: this block is the ONE place in apps/web that rendered
          hardcoded Chinese ("仅建议 · advisory only"), and the task named it
          as the string that had to enter the catalog. It is a LABEL, not a
          domain code: `data-conflict-advisory-badge` next to it is the
          stable anchor and stays exactly as it is. The rest of this page's
          copy is still literal — see the coverage table in the RESULT, which
          lists this route as partially converted rather than claiming
          otherwise. */}
      <section className="conflicts-explanation" data-conflict-advisory>
        <p className="conflicts-explanation-label">
          {t("conflicts.detectorExplanation")}{" "}
          <StateLabel shape="advisory" tone="attention" data-conflict-advisory-badge>
            {t("conflicts.advisoryOnly")}
          </StateLabel>
        </p>
        <p className="conflicts-detail">{conflict.detail}</p>
        <p className="conflicts-advisory-note">
          {t("conflicts.advisoryNote")}
        </p>
      </section>

      {change !== null ? (
        <ThreeWay change={change} conflict={conflict} />
      ) : (
        <p className="conflicts-hint">
          No three-way change carries this conflict — re-run the comparison.
        </p>
      )}

      <EvidenceContext evidence={evidence} />

      <form
        className="conflicts-form"
        onSubmit={(e) => {
          e.preventDefault();
          void decide();
        }}
      >
        <fieldset className="conflicts-options" disabled={saving}>
          <legend className="conflicts-options-title">Your decision</legend>
          {OPTIONS.map((option) => (
            <label
              key={option.kind}
              className="conflicts-option"
              data-conflicts-option={option.kind}
            >
              <input
                type="radio"
                name={`resolution-${targetKind}-${targetId}-${conflict.code}`}
                value={option.kind}
                data-resolution-kind={option.kind}
                checked={selected === option.kind}
                onChange={() => setSelected(option.kind)}
              />
              <span className="conflicts-option-label">{option.label}</span>
              <span className="conflicts-option-hint">{option.hint}</span>
            </label>
          ))}
        </fieldset>
        <label className="conflicts-note">
          <span className="conflicts-note-label">Note</span>
          <textarea
            value={note}
            data-conflict-note
            rows={2}
            placeholder="Reasoning, or the validation branch name (optional)"
            onChange={(e) => setNote(e.target.value)}
          />
        </label>
        <div className="conflicts-actions">
          <button
            type="submit"
            className="conflicts-save"
            data-conflict-save
            disabled={selected === null || saving}
          >
            {saving ? "Saving…" : "Save decision"}
          </button>
          {/* T1104: the failure half of the same write; alert, because the
              reader's decision did not land and the chip above never comes. */}
          {saveError !== null ? (
            <p className="conflicts-save-error" data-conflict-save-error role="alert">
              {saveError}
            </p>
          ) : null}
        </div>
      </form>
    </li>
  );
}

/** The three-way context table: base, source (A), target (B). */
function ThreeWay({
  change,
  conflict,
}: {
  change: ObjectChange | RelationChange;
  conflict: ConflictDetail;
}) {
  // One row per diverged field; a payload conflict with payload_keys
  // renders one row per diverged key (the diverging values are the
  // scientific content the human is deciding about).
  const rows: { label: string; base: string; source: string; target: string }[] = [];
  for (const field of conflict.fields) {
    if (field === "payload" && conflict.payload_keys.length > 0) {
      for (const key of conflict.payload_keys) {
        rows.push({
          label: `payload.${key}`,
          base: payloadValue(baseOf(change), key),
          source: payloadValue(sourceOf(change), key),
          target: payloadValue(targetOf(change), key),
        });
      }
    } else {
      rows.push({
        label: field,
        base: fieldValue(baseOf(change), field),
        source: fieldValue(sourceOf(change), field),
        target: fieldValue(targetOf(change), field),
      });
    }
  }
  if (rows.length === 0) {
    // An identity conflict: the source created the object; the target has
    // the suspected duplicate the detector paired it with.
    const source = sourceOf(change);
    return (
      <div className="conflicts-threeway" data-conflicts-threeway>
        <p className="conflicts-identity-line">
          Created on the source side:{" "}
          <code>{source !== null ? versionTitle(source) : targetIdOf(change)}</code>
          {conflict.other_object_id !== "" ? (
            <>
              {" "}
              · suspected duplicate on the target side:{" "}
              <code title={conflict.other_object_id}>{short(conflict.other_object_id)}</code>
            </>
          ) : null}
        </p>
      </div>
    );
  }
  return (
    <div className="conflicts-threeway" data-conflicts-threeway>
      <Table
        borders="grid"
        columns={[
          { key: "field", header: "Field", rowHeader: true, render: (row) => row.label },
          {
            key: "base",
            header: "Base",
            render: (row) => <pre>{row.base}</pre>,
            cellAttrs: () => ({ "data-side": "base" }),
          },
          {
            key: "source",
            header: "Source (A)",
            render: (row) => <pre>{row.source}</pre>,
            cellAttrs: () => ({ "data-side": "source" }),
          },
          {
            key: "target",
            header: "Target (B)",
            render: (row) => <pre>{row.target}</pre>,
            cellAttrs: () => ({ "data-side": "target" }),
          },
        ]}
        rows={rows}
        rowKey={(row) => row.label}
      />
    </div>
  );
}

/** The per-side evidence context of one conflicted object. */
function EvidenceContext({ evidence }: { evidence: ObjectEvidence | null }) {
  if (evidence === null) {
    return null;
  }
  return (
    <section className="conflicts-evidence" data-conflicts-evidence>
      <h3 className="conflicts-evidence-title">Evidence context</h3>
      <Diff
        sides={[
          evidenceSide("Source (A)", evidence.source_evidence),
          evidenceSide("Target (B)", evidence.target_evidence),
        ]}
      />
    </section>
  );
}

/**
 * One side of the evidence comparison, in the shared Diff shape (T1101).
 *
 * The two columns of this section used to be `.conflicts-evidence-column`
 * in conflicts.css — a bordered panel with a heading and a list of items
 * whose title carries a type mark and whose meta line carries the relation
 * facts. That is `Diff`'s `sides` layout; what stays here is the mapping
 * from an evidence item to an entry.
 */
function evidenceSide(side: string, items: EvidenceItem[]): DiffSide {
  return {
    key: side,
    heading: side,
    attrs: { "data-evidence-side": side },
    empty: "No evidence recorded on this side.",
    entries: items.map((item, i) => ({
      key: `${item.evidence_object_id}-${i}`,
      title: (
        <>
          {item.evidence_title}{" "}
          <span className="conflicts-evidence-item-type">{item.evidence_object_type}</span>
        </>
      ),
      meta: (
        <>
          {item.relation_type} · {item.directness} · {item.review_state}
          {item.reasoning_note !== null && item.reasoning_note !== ""
            ? ` · ${item.reasoning_note}`
            : ""}
        </>
      ),
    })),
  };
}

/* ---------- three-way helpers ---------- */

function baseOf(change: ObjectChange | RelationChange): ObjectVersion | RelationVersion | null {
  return change.base_version;
}

function sourceOf(change: ObjectChange | RelationChange): ObjectVersion | RelationVersion {
  return change.source_version;
}

function targetOf(change: ObjectChange | RelationChange): ObjectVersion | RelationVersion | null {
  return change.target_version;
}

function targetIdOf(change: ObjectChange | RelationChange): string {
  return "object_id" in change ? change.object_id : change.relation_id;
}

/** The displayed title of one version (objects have titles, relations types). */
function versionTitle(version: ObjectVersion | RelationVersion): string {
  return "title" in version ? version.title : version.relation_type;
}

/** The value of one diverged field on one side; "—" when the side has none. */
function fieldValue(
  version: ObjectVersion | RelationVersion | null,
  field: string,
): string {
  if (version === null) return "—";
  if ("title" in version) {
    switch (field) {
      case "payload":
        return JSON.stringify(version.payload, null, 2);
      case "title":
        return version.title;
      case "lifecycle_state":
        return version.lifecycle_state;
      case "schema_ref":
        return `${version.schema_ref.id}@${version.schema_ref.version}`;
      case "version_no":
        return String(version.version_no);
      default:
        return "—";
    }
  }
  switch (field) {
    case "payload":
      return JSON.stringify(version.payload, null, 2);
    case "relation_type":
      return version.relation_type;
    case "source_object_version_id":
      return version.source_object_version_id;
    case "target_object_version_id":
      return version.target_object_version_id;
    case "version_no":
      return String(version.version_no);
    default:
      return "—";
  }
}

/** The value of one payload key on one side; "—" when absent there. */
function payloadValue(
  version: ObjectVersion | RelationVersion | null,
  key: string,
): string {
  if (version === null) return "—";
  const payload = version.payload;
  if (payload === null || typeof payload !== "object" || !(key in payload)) return "—";
  return JSON.stringify((payload as Record<string, unknown>)[key], null, 2);
}

/** The recorded decision matching one conflict, if any. */
function findRecord(
  resolutions: ResolutionRecord[],
  targetKind: "object" | "relation",
  targetId: string,
  conflict: ConflictDetail,
): ResolutionRecord | null {
  return (
    resolutions.find(
      (r) =>
        r.target_kind === targetKind &&
        r.target_id === targetId &&
        r.code === conflict.code &&
        join(r.fields) === join(conflict.fields) &&
        join(r.payload_keys) === join(conflict.payload_keys) &&
        (r.other_object_id ?? "") === conflict.other_object_id,
    ) ?? null
  );
}

/** The classifier-key join (mirrors the service's joinStrings). */
function join(items: string[]): string {
  return [...items].sort().join("\u0000");
}

/** The first 8 characters of an id, for dense rendering. */
function short(id: string): string {
  return id.slice(0, 8);
}
