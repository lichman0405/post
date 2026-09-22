"use client";

import { useEffect, useMemo, useRef, useState, type FormEvent } from "react";
import Link from "next/link";
import {
  AlertIcon,
  CheckCircleIcon,
  IssueOpenedIcon,
  ShieldCheckIcon,
  XCircleIcon,
} from "@primer/octicons-react";
import { Spinner } from "@primer/react";

import {
  ApiError,
  assessPullRisks,
  changeKindLabelKey,
  createPullsClient,
  DEFAULT_PULL_TAB,
  diffTotals,
  dimensionLabelKey,
  evidenceChanges,
  INTEGRITY_DIMENSIONS,
  isStaleReview,
  knowledgeChanges,
  latestHeadReview,
  pullRequestCodeKey,
  PULL_TABS,
  pullTabHintKey,
  pullTabLabelKey,
  rawPatchUrl,
  relationTypeOf,
  type CheckResult,
  type DiffDocument,
  type IntegrityReport,
  type ObjectChange,
  type PullRequest,
  type PullRisk,
  type PullTab,
  type RelationChange,
  type Review,
  type ReviewInput,
} from "../../../../../../lib/pulls";
import { translateOr } from "../../../../../../lib/i18n";
import { Diff } from "@post/ui";
import type { Tone } from "@post/ui";
import { useT } from "../../../../../i18n-provider";
import { useProjectShell } from "../../shell-context";
import "./pull-detail.css";

/**
 * Pull request detail (T0403, extended by T0408): the Research State Diff
 * of one proposal, with the machine integrity report and the review
 * controls.
 *
 * docs/06 §6 fixes the shape: the first screen is the semantic diff —
 * the scientific objects created/updated/aborted, the knowledge changes,
 * the evidence changes, the schema and visibility changes, the dependency
 * checks — and the RAW Git diff lives on a secondary tab, never on the
 * first screen. The summary is the default tab, so a reader who lands
 * here sees the semantic answer; the raw file view takes a deliberate
 * click.
 *
 * Two things the page refuses to do. It never softens a risk: the banner
 * above the tabs is computed from the API's own answers (the machine's
 * failing checks at their own severity, the target branch having moved the
 * same entries, aborts, schema and visibility changes, a
 * changes_requested decision on the current head) and is rendered
 * verbatim, blocking first. And it never flattens the review: docs/09 §5
 * keeps the scientific and integrity dimensions apart, so the summary
 * shows one line per dimension for the CURRENT head, marks decisions on
 * older heads as what they are, and the review form records exactly one
 * dimension's decision — the API decides whether the caller may submit
 * one, and its refusal renders inline.
 */

/** The two review dimensions, in docs/09 §5 order. */
const REVIEW_KINDS = ["scientific", "integrity"] as const;

/** The three decisions, in the domain's order. */
const REVIEW_DECISIONS = ["approved", "changes_requested", "comment"] as const;

