"use client";

import Link from "next/link";
import { useEffect, useMemo, useState } from "react";
import { Spinner } from "@primer/react";

import { useT } from "../../../../i18n-provider";
import { useProjectShell } from "../shell-context";
import {
  countEntries,
  humanizeCode,
  mapLanes,
  objectHref,
  objectTypeKey,
  relationKindKey,
  researchHref,
  researchNodes,
  type NodeRelation,
  type ResearchNode,
  type ResearchOutline,
  type ResearchView,
} from "./research-data";
import "./research.css";

/**
 * The Research tab (WEB-05 / G06): the project's research OBJECTS, not a
 * file tree (D14). Two surfaces over ONE node set:
 *
 *   - the Research Map, an aggregated picture: one lane per object type,
 *     each node naming the outline edges it takes part in;
 *   - the List, the same nodes in outline order with their statements.
 *
 * `researchNodes` is the single traversal both render, so the picture and
 * the list cannot disagree about which objects exist. Every node that has
 * a branch coordinate links to the object's own page — a node whose
 * version carries no branch renders as text, never as a link to a
 * fabricated coordinate.
 *
 * Read-only by construction: no form, no input, no mutation affordance.
 * The page is exactly as visible as its project (the API's read gate), and
 * a refused read renders a neutral state, not a partial outline.
 */
export function ResearchWorkspace({ view, focus }: { view: ResearchView; focus: string }) {
  const shell = useProjectShell();
  const t = useT();
  const [outline, setOutline] = useState<ResearchOutline | null>(null);
  const [error, setError] = useState("");
  const url = useMemo(
    () =>
      shell === null
        ? ""
        : `${shell.apiBaseUrl.replace(/\/+$/, "")}/api/v1/projects/${encodeURIComponent(shell.project.id)}/research`,
    [shell],
  );

  useEffect(() => {
    if (url === "") return;
    const controller = new AbortController();
    fetch(url, { credentials: "include", signal: controller.signal, headers: { accept: "application/json" } })
      .then(async (response) => {
        if (!response.ok) throw new Error(t("research.error.fallback"));
        return (await response.json()) as ResearchOutline;
      })
      .then(setOutline)
      .catch((cause: unknown) => {
        if (!(cause instanceof DOMException && cause.name === "AbortError")) {
          setError(cause instanceof Error ? cause.message : t("research.error.fallback"));
        }
      });
    return () => controller.abort();
  }, [url, t]);

  if (shell === null) return null;
  if (error !== "") {
    return (
      <div className="research-state" data-research-error>
        <h2>{t("research.error.title")}</h2>
        <p>{error}</p>
      </div>
    );
  }
  if (outline === null) {
    return (
      <div className="research-state">
        <Spinner aria-label={t("research.loading")} />
      </div>
    );
  }

  const projectId = shell.project.id;
  const nodes = researchNodes(outline, view);
  const lanes = mapLanes(nodes);
  const empty = nodes.length === 0;

  return (
    <div className="research-workspace" data-project-tab-content="research" data-research-workspace>
      <section className="research-hero">
        <div>
          <p className="research-eyebrow">{t("research.eyebrow")}</p>
          <h2>{t("research.title")}</h2>
          <p>{t("research.intro")}</p>
        </div>
        <div className="research-head">
          <span>{t("research.project")}</span>
          <strong>{outline.project_name}</strong>
          <small>{outline.project_slug}</small>
        </div>
      </section>

      <nav className="research-views" aria-label={t("research.viewLabel")}>
        {(["questions", "findings"] as ResearchView[]).map((key) => (
          <Link
            key={key}
            href={researchHref(projectId, key)}
            className="research-view-link"
            data-research-view-link={key}
            aria-current={view === key ? "page" : undefined}
          >
            {t(key === "questions" ? "research.view.questions" : "research.view.findings")}
          </Link>
        ))}
      </nav>

      <section className="research-counts" aria-label={t("research.countsLabel")}>
        {countEntries(outline).map((entry) => (
          <div className="research-count" key={entry.key}>
            <strong>{entry.count}</strong>
            <span>{t(entry.key)}</span>
          </div>
        ))}
      </section>

      {empty ? (
        <div className="research-state" data-research-empty>
          <h2>{t("research.empty.title")}</h2>
          <p>{t("research.empty.body")}</p>
        </div>
      ) : (
        <>
          <section className="research-panel" aria-label={t("research.map.label")} data-research-map>
            <header>
              <p className="research-eyebrow">{t("research.map.eyebrow")}</p>
              <h3>{t("research.map.title")}</h3>
              <p className="research-panel-note">{t("research.map.note")}</p>
            </header>
            <div className="research-map">
              {lanes.map((lane) => (
                <div className="research-map-lane" key={lane.objectType} data-research-map-lane={lane.objectType}>
                  <h4>{objectTypeLabel(t, lane.objectType)}</h4>
                  <ul>
                    {lane.nodes.map((node) => (
                      <li key={node.objectId}>
                        <MapNode node={node} projectId={projectId} focus={focus} />
                      </li>
                    ))}
                  </ul>
                </div>
              ))}
            </div>
          </section>

          <section className="research-panel" aria-label={t("research.list.label")} data-research-list>
            <header>
              <p className="research-eyebrow">{t("research.list.eyebrow")}</p>
              <h3>{t("research.list.title")}</h3>
              <p className="research-panel-note">{t("research.list.note")}</p>
            </header>
            <ul className="research-rows">
              {nodes.map((node) => (
                <li className="research-row" key={node.objectId}>
                  <Row node={node} projectId={projectId} focus={focus} />
                </li>
              ))}
            </ul>
          </section>
        </>
      )}
    </div>
  );
}

