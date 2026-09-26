"use client";

import { useEffect, useMemo, useState } from "react";
import Link from "next/link";
import { Spinner } from "@primer/react";

import { createAssetsClient, type AssetSummary } from "../../../../../lib/assets";
import { useProjectShell } from "../shell-context";
import { AdsorptionChart } from "./adsorption-chart";
import { CollaborationChart } from "./collaboration-chart";
import "../demo.css";
import "./assets-demo.css";

type ResearchObject = {
  id: string;
  object_type: string;
  version_no: number;
  title: string;
  lifecycle_state: string;
  payload: Record<string, unknown>;
};

type ObjectQuery = { objects: ResearchObject[] };

export default function AssetsPage() {
  const shell = useProjectShell();
  const [objects, setObjects] = useState<ResearchObject[] | null>(null);
  const [objectError, setObjectError] = useState<string | null>(null);
  const [assets, setAssets] = useState<AssetSummary[] | null>(null);
  const [assetError, setAssetError] = useState<string | null>(null);
  const assetClient = useMemo(
    () => (shell ? createAssetsClient(shell.apiBaseUrl) : null),
    [shell?.apiBaseUrl],
  );
  const objectUrl = useMemo(() => {
    if (!shell) return "";
    const query = new URLSearchParams();
    for (const type of ["dataset", "protocol", "material", "calculation"]) {
      query.append("object_type", type);
    }
    query.set("depth", "0");
    return `${shell.apiBaseUrl}/api/v1/projects/${encodeURIComponent(shell.project.id)}/query?${query}`;
  }, [shell]);

  useEffect(() => {
    if (!objectUrl) return;
    const controller = new AbortController();
    fetch(objectUrl, { credentials: "include", signal: controller.signal })
      .then(async (response) => {
        if (!response.ok) throw new Error(`Research records could not be loaded (${response.status}).`);
        return (await response.json()) as ObjectQuery;
      })
      .then((result) => setObjects(result.objects ?? []))
      .catch((error: unknown) => {
        if (!controller.signal.aborted) {
          setObjectError(error instanceof Error ? error.message : "Research records could not be loaded.");
          setObjects([]);
        }
      });
    return () => controller.abort();
  }, [objectUrl]);

  useEffect(() => {
    if (!assetClient || !shell) return;
    let cancelled = false;
    assetClient
      .browse()
      .then((list) => {
        if (!cancelled) {
          // This is a project page filter over assets the API has already
          // returned as publicly readable. It does not make a visibility decision.
          setAssets(list.assets.filter((asset) => asset.origin_project?.id === shell.project.id));
        }
      })
      .catch((error: unknown) => {
        if (!cancelled) {
          setAssetError(error instanceof Error ? error.message : "Published assets could not be loaded.");
          setAssets([]);
        }
      });
    return () => {
      cancelled = true;
    };
  }, [assetClient, shell]);

  if (!shell) return null;

  const datasetAssets = assets?.filter((asset) => asset.type === "dataset") ?? [];
  return (
    <div className="demo-page demo-project-assets" data-project-tab-content="assets">
      <section className="demo-page-head">
        <div>
          <span className="demo-kicker">Research outputs</span>
          <h2>Project assets</h2>
          <p>
            Source records stay in the project. Published assets have a persistent detail page
            with rights, credited creators, version history, provenance, and reuse links.
          </p>
        </div>
        <span className="demo-badge purple">synthetic demo</span>
      </section>

      <section className="demo-project-object-section" aria-labelledby="project-source-records">
        <header className="demo-project-assets-section-head">
          <div>
            <span className="demo-kicker">Project record</span>
            <h3 id="project-source-records">Datasets, protocols, materials, and calculations</h3>
          </div>
          <Link href={`/projects/${shell.project.id}/research`}>Open research map →</Link>
        </header>
        {objects === null ? (
          <div className="demo-loading"><Spinner aria-label="Loading project records" /></div>
        ) : objectError ? (
          <p className="demo-assets-message" role="status">{objectError}</p>
        ) : objects.length === 0 ? (
          <p className="demo-assets-message">No source records of these types are on this project yet.</p>
        ) : (
          <div className="demo-asset-grid">
            {objects.map((object) => (
              <article className="demo-asset demo-source-object" key={object.id}>
                <div className="demo-asset-icon" aria-hidden="true">
                  {object.object_type === "dataset" ? "▦" : object.object_type === "protocol" ? "≡" : "◇"}
                </div>
                <div>
                  <span className="demo-kicker">{object.object_type} · v{object.version_no}</span>
                  <h3>{object.title || object.object_type}</h3>
                  <p>{summaryOf(object.payload)}</p>
                  <div className="demo-labels">
                    <span>{object.lifecycle_state}</span>
                    <span>project scoped</span>
                    <span>object {object.id.slice(0, 8)}</span>
                  </div>
                  <details className="demo-object-detail">
                    <summary>View full metadata</summary>
                    <pre>{JSON.stringify(object.payload, null, 2)}</pre>
                  </details>
                </div>
              </article>
            ))}
          </div>
        )}
      </section>

      {shell.project.slug === "demo-mof-humidity-separation" && datasetAssets.some((asset) => asset.slug.includes("replicates")) ? (
        <section className="demo-chart-section" aria-labelledby="adsorption-figure-title">
          <header className="demo-project-assets-section-head">
            <div>
              <span className="demo-kicker">Scientific figure · synthetic data</span>
              <h3 id="adsorption-figure-title">C₂H₄ and C₂H₆ adsorption under humidity</h3>
            </div>
          </header>
          <AdsorptionChart sources={datasetAssets} />
        </section>
      ) : null}

      {shell.project.slug === "demo-mofx-independent-replication" || shell.project.slug === "demo-mofy-transferability-study" ? (
        <section className="demo-chart-section" aria-labelledby="collaboration-figure-title">
          <header className="demo-project-assets-section-head"><div><span className="demo-kicker">Scientific figure · synthetic project data</span><h3 id="collaboration-figure-title">{shell.project.slug === "demo-mofx-independent-replication" ? "Independent adsorption isotherms" : "MOF-Y predicted site occupancy"}</h3></div></header>
          <CollaborationChart kind={shell.project.slug === "demo-mofx-independent-replication" ? "replication" : "transferability"} />
        </section>
      ) : null}

      <section className="demo-published-assets-section" aria-labelledby="published-assets-title">
        <header className="demo-project-assets-section-head">
          <div>
            <span className="demo-kicker">Network publications</span>
            <h3 id="published-assets-title">Published digital assets</h3>
          </div>
          <Link href="/assets">Browse all network assets →</Link>
        </header>
        {assets === null ? (
          <div className="demo-loading"><Spinner aria-label="Loading published assets" /></div>
        ) : assetError ? (
          <p className="demo-assets-message" role="status">{assetError}</p>
        ) : assets.length === 0 ? (
          <p className="demo-assets-message">No public assets have been published from this project.</p>
        ) : (
          <div className="demo-published-assets-list">
            {assets.map((asset) => (
              <article className="demo-published-asset" key={asset.pid}>
                <div className="demo-published-asset-type">{asset.type.replaceAll("_", " ")}</div>
                <div className="demo-published-asset-main">
                  <Link href={asset.url} className="demo-published-asset-title">{asset.title}</Link>
                  <div className="demo-published-asset-facts">
                    <span>{asset.public_versions} public version{asset.public_versions === 1 ? "" : "s"}</span>
                    <span>latest {asset.latest_version}</span>
                    <code>{asset.pid}</code>
                  </div>
                </div>
                <Link href={asset.latest_url} className="demo-published-asset-open">View full details →</Link>
              </article>
            ))}
          </div>
        )}
        <p className="demo-assets-boundary">
          The demo shows metadata, rights, lineage, versions, events, and reuse. Large-file upload
          and download are not enabled in this V1 trial.
        </p>
      </section>
    </div>
  );
}

function summaryOf(payload: Record<string, unknown>): string {
  for (const key of ["purpose", "objective", "statement", "result_summary", "quality_notes"]) {
    const value = payload[key];
    if (typeof value === "string" && value.trim() !== "") return value;
  }
  return "Open the metadata panel to inspect this research record.";
}