export default function PullDetailPage({
  params,
}: {
  params: Promise<{ number: string }>;
}) {
  const shell = useProjectShell();
  const t = useT();

  // The route param arrives as a Promise, so the page has three states —
  // pending, invalid, ready — and never fewer: a single "null number"
  // state has to double for both "params has not settled yet" and "this
  // segment names no pull request", so the first frame rendered the
  // validation error before the number was even readable. Pending renders
  // the same loading spinner as a fetch in flight.
  const [routeNumber, setRouteNumber] = useState<
    { status: "pending" } | { status: "invalid" } | { status: "ready"; value: number }
  >({ status: "pending" });
  const number = routeNumber.status === "ready" ? routeNumber.value : null;
  // Loaded data is keyed on the PR number so a navigation between PRs
  // never shows the previous PR's content (no synchronous reset needed).
  const [loaded, setLoaded] = useState<{
    number: number;
    pr: PullRequest;
    report: IntegrityReport;
    diff: DiffDocument;
    reviews: Review[];
  } | null>(null);
  const [error, setError] = useState<{ number: number; text: string } | null>(null);
  const [tab, setTab] = useState<PullTab>(DEFAULT_PULL_TAB);
  const tabListRef = useRef<HTMLDivElement | null>(null);

  /* The WAI-ARIA tabs pattern: a tablist is one tab stop and the arrow keys
   * move between its tabs. `role="tablist"` was declared here from the start
   * but nothing implemented the keyboard half of the promise, so the tabs
   * were reachable only by Tab and a screen-reader user was told about a
   * widget that did not behave as announced. Activation follows focus
   * (automatic activation), which is what the APG prescribes when the panels
   * are already on the page — they are; switching a tab only changes which
   * panel is visible. */
  function onTabKeyDown(event: React.KeyboardEvent<HTMLButtonElement>, index: number) {
    const last = PULL_TABS.length - 1;
    let next = index;
    if (event.key === "ArrowRight") next = index === last ? 0 : index + 1;
    else if (event.key === "ArrowLeft") next = index === 0 ? last : index - 1;
    else if (event.key === "Home") next = 0;
    else if (event.key === "End") next = last;
    else return;
    event.preventDefault();
    const target = PULL_TABS[next];
    setTab(target);
    // The button is keyed by its tab name, so the same DOM node survives the
    // re-render and focus set here is not lost to it.
    tabListRef.current?.querySelector<HTMLButtonElement>(`#pull-tab-${target}`)?.focus();
  }

  // The review form: which dimension, which verdict, what reasoning. The
  // verdict starts empty so nothing can be submitted before the human
  // chooses one.
  const [reviewKind, setReviewKind] = useState<ReviewInput["kind"]>("scientific");
  const [reviewDecision, setReviewDecision] = useState<ReviewInput["decision"] | "">("");
  const [reviewBody, setReviewBody] = useState("");
  const [submitError, setSubmitError] = useState<{ number: number; text: string } | null>(null);
  const [submitting, setSubmitting] = useState(false);
  const [justRecorded, setJustRecorded] = useState<{ number: number; decision: string } | null>(null);

  const client = useMemo(
    () => (shell === null ? null : createPullsClient(shell.apiBaseUrl)),
    [shell],
  );

  // The route param arrives as a Promise; parse it once and re-fetch only
  // when the project or number actually changes. A segment that is not a
  // positive integer — or a params promise that does not settle — is the
  // invalid state, not a pending one.
  useEffect(() => {
    let cancelled = false;
    params
      .then((resolved) => {
        if (cancelled) return;
        const parsed = Number(resolved.number);
        setRouteNumber(
          Number.isInteger(parsed) && parsed > 0
            ? { status: "ready", value: parsed }
            : { status: "invalid" },
        );
      })
      .catch(() => {
        if (!cancelled) setRouteNumber({ status: "invalid" });
      });
    return () => {
      cancelled = true;
    };
  }, [params]);

  useEffect(() => {
    if (shell === null || client === null || number === null) return;
    let cancelled = false;
    Promise.all([
      client.get(shell.project.id, number),
      client.checks(shell.project.id, number),
      client.diff(shell.project.id, number),
      client.reviews(shell.project.id, number),
    ])
      .then(([pr, report, diff, reviews]) => {
        if (cancelled) return;
        setError(null);
        setTab(DEFAULT_PULL_TAB);
        setLoaded({ number, pr, report, diff, reviews });
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        setError({
          number,
          text:
            err instanceof ApiError
              ? t(pullRequestCodeKey(err.code))
              : t("pull.error.load"),
        });
      });
    return () => {
      cancelled = true;
    };
    // `t` in the deps: the fallback sentence is resolved in the catch.
  }, [shell, client, number, t]);

  /** Record one review decision; the API's answer is what the page shows. */
  async function submitReview(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (shell === null || client === null || number === null || reviewDecision === "") return;
    setSubmitting(true);
    setSubmitError(null);
    try {
      const recorded = await client.submitReview(shell.project.id, number, {
        kind: reviewKind,
        decision: reviewDecision,
        body: reviewBody,
      });
      setLoaded((prev) =>
        prev === null || prev.number !== number
          ? prev
          : { ...prev, reviews: [...prev.reviews, recorded] },
      );
      setJustRecorded({ number, decision: recorded.decision });
      setReviewDecision("");
      setReviewBody("");
    } catch (err: unknown) {
      setSubmitError({
        number,
        text:
          err instanceof ApiError
            ? t(pullRequestCodeKey(err.code))
            : t("pull.error.review"),
      });
    } finally {
      setSubmitting(false);
    }
  }

  if (shell === null) {
    // The shell only mounts tab content in its ready state; null means a
    // wiring error, not a user-visible page.
    return null;
  }

  if (routeNumber.status === "invalid") {
    return (
      <div className="pulls-page" data-pulls-detail data-pulls-invalid-number>
        <section className="pulls-section">
          <div className="pulls-error" data-pulls-error>
            {t(pullRequestCodeKey("VALIDATION_FAILED"))}
          </div>
          <p className="pulls-back">
            <Link href={`/projects/${shell.project.id}/pulls`}>
              {t("pull.back")}
            </Link>
          </p>
        </section>
      </div>
    );
  }

  const data = loaded !== null && loaded.number === number ? loaded : null;
  const errorText = error !== null && error.number === number ? error.text : null;

  if (errorText !== null) {
    return (
      <div className="pulls-page" data-pulls-detail data-pull-number={number ?? undefined}>
        <p className="pulls-back">
          <Link href={`/projects/${shell.project.id}/pulls`}>
            {t("pull.back")}
          </Link>
        </p>
        <section className="pulls-section">
          <div className="pulls-error" data-pulls-error>{errorText}</div>
        </section>
      </div>
    );
  }

  if (data === null) {
    return (
      <div className="pulls-page" data-pulls-detail data-pull-number={number ?? undefined}>
        <Spinner aria-label={t("pull.loading")} />
      </div>
    );
  }

  const risks = assessPullRisks({
    report: data.report,
    diff: data.diff,
    reviews: data.reviews,
    headStateId: data.pr.proposed_state_id,
  }, t);
  const knowledge = knowledgeChanges(data.diff);
  const evidence = evidenceChanges(data.diff);
  const totals = diffTotals(data.diff);
  const failingChecks = data.report.results.filter((r) => !r.passed).length;

  return (
    <div className="pulls-page" data-pulls-detail data-pull-number={data.pr.number}>
      <p className="pulls-back">
        <Link href={`/projects/${shell.project.id}/pulls`}>
          {t("pull.back")}
        </Link>
      </p>

      <section className="pulls-section" data-pull-header>
        <h2 className="pulls-detail-title">
          <span className="pulls-detail-number">#{data.pr.number}</span>{" "}
          {data.pr.title}
        </h2>
        <p className="pulls-detail-meta">
          <span className="pulls-state" data-pull-state={data.pr.state}>
            {data.pr.state}
          </span>
          <span>
            {t("pull.openedBy", {
              date: data.pr.created_at.slice(0, 10),
              actor: data.pr.created_by,
            })}
          </span>
        </p>
        {data.pr.body !== "" ? (
          <p className="pulls-detail-body">{data.pr.body}</p>
        ) : null}
        <dl className="pulls-pins">
          <dt>{t("pull.baseState")}</dt>
          <dd className="pulls-pin-id" data-pull-base-state>{data.pr.base_state_id}</dd>
          <dt>{t("pull.proposedState")}</dt>
          <dd className="pulls-pin-id" data-pull-head-state>{data.pr.proposed_state_id}</dd>
        </dl>
      </section>

      {/* The risks the API's own answers imply — blocking first, never
          softened, and always above the tabs so no tab hides them. */}
      {risks.length > 0 ? (
        <RiskBanner risks={risks} onOpen={setTab} />
      ) : null}

      <div
        className="pull-tabs"
        role="tablist"
        aria-label={t("pull.tabsLabel")}
        data-pull-tabs
        ref={tabListRef}
      >
        {PULL_TABS.map((each, index) => (
          <button
            key={each}
            type="button"
            role="tab"
            id={`pull-tab-${each}`}
            aria-selected={each === tab}
            aria-controls={`pull-panel-${each}`}
            /* Roving tabindex: the tablist is ONE tab stop (the selected
               tab). Without this every tab is its own stop, which is not
               what `role="tablist"` promises, and the arrow keys below
               would be the only way to reach the others. */
            tabIndex={each === tab ? 0 : -1}
            className={`pull-tab${each === tab ? " pull-tab-active" : ""}`}
            data-pull-tab={each}
            data-tab-selected={each === tab}
            onClick={() => setTab(each)}
            onKeyDown={(event) => onTabKeyDown(event, index)}
          >
            {t(pullTabLabelKey(each))}
            <span className="pull-tab-count" data-tab-count={each}>
              {tabCount(each, {
                objects: totals.objects,
                knowledge: knowledge.objects.length + knowledge.relations.length,
                evidence: evidence.objects.length + evidence.relations.length,
                failing: failingChecks,
                files: totals.files,
              })}
            </span>
          </button>
        ))}
      </div>

      <section
        className="pull-panel"
        role="tabpanel"
        id={`pull-panel-${tab}`}
        aria-labelledby={`pull-tab-${tab}`}
        data-pull-panel={tab}
        tabIndex={0}
      >
        <p className="pull-panel-hint">{t(pullTabHintKey(tab))}</p>

        {tab === "summary" ? (
          <SummaryPanel
            pr={data.pr}
            report={data.report}
            diff={data.diff}
            reviews={data.reviews}
            projectId={shell.project.id}
          />
        ) : null}

        {tab === "scientific" ? (
          <ScientificPanel diff={data.diff} />
        ) : null}

        {tab === "knowledge" ? (
          <ChangesPanel
            objects={knowledge.objects}
            relations={knowledge.relations}
            emptyText={t("pull.knowledge.empty")}
            objectAttr="data-knowledge-object"
            relationAttr="data-knowledge-relation"
          />
        ) : null}

        {tab === "evidence" ? (
          <ChangesPanel
            objects={evidence.objects}
            relations={evidence.relations}
            emptyText={t("pull.evidence.empty")}
            objectAttr="data-evidence-object"
            relationAttr="data-evidence-relation"
          />
        ) : null}

        {tab === "checks" ? <ChecksPanel report={data.report} /> : null}

        {tab === "raw" ? <RawFilesPanel diff={data.diff} /> : null}
      </section>

      {/* The review controls. They live below the tabs so they are one
          screen away from any tab, and they record ONE dimension's
          decision — the API decides whether this caller may. */}
      {tab === "summary" ? (
        <section className="pulls-section pull-review" data-pull-review>
          <h2 className="pulls-section-title">{t("pull.review.title")}</h2>
          <p className="pulls-section-desc">
            {t("pull.review.desc", { stateId: data.pr.proposed_state_id })}
          </p>
          <form className="pull-review-form" onSubmit={submitReview} data-review-form>
            <label className="pull-review-field">
              <span>{t("pull.review.dimension")}</span>
              <select
                value={reviewKind}
                data-review-kind
                onChange={(e) => setReviewKind(e.target.value as ReviewInput["kind"])}
              >
                {REVIEW_KINDS.map((kind) => (
                  <option key={kind} value={kind}>
                    {kind}
                  </option>
                ))}
              </select>
            </label>
            <label className="pull-review-field">
              <span>{t("pull.review.decision")}</span>
              <select
                value={reviewDecision}
                data-review-decision
                onChange={(e) =>
                  setReviewDecision(e.target.value as ReviewInput["decision"] | "")
                }
              >
                <option value="">{t("pull.review.choose")}</option>
                {REVIEW_DECISIONS.map((decision) => (
                  <option key={decision} value={decision}>
                    {decision}
                  </option>
                ))}
              </select>
            </label>
            <label className="pull-review-field pull-review-field-wide">
              <span>{t("pull.review.reasoning")}</span>
              <textarea
                value={reviewBody}
                data-review-body
                rows={3}
                onChange={(e) => setReviewBody(e.target.value)}
              />
            </label>
            <div className="pull-review-actions">
              <button
                type="submit"
                className="pull-review-submit"
                data-review-submit
                disabled={reviewDecision === "" || submitting}
              >
                {submitting ? t("pull.review.submitting") : t("pull.review.submit")}
              </button>
              {/* T1104: recording a review is a SUBMIT, and both of its
                  outcomes are silent markup otherwise — the form clears
                  itself on success (the review moves into the list below)
                  and the button stops saying "Recording…", so a
                  screen-reader reader gets no signal either way. Success is
                  polite status (nothing is broken, it just happened);
                  failure is an alert (the reader's action did not land). */}
              {justRecorded !== null && justRecorded.number === number ? (
                <span className="pull-review-saved" data-review-saved role="status">
                  {t("pull.review.recorded", { decision: justRecorded.decision })}
                </span>
              ) : null}
            </div>
          </form>
          {submitError !== null && submitError.number === number ? (
            <div className="pull-review-error" data-review-error role="alert">
              {submitError.text}
            </div>
          ) : null}
        </section>
      ) : null}
    </div>
  );
}

