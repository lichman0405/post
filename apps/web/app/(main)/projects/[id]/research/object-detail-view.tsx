"use client";

import Link from "next/link";
import { useEffect, useMemo, useState } from "react";
import { Spinner } from "@primer/react";

import { useT } from "../../../../i18n-provider";
import { formatCalendarDate } from "../../../../../lib/i18n";
import { useProjectShell } from "../shell-context";
import {
  humanizeCode,
  objectHref,
  objectTypeKey,
  payloadEntries,
  researchHref,
  researchObjects,
  type ObjectDetail,
  type ObjectRelationPayload,
  type ResearchOutline,
} from "./research-data";
import "./research.css";

/**
 * One scientific object, as a reader browses it (WEB-05 / G06).
 *
 * The page answers three questions a research object raises:
 *
 *   1. WHAT IS IT, AT WHICH VERSION. The version selector is a list of the
 *      object's whole immutable version log. Selecting an old version is a
 *      link (never a hidden state), and the header, the fields and the
 *      version marker all render THAT version's content — an old version is
 *      shown as it was, not as the object stands today.
 *   2. WHAT IS IT CONNECTED TO. Relations are resolved by the API with both
 *      endpoints named; each one links to the related object when the
 *      outline gives that object a coordinate, and renders as text when it
 *      does not (never a fabricated link).
 *   3. HOW DO I GET BACK. The breadcrumb returns to the research outline
 *      focused on this object, and a relation followed FROM another object
 *      leaves a return link carrying that object AND the version the reader
 *      had selected — so coming back keeps both.
 *
 * The branch coordinate is not in the web URL: it is resolved from the
 * outline (which is the as-of branch the outline itself shows), so a link
 * shared from an old version still reads on the branch that version lives
 * on.
 *
 * Read-only by construction: no form, no input, no mutation affordance.
 */

type VersionSelector = number | null | "invalid";

