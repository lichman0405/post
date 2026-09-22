"use client";

import { useEffect, useMemo, useState } from "react";
import { Spinner } from "@primer/react";
import { useT } from "../../../../i18n-provider";
import { useProjectShell } from "../shell-context";
import "./research.css";

type Linked = { object_id: string; object_type: string; title: string };
type Claim = { object_id: string; version_id: string; title: string; resolved: boolean };
type Question = { object_id: string; statement: string; question_state: string; hypotheses: Linked[]; findings: Linked[] };
type Finding = { object_id: string; statement: string; finding_type: string; assessment: string; claims: Claim[] };
type Branch = { id: string; name: string; purpose: string; head_state_id: string };
type Overview = {
  counts: { questions: number; findings: number; hypotheses: number; claims: number; other_objects: number };
  key_questions: Question[];
  key_findings: Finding[];
  branches: { active: Branch[]; merged: number; aborted: number };
  current_main: { head_state_id: string; latest_commit: { message: string; actor: string } } | null;
  empty: boolean;
};

const humanize = (value: string) => value.replaceAll("_", " ");

export default function ResearchPage() {
  const shell = useProjectShell();
  const t = useT();
  const [overview, setOverview] = useState<Overview | null>(null);
  const [error, setError] = useState("");
  const url = useMemo(() => shell === null ? "" : `${shell.apiBaseUrl.replace(/\/+$/, "")}/api/v1/projects/${encodeURIComponent(shell.project.id)}/overview`, [shell]);

  useEffect(() => {
    if (url === "") return;
    const controller = new AbortController();
    fetch(url, { credentials: "include", signal: controller.signal, headers: { accept: "application/json" } })
      .then(async (response) => {
        if (!response.ok) throw new Error(`Research state request failed (${response.status})`);
        return response.json() as Promise<Overview>;
      })
      .then(setOverview)
      .catch((cause: unknown) => {
        // T1105: this sentence renders (it is the <p> under the heading), so
        // it is copy. The scanner does not see it — its rules are syntactic
        // and this literal reaches the DOM through state — which is why the
        // not-in-catalog number is a floor rather than a census.
        if (!(cause instanceof DOMException && cause.name === "AbortError")) setError(cause instanceof Error ? cause.message : t("research.error.fallback"));
      });
    return () => controller.abort();
    // `t` is a dep for the same reason as everywhere else in this diff: the
    // fallback sentence is resolved inside the catch.
  }, [url, t]);

  if (shell === null) return null;
  if (error !== "") return <div className="research-state"><h2>{t("research.error.title")}</h2><p>{error}</p></div>;
  if (overview === null) return <div className="research-state"><Spinner aria-label={t("research.loading")} /></div>;
  if (overview.empty) return <div className="research-state"><h2>{t("research.empty.title")}</h2><p>{t("research.empty.body")}</p></div>;

  const counts = [[t("research.count.questions"), overview.counts.questions], [t("research.count.hypotheses"), overview.counts.hypotheses], [t("research.count.claims"), overview.counts.claims], [t("research.count.findings"), overview.counts.findings], [t("research.count.other"), overview.counts.other_objects]] as const;
  return (
    <div className="research-workspace" data-project-tab-content="research">
      <section className="research-hero">
        <div><p className="research-eyebrow">{t("research.eyebrow")}</p><h2>{t("research.title")}</h2><p>{t("research.intro")}</p></div>
        {overview.current_main ? <div className="research-head"><span>{t("research.mainHead")}</span><code>{overview.current_main.head_state_id.slice(0, 8)}</code><small>{overview.current_main.latest_commit.message} · {overview.current_main.latest_commit.actor}</small></div> : null}
      </section>

      <section className="research-counts" aria-label={t("research.countsLabel")}>
        {counts.map(([label, count]) => <div className="research-count" key={label}><strong>{count}</strong><span>{label}</span></div>)}
      </section>

      <div className="research-columns">
        <section className="research-panel"><header><p className="research-eyebrow">{t("research.questions.eyebrow")}</p><h3>{t("research.questions.title")}</h3></header><div className="research-stack">
          {overview.key_questions.map((question) => <article className="research-card question-card" key={question.object_id}>
            <div className="research-card-heading"><span className="research-pill question">{humanize(question.question_state)}</span><span className="research-ref">{t("research.refQuestion")}{" "}{question.object_id.slice(0, 8)}</span></div>
            <h4>{question.statement}</h4>
            <div className="research-linked-grid"><div><h5>{t("research.hypotheses")}</h5>{question.hypotheses.map((item, index) => <p key={item.object_id}><b>{t("research.hypothesisMark")}{index + 1}</b>{item.title}</p>)}</div><div><h5>{t("research.findingsLinked")}</h5>{question.findings.map((item) => <p key={item.object_id}><span className="research-dot" />{item.title}</p>)}</div></div>
          </article>)}
        </div></section>

        <section className="research-panel"><header><p className="research-eyebrow">{t("research.findings.eyebrow")}</p><h3>{t("research.findings.title")}</h3></header><div className="research-stack">
          {overview.key_findings.map((finding) => <article className="research-card finding-card" key={finding.object_id}>
            <div className="research-card-heading"><span className={`research-pill ${finding.assessment}`}>{humanize(finding.assessment)}</span><span className="research-ref">{humanize(finding.finding_type)}</span></div>
            <h4>{finding.statement}</h4>
            <div className="claim-list"><h5>{t("research.claimBasis")}</h5>{finding.claims.map((claim) => <div className="claim-row" key={claim.version_id}><span aria-hidden="true">{claim.resolved ? "✓" : "?"}</span><p>{claim.title}</p></div>)}</div>
          </article>)}
        </div></section>
      </div>

      <section className="research-panel branch-panel"><header><p className="research-eyebrow">{t("research.branches.eyebrow")}</p><h3>{t("research.branches.title")}</h3></header><div className="branch-grid">
        {overview.branches.active.map((branch) => <article className="branch-card" key={branch.id}><div><span className="branch-mark">⑂</span><h4>{branch.name}</h4></div><p>{branch.purpose}</p><small>{t("research.branchHead")}{" "}{branch.head_state_id.slice(0, 8)}</small></article>)}
      </div></section>
    </div>
  );
}
