"use client";

import { useEffect, useMemo, useState } from "react";
import { Spinner } from "@primer/react";
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
        if (!(cause instanceof DOMException && cause.name === "AbortError")) setError(cause instanceof Error ? cause.message : "Research state is unavailable");
      });
    return () => controller.abort();
  }, [url]);

  if (shell === null) return null;
  if (error !== "") return <div className="research-state"><h2>Research state unavailable</h2><p>{error}</p></div>;
  if (overview === null) return <div className="research-state"><Spinner aria-label="Loading research state" /></div>;
  if (overview.empty) return <div className="research-state"><h2>No research state yet</h2><p>Create a research question on the main branch to begin.</p></div>;

  const counts = [["Questions", overview.counts.questions], ["Hypotheses", overview.counts.hypotheses], ["Claims", overview.counts.claims], ["Findings", overview.counts.findings], ["Other objects", overview.counts.other_objects]] as const;
  return (
    <div className="research-workspace" data-project-tab-content="research">
      <section className="research-hero">
        <div><p className="research-eyebrow">Accepted research state</p><h2>Evidence-led project map</h2><p>Questions connect competing hypotheses to version-pinned claims, findings and active research branches.</p></div>
        {overview.current_main ? <div className="research-head"><span>Main head</span><code>{overview.current_main.head_state_id.slice(0, 8)}</code><small>{overview.current_main.latest_commit.message} · {overview.current_main.latest_commit.actor}</small></div> : null}
      </section>

      <section className="research-counts" aria-label="Research object counts">
        {counts.map(([label, count]) => <div className="research-count" key={label}><strong>{count}</strong><span>{label}</span></div>)}
      </section>

      <div className="research-columns">
        <section className="research-panel"><header><p className="research-eyebrow">Research questions</p><h3>What the project is trying to resolve</h3></header><div className="research-stack">
          {overview.key_questions.map((question) => <article className="research-card question-card" key={question.object_id}>
            <div className="research-card-heading"><span className="research-pill question">{humanize(question.question_state)}</span><span className="research-ref">Q · {question.object_id.slice(0, 8)}</span></div>
            <h4>{question.statement}</h4>
            <div className="research-linked-grid"><div><h5>Competing hypotheses</h5>{question.hypotheses.map((item, index) => <p key={item.object_id}><b>H{index + 1}</b>{item.title}</p>)}</div><div><h5>Synthesized findings</h5>{question.findings.map((item) => <p key={item.object_id}><span className="research-dot" />{item.title}</p>)}</div></div>
          </article>)}
        </div></section>

        <section className="research-panel"><header><p className="research-eyebrow">Key findings</p><h3>What the current evidence supports</h3></header><div className="research-stack">
          {overview.key_findings.map((finding) => <article className="research-card finding-card" key={finding.object_id}>
            <div className="research-card-heading"><span className={`research-pill ${finding.assessment}`}>{humanize(finding.assessment)}</span><span className="research-ref">{humanize(finding.finding_type)}</span></div>
            <h4>{finding.statement}</h4>
            <div className="claim-list"><h5>Claim basis</h5>{finding.claims.map((claim) => <div className="claim-row" key={claim.version_id}><span aria-hidden="true">{claim.resolved ? "✓" : "?"}</span><p>{claim.title}</p></div>)}</div>
          </article>)}
        </div></section>
      </div>

      <section className="research-panel branch-panel"><header><p className="research-eyebrow">Active research branches</p><h3>Work in progress, separated from accepted main</h3></header><div className="branch-grid">
        {overview.branches.active.map((branch) => <article className="branch-card" key={branch.id}><div><span className="branch-mark">⑂</span><h4>{branch.name}</h4></div><p>{branch.purpose}</p><small>head {branch.head_state_id.slice(0, 8)}</small></article>)}
      </div></section>
    </div>
  );
}
