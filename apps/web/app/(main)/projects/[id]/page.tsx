"use client";

import Link from "next/link";
import { useEffect, useMemo, useState } from "react";
import { Spinner } from "@primer/react";
import { useProjectShell } from "./shell-context";
import "./demo.css";

type Overview = { counts: Record<"questions"|"findings"|"hypotheses"|"claims"|"other_objects",number>; key_questions:Array<{object_id:string;statement:string;question_state:string}>; key_findings:Array<{object_id:string;statement:string;assessment:string}>; branches:{active:Array<{id:string;name:string;purpose:string}>}; current_main:{head_state_id:string;latest_commit:{message:string;actor:string}}|null };

export default function ProjectOverviewPage(){
  const shell=useProjectShell(); const [overview,setOverview]=useState<Overview|null>(null);
  const url=useMemo(()=>shell?`${shell.apiBaseUrl}/api/v1/projects/${shell.project.id}/overview`:"",[shell]);
  useEffect(()=>{if(!url)return;const c=new AbortController();fetch(url,{credentials:"include",signal:c.signal}).then(r=>r.json()).then(setOverview).catch(()=>null);return()=>c.abort()},[url]);
  if(!shell)return null;if(!overview)return <div className="demo-loading"><Spinner aria-label="Loading project overview"/></div>;
  const {project}=shell;
  const counts=overview.counts??{};
  const total=Object.values(counts).reduce((s,n)=>s+n,0);
  const activeBranches=overview.branches?.active??[];
  const keyQuestions=overview.key_questions??[];
  const keyFindings=overview.key_findings??[];
  const spaces=[["Research","Question → hypothesis → evidence → finding","research","ready"],["Issues","Open scientific and integrity work","issues","4 open"],["Pull requests","Research-state proposals and checks","pulls","in review"],["Milestones","Candidate and validation timeline","milestones","4 events"],["Assets","Publishable datasets and protocols","assets","8 candidates"],["Files","Methods, data dictionary and analysis notes","files","main"],["Releases","Immutable, reviewed snapshots","releases","gate pending"]];
  return <div className="demo-overview" data-project-tab-content="overview"><section className="demo-lead"><div><span className="demo-kicker">MOF humidity validation · 298 K</span><h2>Can MOF-X separate ethylene under realistic humidity?</h2><p>{project.purpose}</p></div><div className="demo-decision"><span>Current decision</span><strong>Proceed below 40% RH</strong><small>Upstream drying or reactivation required above the threshold.</small></div></section>
    <section className="demo-metrics">{[[total,"Research objects"],[counts.claims??0,"Versioned claims"],[counts.findings??0,"Synthesized findings"],[activeBranches.length,"Active branches"],[8,"Evidence links"]].map(([n,l])=><div key={l}><strong>{n}</strong><span>{l}</span></div>)}</section>
    <div className="demo-two-col"><section className="demo-panel"><header><span className="demo-kicker">Primary question</span><Link href={`/projects/${project.id}/research`}>Open research map →</Link></header>{/* A project with no accepted question yet renders this panel with an empty
    <h3>, which axe reports as `empty-heading` (best-practice). The fallback is
    the empty state, not a substitute for content: when a question exists its
    statement is still what renders. */}
    {keyQuestions.length > 0 ? <h3>{keyQuestions[0]?.statement}</h3> : <h3>No primary question yet</h3>}<div className="demo-status-row"><span className="demo-badge">partially answered</span><span>2 competing hypotheses</span></div><h4>Evidence-backed findings</h4>{keyFindings.map(f=><div className="demo-list-row" key={f.object_id}><span className={`demo-dot ${f.assessment}`}/><p>{f.statement}</p></div>)}</section><section className="demo-panel"><header><span className="demo-kicker">Work in progress</span><span>{activeBranches.length} branches</span></header><div className="demo-branch-list">{activeBranches.map(b=><div key={b.id}><strong>⑂ {b.name}</strong><p>{b.purpose}</p></div>)}</div></section></div>
    <section className="demo-panel"><header><span className="demo-kicker">Project workspaces</span><span>Live demo surface</span></header><div className="demo-workspaces">{spaces.map(([name,desc,path,status])=><Link key={name} className="demo-workspace" href={`/projects/${project.id}/${path}`}><strong>{name}</strong><p>{desc}</p><span>{status} →</span></Link>)}</div></section>
    {overview.current_main?<p className="demo-head-note">Accepted main <code>{overview.current_main.head_state_id.slice(0,8)}</code> · {overview.current_main.latest_commit.message} by {overview.current_main.latest_commit.actor}</p>:null}</div>
}
