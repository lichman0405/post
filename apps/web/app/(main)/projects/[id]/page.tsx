"use client";

import Link from "next/link";
import { useEffect, useMemo, useState } from "react";
import { Spinner } from "@primer/react";
import { useProjectShell } from "./shell-context";
import { useT } from "../../../i18n-provider";
import "./demo.css";

type Overview = { counts: Record<"questions"|"findings"|"hypotheses"|"claims"|"other_objects",number>; key_questions:Array<{object_id:string;statement:string;question_state:string}>; key_findings:Array<{object_id:string;statement:string;assessment:string}>; branches:{active:Array<{id:string;name:string;purpose:string}>}; current_main:{head_state_id:string;latest_commit:{message:string;actor:string}}|null };

export default function ProjectOverviewPage(){
  const t=useT();
  const shell=useProjectShell(); const [overview,setOverview]=useState<Overview|null>(null);
  const url=useMemo(()=>shell?`${shell.apiBaseUrl}/api/v1/projects/${shell.project.id}/overview`:"",[shell]);
  useEffect(()=>{if(!url)return;const c=new AbortController();fetch(url,{credentials:"include",signal:c.signal}).then(r=>r.json()).then(setOverview).catch(()=>null);return()=>c.abort()},[url]);
  if(!shell)return null;if(!overview)return <div className="demo-loading"><Spinner aria-label={t("project.overview.loading")}/></div>;
  const {project}=shell;
  const counts=overview.counts??{};
  const total=Object.values(counts).reduce((s,n)=>s+n,0);
  const activeBranches=overview.branches?.active??[];
  const keyQuestions=overview.key_questions??[];
  const keyFindings=overview.key_findings??[];
  const spaces=[["project.space.research","project.space.researchDesc","research","project.space.researchStatus"],["project.space.issues","project.space.issuesDesc","issues","project.space.issuesStatus"],["project.space.pulls","project.space.pullsDesc","pulls","project.space.pullsStatus"],["project.space.milestones","project.space.milestonesDesc","milestones","project.space.milestonesStatus"],["project.space.assets","project.space.assetsDesc","assets","project.space.assetsStatus"],["project.space.files","project.space.filesDesc","files","project.space.filesStatus"],["project.space.releases","project.space.releasesDesc","releases","project.space.releasesStatus"]];
  return <div className="demo-overview" data-project-tab-content="overview"><section className="demo-lead"><div><span className="demo-kicker">{t("project.overview.kicker")}</span><h2>{t("project.overview.headline")}</h2><p>{project.purpose}</p></div><div className="demo-decision"><span>{t("project.overview.currentDecision")}</span><strong>{t("project.overview.decision")}</strong><small>{t("project.overview.decisionNote")}</small></div></section>
    <section className="demo-metrics">{([[total,"project.overview.metric.objects"],[counts.claims??0,"project.overview.metric.claims"],[counts.findings??0,"project.overview.metric.findings"],[activeBranches.length,"project.overview.metric.branches"],[8,"project.overview.metric.evidence"]] as Array<[number,string]>).map(([n,l])=><div key={l}><strong>{n}</strong><span>{t(l)}</span></div>)}</section>
    <div className="demo-two-col"><section className="demo-panel"><header><span className="demo-kicker">{t("project.overview.primaryQuestion")}</span><Link href={`/projects/${project.id}/research`}>{t("project.overview.openMap")}</Link></header>{/* A project with no accepted question yet renders this panel with an empty
    <h3>, which axe reports as `empty-heading` (best-practice). The fallback is
    the empty state, not a substitute for content: when a question exists its
    statement is still what renders. */}
    {keyQuestions.length > 0 ? <h3>{keyQuestions[0]?.statement}</h3> : <h3>{t("project.overview.noQuestion")}</h3>}<div className="demo-status-row"><span className="demo-badge">{t("project.overview.partiallyAnswered")}</span><span>{t("project.overview.competingHypotheses")}</span></div><h4>{t("project.overview.findings")}</h4>{keyFindings.map(f=><div className="demo-list-row" key={f.object_id}><span className={`demo-dot ${f.assessment}`}/><p>{f.statement}</p></div>)}</section><section className="demo-panel"><header><span className="demo-kicker">{t("project.overview.workInProgress")}</span><span>{t("project.overview.branchCount",{count:activeBranches.length})}</span></header><div className="demo-branch-list">{activeBranches.map(b=><div key={b.id}><strong>⑂ {b.name}</strong><p>{b.purpose}</p></div>)}</div></section></div>
    <section className="demo-panel"><header><span className="demo-kicker">{t("project.overview.workspaces")}</span><span>{t("project.overview.liveSurface")}</span></header><div className="demo-workspaces">{spaces.map(([nameKey,descKey,path,statusKey])=><Link key={nameKey} className="demo-workspace" href={`/projects/${project.id}/${path}`}><strong>{t(nameKey)}</strong><p>{t(descKey)}</p><span>{t(statusKey)} →</span></Link>)}</div></section>
    {overview.current_main?<p className="demo-head-note">{t("project.overview.acceptedMain")} <code>{overview.current_main.head_state_id.slice(0,8)}</code> · {overview.current_main.latest_commit.message} {t("project.overview.by")} {overview.current_main.latest_commit.actor}</p>:null}</div>
}