/** The count one tab shows, from what the diff and the report contain. */
function tabCount(
  tab: PullTab,
  counts: { objects: number; knowledge: number; evidence: number; failing: number; files: number },
): number {
  switch (tab) {
    case "summary":
      return counts.objects;
    case "scientific":
      return counts.objects;
    case "knowledge":
      return counts.knowledge;
    case "evidence":
      return counts.evidence;
    case "checks":
      return counts.failing;
    case "raw":
      return counts.files;
  }
}

/* ---------- The risk banner (docs/06 §6: 重要风险必须明显) ---------- */

function RiskBanner({ risks, onOpen }: { risks: PullRisk[]; onOpen: (tab: PullTab) => void }) {
  const t = useT();
  const blocking = risks.filter((r) => r.severity === "blocking").length;
  return (
    <section className="pull-risks" data-pull-risks data-risk-count={risks.length} data-risk-blocking={blocking}>
      <h2 className="pull-risks-title">
        <AlertIcon size={16} aria-hidden="true" />
        {blocking > 0
          ? t(blocking === 1 ? "pull.risk.blocking.one" : "pull.risk.blocking.many", { count: blocking })
          : t(risks.length === 1 ? "pull.risk.weigh.one" : "pull.risk.weigh.many", { count: risks.length })}
      </h2>
      <ul className="pull-risk-list">
        {risks.map((risk) => (
          <li
            key={`${risk.code}:${risk.title}`}
            className={`pull-risk pull-risk-${risk.severity}`}
            data-pull-risk={risk.code}
            data-risk-severity={risk.severity}
          >
            <div className="pull-risk-head">
              {risk.severity === "blocking" ? (
                <XCircleIcon size={14} aria-hidden="true" className="pull-risk-icon" />
              ) : (
                <IssueOpenedIcon size={14} aria-hidden="true" className="pull-risk-icon" />
              )}
              <span className="pull-risk-severity">{risk.severity}</span>
              <span className="pull-risk-title-text">{risk.title}</span>
            </div>
            <div className="pull-risk-detail">{risk.detail}</div>
            <button
              type="button"
              className="pull-risk-link"
              data-risk-tab={risk.tab}
              onClick={() => onOpen(risk.tab)}
            >
              {t("pull.risk.open", { tab: t(pullTabLabelKey(risk.tab)) })}
            </button>
          </li>
        ))}
      </ul>
    </section>
  );
}

