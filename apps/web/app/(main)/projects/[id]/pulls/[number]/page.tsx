"use client";

import { useEffect, useMemo, useState } from "react";
import Link from "next/link";
import {
  CheckCircleIcon,
  IssueOpenedIcon,
  XCircleIcon,
} from "@primer/octicons-react";
import { Spinner } from "@primer/react";

import {
  ApiError,
  createPullsClient,
  dimensionLabel,
  INTEGRITY_DIMENSIONS,
  messageForPullRequestCode,
  type CheckResult,
  type IntegrityReport,
  type PullRequest,
} from "../../../../../../lib/pulls";
import { useProjectShell } from "../../shell-context";

/**
 * Pull request detail (T0403): the PR pins (base → proposed) and the
 * machine integrity report rendered as the review answer. The verdict
 * banner uses the gate-ladder vocabulary (pass green, pass_with_warnings
 * yellow, blocked red); blocking failures and warnings inherit those
 * colors per row, and every row shows the machine's own reason (why).
 */
export default function PullDetailPage({
  params,
}: {
  params: Promise<{ number: string }>;
}) {
  const shell = useProjectShell();

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
  } | null>(null);
  const [error, setError] = useState<{ number: number; text: string } | null>(null);

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
    Promise.all([client.get(shell.project.id, number), client.checks(shell.project.id, number)])
      .then(([prResult, reportResult]) => {
        if (cancelled) return;
        setError(null);
        setLoaded({ number, pr: prResult, report: reportResult });
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        setError({
          number,
          text:
            err instanceof ApiError
              ? messageForPullRequestCode(err.code)
              : "Could not load this pull request.",
        });
      });
    return () => {
      cancelled = true;
    };
  }, [shell, client, number]);

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
            {messageForPullRequestCode("VALIDATION_FAILED")}
          </div>
          <p className="pulls-back">
            <Link href={`/projects/${shell.project.id}/pulls`}>
              Back to pull requests
            </Link>
          </p>
        </section>
      </div>
    );
  }

  const data = loaded !== null && loaded.number === number ? loaded : null;
  const errorText = error !== null && error.number === number ? error.text : null;

  return (
    <div className="pulls-page" data-pulls-detail data-pull-number={number ?? undefined}>
      <p className="pulls-back">
        <Link href={`/projects/${shell.project.id}/pulls`}>
          Back to pull requests
        </Link>
      </p>

      {errorText !== null ? (
        <section className="pulls-section">
          <div className="pulls-error" data-pulls-error>{errorText}</div>
        </section>
      ) : data === null ? (
        <Spinner aria-label="Loading pull request" />
      ) : (
        <>
          <section className="pulls-section" data-pull-header>
            <h2 className="pulls-detail-title">
              <span className="pulls-detail-number">#{data.pr.number}</span>{" "}
              {data.pr.title}
            </h2>
            <p className="pulls-detail-meta">
              <span className="pulls-state" data-pull-state={data.pr.state}>
                {data.pr.state}
              </span>
              <span>opened {data.pr.created_at.slice(0, 10)} by {data.pr.created_by}</span>
            </p>
            {data.pr.body !== "" ? (
              <p className="pulls-detail-body">{data.pr.body}</p>
            ) : null}
            <dl className="pulls-pins">
              <dt>Base (main) state</dt>
              <dd className="pulls-pin-id">{data.pr.base_state_id}</dd>
              <dt>Proposed (branch) state</dt>
              <dd className="pulls-pin-id">{data.pr.proposed_state_id}</dd>
            </dl>
          </section>

          <section className="pulls-section" data-pull-checks>
            <h2 className="pulls-section-title">Integrity checks</h2>
            <p className="pulls-section-desc">
              Machine-computed review of the proposal against the pinned base.
              Computed at {data.report.computed_at}.
            </p>
            <div
              className={`pulls-verdict pulls-verdict-${verdictClass(data.report.verdict)}`}
              data-verdict={data.report.verdict}
            >
              {verdictIcon(data.report.verdict)}
              <span className="pulls-verdict-text">
                <strong>Verdict: {data.report.verdict}</strong>
                <span className="pulls-verdict-explanation">
                  {data.report.explanation}
                </span>
              </span>
            </div>
            <div className="pulls-check-groups">
              {INTEGRITY_DIMENSIONS.map((dimension) => {
                const results = data.report.results.filter((r) => r.dimension === dimension);
                if (results.length === 0) return null;
                return (
                  <div
                    className="pulls-check-group"
                    key={dimension}
                    data-dimension={dimension}
                  >
                    <h3 className="pulls-dimension-title">
                      {dimensionLabel(dimension)}
                    </h3>
                    {results.map((result) => (
                      <CheckRow key={result.check} result={result} />
                    ))}
                  </div>
                );
              })}
            </div>
          </section>
        </>
      )}
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
