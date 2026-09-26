"use client";

import Link from "next/link";
import { useEffect, useMemo, useState } from "react";
import { Spinner } from "@primer/react";
import { useProjectShell } from "./shell-context";
import { demoStories } from "../../../../lib/demo-story";
import "./demo.css";

type Overview = {
  counts: Record<string, number>;
  key_questions: Array<{ object_id: string; statement: string }>;
  key_findings: Array<{ object_id: string; statement: string }>;
  branches: { active: Array<{ id: string; name: string; purpose: string }> };
  current_main: { head_state_id?: string } | null;
};

export default function ProjectOverviewPage() {
  const shell = useProjectShell();
  const [overview, setOverview] = useState<Overview | null>(null);
  const [error, setError] = useState<string | null>(null);
  const url = useMemo(() => shell ? `${shell.apiBaseUrl}/api/v1/projects/${shell.project.id}/overview` : "", [shell]);
  useEffect(() => {
    if (!url) return;
    const controller = new AbortController();
    fetch(url, { credentials: "include", signal: controller.signal })
      .then(async response => {
        if (!response.ok) throw new Error(`Project overview could not be loaded (${response.status}).`);
        return response.json() as Promise<Overview>;
      })
      .then(setOverview)
      .catch((cause: unknown) => { if (!controller.signal.aborted) setError(cause instanceof Error ? cause.message : "Project overview could not be loaded."); });
    return () => controller.abort();
  }, [url]);
  if (!shell) return null;
  if (error) return <p role="alert">{error}</p>;
  if (!overview) return <div className="demo-loading"><Spinner aria-label="Loading project overview" /></div>;

  const { project } = shell;
  const story = demoStories[project.slug];
  const externalFork = project.slug.startsWith("demo-mof-humidity-separation-demo-external-");
  const counts = overview.counts ?? {};
  const total = Object.values(counts).reduce((sum, count) => sum + count, 0);
  const questions = overview.key_questions ?? [];
  const findings = overview.key_findings ?? [];
  const branches = overview.branches?.active ?? [];
  const workspaces = story ? [
    ["Research map", "Questions, findings and recorded relationships", "research"],
    ["Source records and assets", "Methods, datasets and digital assets", "assets"],
    ["Files", "Versioned project files and analysis notes", "files"],
    ["Activity", "Who contributed and when", "activity"],
  ] : externalFork ? [
    ["Research map", "Active branch and recorded research objects", "research"],
    ["Activity", "Follow this external contribution", "activity"],
    ["Files", "Versioned files in this fork", "files"],
  ] : [
    ["Research", "Questions, evidence and findings", "research"],
    ["Issues", "Open work", "issues"],
    ["Pull requests", "Proposed changes", "pulls"],
    ["Assets", "Research outputs", "assets"],
  ];

  return <div className="demo-overview" data-project-tab-content="overview">
    <section className="demo-lead"><div><span className="demo-kicker">{story?.eyebrow ?? "Project overview"}</span><h2>{story?.headline ?? project.name}</h2><p>{story?.interpretation ?? project.purpose}</p></div>{story ? <div className="demo-decision"><span>Research status</span><strong>{project.slug === "demo-mof-humidity-separation" ? "Partially answered" : "Open for validation"}</strong><small>Every result here is synthetic.</small></div> : null}</section>
    <section className="demo-metrics" aria-label="Recorded project content">{([[total, "Research objects"], [counts.questions ?? 0, "Questions"], [counts.findings ?? 0, "Findings"], [counts.claims ?? 0, "Claims"], [branches.length, "Active branches"]] as Array<[number, string]>).map(([number, label]) => <div key={label}><strong>{number}</strong><span>{label}</span></div>)}</section>
    {story ? <><div className="demo-two-col">
      <section className="demo-panel"><header><span className="demo-kicker">Research question</span><Link href={`/projects/${project.id}/research`}>Open research map →</Link></header><h3>{story.question}</h3><h4>What the records show</h4>{story.observations.map(item => <div className="demo-list-row" key={item}><span className="demo-dot" /><p>{item}</p></div>)}</section>
      <section className="demo-panel"><header><span className="demo-kicker">Methods and provenance</span><Link href={`/projects/${project.id}/assets`}>Inspect records →</Link></header><h3>Trace the result back to its inputs</h3>{story.methods.map(item => <div className="demo-list-row" key={item}><span className="demo-dot preliminary" /><p>{item}</p></div>)}<h4>Next experiment</h4><p>{story.nextStep}</p></section>
    </div><p className="demo-story-caveat">{story.caveat}</p></> : externalFork ? <><div className="demo-two-col">
      <section className="demo-panel"><header><span className="demo-kicker">External replication</span><Link href={`/projects/${project.id}/research`}>Open research map →</Link></header><h3>A second group tests the 70% RH result</h3><p>This synthetic fork records an independently prepared MOF-X sample, a 180 °C vacuum activation protocol, and a 70% RH adsorption dataset. Its proposed result shows an 18% selectivity loss, smaller than the source campaign.</p><h4>Why this matters</h4><p>The contradictory evidence stays attributable to the external contributor while the parent project reviews the open pull request.</p></section>
      <section className="demo-panel"><header><span className="demo-kicker">Contribution status</span><span>{branches.length} active branch</span></header><h3>Work remains on the fork branch</h3><div className="demo-branch-list">{branches.map(branch => <div key={branch.id}><strong>{branch.name}</strong><p>{branch.purpose}</p></div>)}</div><p>No result from this fork has been accepted into its main state yet.</p></section>
    </div><p className="demo-story-caveat">All people, organizations, measurements, and results in this fork are synthetic demonstration records.</p></> : <div className="demo-two-col">
      <section className="demo-panel"><header><span className="demo-kicker">Primary question</span><Link href={`/projects/${project.id}/research`}>Open research map →</Link></header><h3>{questions[0]?.statement ?? "No question recorded yet"}</h3><h4>Findings</h4>{findings.map(item => <div className="demo-list-row" key={item.object_id}><span className="demo-dot" /><p>{item.statement}</p></div>)}</section>
      <section className="demo-panel"><header><span className="demo-kicker">Active branches</span><span>{branches.length}</span></header><div className="demo-branch-list">{branches.map(branch => <div key={branch.id}><strong>{branch.name}</strong><p>{branch.purpose}</p></div>)}</div></section>
    </div>}
    <section className="demo-panel"><header><span className="demo-kicker">Explore this project</span><Link href="/projects">All projects →</Link></header><div className="demo-workspaces">{workspaces.map(([name, description, path]) => <Link className="demo-workspace" key={path} href={`/projects/${project.id}/${path}`}><strong>{name}</strong><p>{description}</p><span>Open →</span></Link>)}</div></section>
    {overview.current_main?.head_state_id ? <p className="demo-head-note">Accepted main state <code>{overview.current_main.head_state_id.slice(0, 8)}</code></p> : null}
  </div>;
}