/* ---------- Summary ---------- */

function SummaryPanel({
  pr,
  report,
  diff,
  reviews,
  projectId,
}: {
  pr: PullRequest;
  report: IntegrityReport;
  diff: DiffDocument;
  reviews: Review[];
  projectId: string;
}) {
  const t = useT();
  const earlier = reviews.filter((r) => isStaleReview(r, pr.proposed_state_id));
  const conflictsHref =
    `/projects/${projectId}/conflicts` +
    `?base_state_id=${encodeURIComponent(diff.base.id)}` +
    `&source_state_id=${encodeURIComponent(diff.source.id)}` +
    `&target_state_id=${encodeURIComponent(diff.target.id)}`;
  return (
    <>
      <div className="pull-summary-counts" data-pull-totals>
        {/* `code` is the stable identifier the DOM carries (data-count-label,
            which tests/e2e-pulls/pulls-e2e.mjs reads); `label` is the copy a
            reader sees. docs/28 §3 is the split: the code does not follow the
            language preference, the label does. */}
        <Count code="Objects created" label={t("pull.count.objectsCreated")} value={diff.summary.objects_created} />
        <Count code="Objects updated" label={t("pull.count.objectsUpdated")} value={diff.summary.objects_updated} />
        <Count code="Objects aborted" label={t("pull.count.objectsAborted")} value={diff.summary.objects_aborted} />
        <Count code="Objects reopened" label={t("pull.count.objectsReopened")} value={diff.summary.objects_reopened} />
        <Count code="Relations created" label={t("pull.count.relationsCreated")} value={diff.summary.relations_created} />
        <Count code="Relations updated" label={t("pull.count.relationsUpdated")} value={diff.summary.relations_updated} />
      </div>

      <div className="pull-summary-states" data-pull-states>
        <StateLine role="Base (pinned)" label={t("pull.state.basePinned")} ref_={diff.base} />
        <StateLine role="Proposed" label={t("pull.state.proposed")} ref_={diff.source} />
        <StateLine role="Target (branch head)" label={t("pull.state.targetHead")} ref_={diff.target} />
      </div>

      <h3 className="pull-subtitle">{t("pull.reviewState")}</h3>
      <div className="pull-review-state" data-pull-review-state>
        {REVIEW_KINDS.map((kind) => {
          const review = latestHeadReview(reviews, kind, pr.proposed_state_id);
          return (
            <div
              className="pull-review-line"
              key={kind}
              data-review-state-dimension={kind}
              data-review-state-decision={review === null ? "none" : review.decision}
            >
              <span className="pull-review-kind">{kind}</span>
              {review === null ? (
                <span className="pull-review-none">
                  {t("pull.review.none", { kind })}
                </span>
              ) : (
                <span className="pull-review-recorded">
                  <strong>{review.decision}</strong>
                  {review.responsibility !== "" ? ` · ${review.responsibility}` : ""}
                  {` · ${review.reviewer_id.slice(0, 8)} · ${review.created_at.slice(0, 10)}`}
                </span>
              )}
            </div>
          );
        })}
      </div>

      <p className="pull-summary-line" data-pull-integrity-verdict={report.verdict}>
        <ShieldCheckIcon size={14} aria-hidden="true" /> {t("pull.integrityVerdict")}{" "}
        <strong>{report.verdict}</strong>
      </p>

      {earlier.length > 0 ? (
        <div className="pull-earlier" data-pull-earlier-reviews>
          <h3 className="pull-subtitle">{t("pull.earlier.title")}</h3>
          <ul className="pull-earlier-list">
            {earlier.map((review) => (
              <li key={review.id} data-earlier-review={review.id}>
                {review.kind} · {review.decision} · {t("pull.earlier.judged")}{" "}
                <code>{review.reviewed_state_id.slice(0, 8)}</code>
                {review.reviewed_state_id !== pr.proposed_state_id
                  ? t("pull.earlier.notHead")
                  : ""}
              </li>
            ))}
          </ul>
        </div>
      ) : null}

      <p className="pull-summary-line">
        <Link href={conflictsHref} data-pull-conflicts-link>
          {t("pull.openConflicts")}
        </Link>
      </p>
    </>
  );
}