export function ObjectDetailView({
  objectId,
  version,
  from,
  fromVersion,
}: {
  objectId: string;
  version: VersionSelector;
  from: string;
  fromVersion: VersionSelector;
}) {
  const shell = useProjectShell();
  const t = useT();
  const [detail, setDetail] = useState<ObjectDetail | null>(null);
  const [outline, setOutline] = useState<ResearchOutline | null>(null);
  const [error, setError] = useState("");
  const [missing, setMissing] = useState(false);

  const outlineUrl = useMemo(
    () =>
      shell === null
        ? ""
        : `${shell.apiBaseUrl.replace(/\/+$/, "")}/api/v1/projects/${encodeURIComponent(shell.project.id)}/research`,
    [shell],
  );

  useEffect(() => {
    if (outlineUrl === "" || shell === null || version === "invalid") return;
    const base = shell.apiBaseUrl.replace(/\/+$/, "");
    const project = encodeURIComponent(shell.project.id);
    const controller = new AbortController();
    const read = async (): Promise<void> => {
      try {
        const outlineResponse = await fetch(outlineUrl, {
          credentials: "include",
          signal: controller.signal,
          headers: { accept: "application/json" },
        });
        if (!outlineResponse.ok) throw new Error(t("research.error.fallback"));
        const outlineBody = (await outlineResponse.json()) as ResearchOutline;
        setOutline(outlineBody);

        const coordinate = researchObjects(outlineBody).get(objectId);
        if (coordinate === undefined || coordinate.branchId === "") {
          setMissing(true);
          return;
        }

        const query = new URLSearchParams({ include: "versions,relations" });
        if (version !== null) query.set("version", String(version));
        const detailResponse = await fetch(
          `${base}/api/v1/projects/${project}/branches/${encodeURIComponent(coordinate.branchId)}/objects/${encodeURIComponent(objectId)}?${query.toString()}`,
          { credentials: "include", signal: controller.signal, headers: { accept: "application/json" } },
        );
        if (detailResponse.status === 404) {
          setMissing(true);
          return;
        }
        if (!detailResponse.ok) throw new Error(t("research.error.fallback"));
        setDetail((await detailResponse.json()) as ObjectDetail);
      } catch (cause: unknown) {
        if (cause instanceof DOMException && cause.name === "AbortError") return;
        setError(cause instanceof Error ? cause.message : t("research.error.fallback"));
      }
    };
    void read();
    return () => controller.abort();
  }, [outlineUrl, objectId, version, shell, t]);

  if (shell === null) return null;
  if (version === "invalid" || fromVersion === "invalid") {
    return (
      <div className="research-state" data-object-error="version">
        <h2>{t("research.object.versionInvalid.title")}</h2>
        <p>{t("research.object.versionInvalid.body")}</p>
      </div>
    );
  }
  if (error !== "") {
    return (
      <div className="research-state" data-object-error>
        <h2>{t("research.error.title")}</h2>
        <p>{error}</p>
      </div>
    );
  }
  if (missing) {
    return (
      <div className="research-state" data-object-notfound>
        <h2>{t("research.object.notFound.title")}</h2>
        <p>{t("research.object.notFound.body")}</p>
        <Link href={researchHref(shell.project.id, "questions")}>{t("research.object.backToResearch")}</Link>
      </div>
    );
  }
  if (detail === null || outline === null) {
    return (
      <div className="research-state">
        <Spinner aria-label={t("research.loading")} />
      </div>
    );
  }

  const projectId = shell.project.id;
  const index = researchObjects(outline);
  const selected = detail.selected_version_no;
  const versions = [...detail.versions].sort((a, b) => b.version_no - a.version_no);
  const fields = payloadEntries(detail.payload);
  const backTarget =
    from !== "" && from !== objectId && index.has(from)
      ? { id: from, title: index.get(from)?.title ?? from, version: fromVersion }
      : null;
  const typeKey = objectTypeKey(detail.object_type);

  return (
    <div
      className="research-object"
      data-research-object
      data-object-id={detail.id}
      data-object-type={detail.object_type}
      data-object-version={selected}
      data-object-branch={detail.branch_id}
    >
      <nav className="research-breadcrumb" aria-label={t("research.object.breadcrumb")}>
        <Link href={researchHref(projectId, "questions", detail.id)} data-back-to="research">
          {t("research.object.backToResearch")}
        </Link>
        {backTarget === null ? null : (
          <Link
            href={objectHref(projectId, backTarget.id, { version: backTarget.version ?? undefined })}
            data-back-to="object"
            data-back-object={backTarget.id}
            data-back-version={backTarget.version ?? undefined}
          >
            {t("research.object.backToObject", { title: backTarget.title })}
          </Link>
        )}
      </nav>

      <header className="research-object-header">
        <div>
          <span className={`research-pill type-${detail.object_type}`}>
            {typeKey === null ? humanizeCode(detail.object_type) : t(typeKey)}
          </span>
          <span className="research-object-id" data-object-state={detail.lifecycle_state}>
            {humanizeCode(detail.lifecycle_state)}
          </span>
          <h2>{detail.title}</h2>
          <p className="research-object-location">
            {detail.project_name} · {detail.branch_name}
          </p>
        </div>
        <div className="research-object-version">
          <span>{t("research.object.selectedVersion")}</span>
          <strong data-selected-version>{selected}</strong>
          <small>{t("research.object.ofVersions", { count: detail.versions.length })}</small>
        </div>
      </header>

      <section className="research-panel" aria-label={t("research.object.versions")} data-version-list>
        <header>
          <p className="research-eyebrow">{t("research.object.versionsEyebrow")}</p>
          <h3>{t("research.object.versions")}</h3>
          <p className="research-panel-note">{t("research.object.versionsNote")}</p>
        </header>
        <ol className="research-version-list">
          {versions.map((entry) => (
            <li key={entry.version_id} data-version-row={entry.version_no}>
              <Link
                className="research-version-link"
                href={objectHref(projectId, detail.id, {
                  version: entry.version_no,
                  from: backTarget === null ? undefined : backTarget.id,
                  fromVersion: backTarget?.version ?? undefined,
                })}
                data-version-option={entry.version_no}
                // data-version-current marks the object's HEAD (which version
                // is newest), not which version is being viewed. The two
                // differ the moment a reader selects an old version, and the
                // selection is already carried by aria-current and the URL.
                data-version-current={entry.current ? "true" : "false"}
                aria-current={entry.version_no === selected ? "true" : undefined}
              >
                <strong>{t("research.object.versionOption", { version: entry.version_no })}</strong>
                <span>{entry.title}</span>
                <small>
                  {humanizeCode(entry.lifecycle_state)} · {formatCalendarDate(entry.created_at)}
                  {entry.current ? ` · ${t("research.object.current")}` : ""}
                </small>
              </Link>
            </li>
          ))}
        </ol>
      </section>

      <section className="research-panel" aria-label={t("research.object.fields")} data-object-payload>
        <header>
          <p className="research-eyebrow">{t("research.object.fieldsEyebrow")}</p>
          <h3>{t("research.object.fields")}</h3>
          <p className="research-panel-note">
            {t("research.object.fieldsNote", { version: selected })}
          </p>
        </header>
        <dl className="research-fields">
          {fields.map((field) => (
            <div key={field.field}>
              <dt>{humanizeCode(field.field)}</dt>
              <dd>{field.value === "" ? t("research.object.emptyValue") : field.value}</dd>
            </div>
          ))}
        </dl>
      </section>

      <section className="research-panel" aria-label={t("research.object.relations")} data-relation-list>
        <header>
          <p className="research-eyebrow">{t("research.object.relationsEyebrow")}</p>
          <h3>{t("research.object.relations")}</h3>
          <p className="research-panel-note">{t("research.object.relationsNote")}</p>
        </header>
        {detail.relations.length === 0 ? (
          <p data-relations-empty>{t("research.object.noRelations")}</p>
        ) : (
          <ul className="research-relations">
            {detail.relations.map((relation) => (
              <RelationRow
                key={`${relation.relation_type}:${relation.other_object_id}:${relation.direction}`}
                relation={relation}
                projectId={projectId}
                detail={detail}
                known={index.has(relation.other_object_id)}
              />
            ))}
          </ul>
        )}
      </section>
    </div>
  );
}