type Translate = ReturnType<typeof useT>;

function objectTypeLabel(t: Translate, objectType: string): string {
  const key = objectTypeKey(objectType);
  return key === null ? humanizeCode(objectType) : t(key);
}

function relationLabel(t: Translate, relation: NodeRelation): string {
  const key = relationKindKey(relation.kind);
  return key === null ? humanizeCode(relation.kind) : t(key);
}

function NodeMeta({ node, t }: { node: ResearchNode; t: Translate }) {
  return (
    <span className="research-node-meta">
      <span className={`research-pill type-${node.objectType}`}>{objectTypeLabel(t, node.objectType)}</span>
      {node.state === "" ? null : <span className="research-pill state">{humanizeCode(node.state)}</span>}
    </span>
  );
}

function MapNode({ node, projectId, focus }: { node: ResearchNode; projectId: string; focus: string }) {
  const t = useT();
  const body = (
    <>
      <NodeMeta node={node} t={t} />
      <strong>{node.title}</strong>
      {node.relations.length === 0 ? null : (
        <span className="research-node-edges">
          {node.relations.map((relation) => (
            <small key={`${relation.kind}:${relation.objectId}`}>
              {relationLabel(t, relation)} · {relation.title}
            </small>
          ))}
        </span>
      )}
    </>
  );
  return node.branchId === "" ? (
    <span className="research-node" data-research-map-node={node.objectId} data-object-type={node.objectType}>
      {body}
    </span>
  ) : (
    <Link
      className="research-node"
      href={objectHref(projectId, node.objectId)}
      data-research-map-node={node.objectId}
      data-object-type={node.objectType}
      data-research-focus={focus === node.objectId ? "true" : undefined}
      aria-current={focus === node.objectId ? "true" : undefined}
    >
      {body}
    </Link>
  );
}

function Row({ node, projectId, focus }: { node: ResearchNode; projectId: string; focus: string }) {
  const t = useT();
  const body = (
    <>
      <span className="research-row-head">
        <NodeMeta node={node} t={t} />
        <strong>{node.title}</strong>
      </span>
      {node.statement === "" ? null : <p className="research-row-statement">{node.statement}</p>}
      {node.relations.length === 0 ? null : (
        <span className="research-row-edges">
          {node.relations.map((relation) => (
            <small key={`${relation.kind}:${relation.objectId}`}>
              {relationLabel(t, relation)} · {relation.title}
            </small>
          ))}
        </span>
      )}
    </>
  );
  const label = t("research.row.open", { title: node.title });
  return node.branchId === "" ? (
    <span className="research-row-body" data-research-row={node.objectId} data-object-type={node.objectType}>
      {body}
    </span>
  ) : (
    <Link
      className="research-row-body"
      href={objectHref(projectId, node.objectId)}
      data-research-row={node.objectId}
      data-object-type={node.objectType}
      data-research-focus={focus === node.objectId ? "true" : undefined}
      aria-current={focus === node.objectId ? "true" : undefined}
      aria-label={label}
    >
      {body}
    </Link>
  );
}