function Count({ code, label, value }: { code: string; label: string; value: number }) {
  return (
    <div className="pull-count" data-count-label={code}>
      <span className="pull-count-value">{value}</span>
      <span className="pull-count-label">{label}</span>
    </div>
  );
}

/** One side of the comparison. `role` is the stable code the DOM carries
 *  (data-state-role); `label` is the copy a reader sees. */
function StateLine({
  role,
  label,
  ref_,
}: {
  role: string;
  label: string;
  ref_: { id: string; git_ref: string | null };
}) {
  const t = useT();
  return (
    <div className="pull-summary-state" data-state-role={role}>
      <span className="pull-summary-state-role">{label}</span>
      <code className="pulls-pin-id">{ref_.id}</code>
      <span className="pull-summary-state-git">
        {ref_.git_ref === null || ref_.git_ref === "" ? t("pull.noGitRef") : ref_.git_ref.slice(0, 12)}
      </span>
    </div>
  );
}

/* ---------- Scientific / knowledge / evidence change lists ---------- */

function ScientificPanel({ diff }: { diff: DiffDocument }) {
  const t = useT();
  return (
    <>
      <h3 className="pull-subtitle">{t("pull.scientific.objects")}</h3>
      <ObjectList
        changes={diff.object_changes}
        emptyText={t("pull.scientific.emptyObjects")}
        attr="data-scientific-object"
      />
      <h3 className="pull-subtitle">{t("pull.scientific.relations")}</h3>
      <RelationList
        changes={diff.relation_changes}
        emptyText={t("pull.scientific.emptyRelations")}
        attr="data-scientific-relation"
      />
    </>
  );
}

