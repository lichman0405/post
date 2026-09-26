"use client";

import Link from "next/link";
import { useEffect, useMemo, useState } from "react";
import { createProjectsClient, type Project } from "../../lib/projects";
import { demoStories } from "../../lib/demo-story";
import "./demo-landing.css";

const projectOrder = ["demo-mof-humidity-separation", "demo-mofx-independent-replication", "demo-mofy-transferability-study"];

export function DemoLanding({ apiBaseUrl }: { apiBaseUrl: string }) {
  const client = useMemo(() => createProjectsClient(apiBaseUrl), [apiBaseUrl]);
  const [projects, setProjects] = useState<Project[]>([]);
  useEffect(() => {
    let cancelled = false;
    client.list().then(list => { if (!cancelled) setProjects(list); }).catch(() => undefined);
    return () => { cancelled = true; };
  }, [client]);
  const bySlug = new Map(projects.map(project => [project.slug, project]));
  return <div className="landing">
    <section className="landing-hero"><div><span className="landing-kicker">POST · Open scientific work, with provenance</span><h1>Follow the question. Inspect the evidence. See who contributed.</h1><p>A synthetic materials-science case study shows how POST connects a research question to protocols, data, competing findings, independent replication, and reusable digital assets.</p><div className="landing-actions"><Link href={bySlug.get(projectOrder[0]) ? `/projects/${bySlug.get(projectOrder[0])!.id}` : "/projects"}>Explore the MOF-X study →</Link><Link href="/demo">Choose a demo role</Link></div></div><div className="landing-key-result"><span>CASE STUDY · 298 K</span><strong>C₂H₄ / C₂H₆</strong><p>What happens to separation selectivity when relative humidity rises from dry conditions to 40% and 70%?</p><small>All data and identities are synthetic.</small></div></section>
    <section className="landing-section"><div className="landing-heading"><span>THE SCIENTIFIC STORY</span><h2>One result, three accountable research spaces</h2><p>Each project has its own owner and records. The disagreement remains visible.</p></div><div className="landing-projects">{projectOrder.map((slug, index) => { const story = demoStories[slug]; const project = bySlug.get(slug); return <article key={slug}><span>0{index + 1} · {index === 0 ? "Source campaign" : index === 1 ? "Independent laboratory" : "Computational group"}</span><h3>{story.headline}</h3><p>{story.interpretation}</p><Link href={project ? `/projects/${project.id}` : "/projects"}>{project ? "Open project →" : "Find project →"}</Link></article>; })}</div></section>
    <section className="landing-section landing-path"><div className="landing-heading"><span>WHAT TO EXPLORE</span><h2>A guided route through POST</h2></div><div className="landing-steps"><Link href={bySlug.get(projectOrder[0]) ? `/projects/${bySlug.get(projectOrder[0])!.id}/research` : "/projects"}><b>01</b><strong>Research map</strong><span>Question, hypotheses, findings and competing interpretations</span></Link><Link href={bySlug.get(projectOrder[0]) ? `/projects/${bySlug.get(projectOrder[0])!.id}/assets` : "/assets"}><b>02</b><strong>Evidence and assets</strong><span>Replicate data, methods, versions, rights and provenance</span></Link><Link href="/people"><b>03</b><strong>People and contributions</strong><span>Eight demo accounts from distinct roles and organizations</span></Link></div></section>
    <p className="landing-disclaimer">Presentation boundary: this is a synthetic product demonstration. No actual adsorption experiment or DFT calculation was performed. File transfer is outside this V1 trial.</p>
  </div>;
}