function RelationRow({
  relation,
  projectId,
  detail,
  known,
}: {
  relation: ObjectRelationPayload;
  projectId: string;
  detail: ObjectDetail;
  known: boolean;
}) {
  const t = useT();
  const direction = relation.direction === "incoming" ? t("research.relation.incoming") : t("research.relation.outgoing");
  const typeKey = objectTypeKey(relation.other_object_type);
  const body = (
    <>
      <span className="research-relation-kind">{humanizeCode(relation.relation_type)}</span>
      <span className="research-relation-direction">{direction}</span>
      <span className={`research-pill type-${relation.other_object_type}`}>
        {typeKey === null ? humanizeCode(relation.other_object_type) : t(typeKey)}
      </span>
      <strong>{relation.other_title}</strong>
    </>
  );
  return (
    <li data-relation-direction={relation.direction} data-relation-type={relation.relation_type}>
      {known ? (
        <Link
          className="research-relation-link"
          href={objectHref(projectId, relation.other_object_id, {
            from: detail.id,
            fromVersion: detail.selected_version_no,
          })}
          data-relation-target={relation.other_object_id}
          data-relation-object-type={relation.other_object_type}
        >
          {body}
        </Link>
      ) : (
        <span className="research-relation-static" data-relation-object-type={relation.other_object_type}>
          {body}
        </span>
      )}
    </li>
  );
}