function ChangesPanel({
  objects,
  relations,
  emptyText,
  objectAttr,
  relationAttr,
}: {
  objects: ObjectChange[];
  relations: RelationChange[];
  emptyText: string;
  objectAttr: string;
  relationAttr: string;
}) {
  if (objects.length === 0 && relations.length === 0) {
    return (
      <p className="pull-empty" data-pull-bucket-empty>
        {emptyText}
      </p>
    );
  }
  return (
    <>
      {objects.length > 0 ? (
        <ObjectList changes={objects} emptyText={emptyText} attr={objectAttr} />
      ) : null}
      {relations.length > 0 ? (
        <RelationList changes={relations} emptyText={emptyText} attr={relationAttr} />
      ) : null}
    </>
  );
}

/**
 * The colour family of a change kind (T1101): a creation is good news, an
 * abort is a removal, and an update or a reopen is neither — it is a
 * change, and the row says so in words. This mapping used to be four
 * `.pull-change-*` rules in pull-detail.css; the chip is now the shared
 * Diff's, painted from the tone.
 */
function changeKindTone(kind: string): Tone {
  switch (kind) {
    case "created":
      return "success";
    case "aborted":
      return "danger";
    default:
      return "neutral";
  }
}

function ObjectList({
  changes,
  emptyText,
  attr,
}: {
  changes: ObjectChange[];
  emptyText: string;
  attr: string;
}) {
  const t = useT();
  if (changes.length === 0) {
    return <p className="pull-empty">{emptyText}</p>;
  }
  return (
    <Diff
      entries={changes.map((change) => ({
        key: change.object_id,
        kind: translateOr(t, changeKindLabelKey(change.kind), change.kind),
        tone: changeKindTone(change.kind),
        type: change.object_type,
        title: change.source_version.title || change.object_id,
        note: change.target_moved ? t("pull.change.moved") : undefined,
        noteAttrs: change.target_moved ? { "data-change-moved": true } : undefined,
        meta: (
          <>
            <span className="pull-change-id">{change.object_id}</span>
            <span className="pull-change-lifecycle">
              {change.source_version.lifecycle_state}
            </span>
          </>
        ),
        fields:
          change.changed_fields.length === 0
            ? t("pull.change.newObject")
            : t("pull.change.changed", { fields: change.changed_fields.join(", ") }),
        attrs: {
          [attr]: change.object_id,
          "data-change-kind": change.kind,
          "data-object-type": change.object_type,
          "data-target-moved": change.target_moved,
        },
      }))}
    />
  );
}

function RelationList({
  changes,
  emptyText,
  attr,
}: {
  changes: RelationChange[];
  emptyText: string;
  attr: string;
}) {
  const t = useT();
  if (changes.length === 0) {
    return <p className="pull-empty">{emptyText}</p>;
  }
  return (
    <Diff
      entries={changes.map((change) => ({
        key: change.relation_id,
        kind: translateOr(t, changeKindLabelKey(change.kind), change.kind),
        tone: changeKindTone(change.kind),
        type: relationTypeOf(change),
        title: (
          <>
            {change.source_version.source_object_version_id.slice(0, 8)} →{" "}
            {change.source_version.target_object_version_id.slice(0, 8)}
          </>
        ),
        note: change.target_moved ? t("pull.change.moved") : undefined,
        noteAttrs: change.target_moved ? { "data-change-moved": true } : undefined,
        meta: <span className="pull-change-id">{change.relation_id}</span>,
        fields:
          change.changed_fields.length === 0
            ? t("pull.change.newRelation")
            : t("pull.change.changed", { fields: change.changed_fields.join(", ") }),
        attrs: {
          [attr]: change.relation_id,
          "data-change-kind": change.kind,
          "data-relation-type": relationTypeOf(change),
          "data-target-moved": change.target_moved,
        },
      }))}
    />
  );
}

/* ---------- Checks ---------- */

function ChecksPanel({ report }: { report: IntegrityReport }) {
  const t = useT();
  return (
    <div data-pull-checks>
      <div
        className={`pulls-verdict pulls-verdict-${verdictClass(report.verdict)}`}
        data-verdict={report.verdict}
      >
        {verdictIcon(report.verdict)}
        <span className="pulls-verdict-text">
          <strong>{t("pull.verdict", { verdict: report.verdict })}</strong>
          <span className="pulls-verdict-explanation">{report.explanation}</span>
        </span>
      </div>
      <div className="pulls-check-groups">
        {INTEGRITY_DIMENSIONS.map((dimension) => {
          const results = report.results.filter((r) => r.dimension === dimension);
          if (results.length === 0) return null;
          return (
            <div className="pulls-check-group" key={dimension} data-dimension={dimension}>
              <h3 className="pulls-dimension-title">{translateOr(t, dimensionLabelKey(dimension), dimension)}</h3>
              {results.map((result) => (
                <CheckRow key={result.check} result={result} />
              ))}
            </div>
          );
        })}
      </div>
      <p className="pull-summary-line">{t("pull.computedAt", { at: report.computed_at })}</p>
    </div>
  );
}

/** One check result: severity badge, subject, detail and the machine's why. */
function CheckRow({ result }: { result: CheckResult }) {
  const failed = !result.passed;
  const cls =
    failed && result.severity === "blocking"
      ? "pulls-check-failed"
      : failed
        ? "pulls-check-warning"
        : "pulls-check-passed";
  return (
    <div
      className={`pulls-check ${cls}`}
      data-check-row={`${result.dimension}-${result.check}`}
      data-check-passed={result.passed}
      data-check-severity={result.severity}
    >
      <div className="pulls-check-head">
        {result.passed ? (
          <CheckCircleIcon size={14} aria-hidden="true" className="pulls-check-icon" />
        ) : (
          <XCircleIcon size={14} aria-hidden="true" className="pulls-check-icon" />
        )}
        <span className="pulls-check-id">{result.check}</span>
        <span className={`pulls-severity pulls-severity-${result.severity}`}>
          {result.severity}
        </span>
        {result.subject !== undefined ? (
          <span className="pulls-check-subject">{result.subject}</span>
        ) : null}
      </div>
      {result.detail !== undefined ? (
        <div className="pulls-check-detail">{result.detail}</div>
      ) : null}
      <div className="pulls-check-why">{result.why}</div>
    </div>
  );
}

/* ---------- Raw files (the secondary tab) ---------- */

function RawFilesPanel({ diff }: { diff: DiffDocument }) {
  const shell = useProjectShell();
  const t = useT();
  const apiBaseUrl = shell === null ? null : shell.apiBaseUrl;
  const projectId = shell === null ? null : shell.project.id;
  if (diff.file_diff_refs.length === 0) {
    return (
      <p className="pull-empty" data-raw-empty>
        {t("pull.raw.empty")}
      </p>
    );
  }
  return (
    <div data-raw-files>
      {diff.file_diff_refs.map((ref_) => (
        <div className="pull-raw-side" key={ref_.kind} data-file-diff-ref={ref_.kind}>
          <h3 className="pull-subtitle">
            {ref_.kind === "source"
              ? t("pull.raw.sourceFiles")
              : t("pull.raw.targetFiles")}
          </h3>
          <dl className="pull-raw-refs">
            <dt>{t("pull.raw.baseRef")}</dt>
            <dd data-raw-base-ref>
              {ref_.base_git_ref === "" ? (
                <span className="pull-raw-empty-ref">
                  {t("pull.raw.noneEmptyTree")}
                </span>
              ) : (
                <RawRefLink apiBaseUrl={apiBaseUrl} projectId={projectId} sha={ref_.base_git_ref} />
              )}
            </dd>
            <dt>{t("pull.raw.headRef")}</dt>
            <dd data-raw-head-ref>
              {ref_.head_git_ref === "" ? (
                <span className="pull-raw-empty-ref">{t("common.none")}</span>
              ) : (
                <RawRefLink apiBaseUrl={apiBaseUrl} projectId={projectId} sha={ref_.head_git_ref} />
              )}
            </dd>
          </dl>
        </div>
      ))}
      <p className="pull-summary-line">
        {t("pull.raw.note")}
      </p>
    </div>
  );
}

/** One git ref of a raw file-diff side, linked to its commit's raw patch. */
function RawRefLink({
  apiBaseUrl,
  projectId,
  sha,
}: {
  apiBaseUrl: string | null;
  projectId: string | null;
  sha: string;
}) {
  const t = useT();
  if (apiBaseUrl === null || projectId === null) {
    return <code>{sha.slice(0, 12)}</code>;
  }
  return (
    <a
      href={rawPatchUrl(apiBaseUrl, projectId, sha)}
      className="pull-raw-link"
      data-raw-patch={sha}
    >
      {t("pull.raw.patch")} <code>{sha.slice(0, 12)}</code>
    </a>
  );
}

/* ---------- Shared bits ---------- */

/** Banner color class per verdict (gate-ladder vocabulary). */
function verdictClass(verdict: IntegrityReport["verdict"]): string {
  switch (verdict) {
    case "pass":
      return "pass";
    case "blocked":
      return "blocked";
    default:
      return "warn";
  }
}

/** Banner icon per verdict. */
function verdictIcon(verdict: IntegrityReport["verdict"]) {
  switch (verdict) {
    case "pass":
      return <CheckCircleIcon size={16} aria-hidden="true" />;
    case "blocked":
      return <XCircleIcon size={16} aria-hidden="true" />;
    default:
      return <IssueOpenedIcon size={16} aria-hidden="true" />;
  }
}
