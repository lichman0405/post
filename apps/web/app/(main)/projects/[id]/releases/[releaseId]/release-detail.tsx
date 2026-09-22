"use client";

import { useEffect, useMemo, useState } from "react";
import Link from "next/link";
import { useParams } from "next/navigation";
import { AlertIcon, DownloadIcon, TagIcon } from "@primer/octicons-react";
import { Spinner } from "@primer/react";

import {
  ApiError,
  createReleasesClient,
  messageForReleaseCode,
  releaseManifestHref,
  type Release,
} from "../../../../../../lib/releases";
import { Sidebar } from "@post/ui";
import { useT } from "../../../../../i18n-provider";
import { useProjectShell } from "../../shell-context";

/**
 * Release detail (T0606): one immutable snapshot's fixed facts and the
 * manifest export. The manifest link is a plain anchor download — the
 * browser carries the session cookie, and the API answers the canonical
 * document bytes (verified against the stored hash server-side).
 *
 * Strictly read-only: an immutable release has no edit and no delete,
 * so the page offers neither (and the API registers no such routes).
 *
 * This is the client half of the route. T0801 gave the segment a server
 * page (page.tsx) so the release could carry a crawlable <head> of its
 * own; the body below is unchanged, and it is still the component that
 * renders whatever the API answers THIS reader — the metadata fetch never
 * touches a session and never decides what the page shows.
 */
export default function ReleaseDetail() {
  const shell = useProjectShell();
  const t = useT();
  const params = useParams<{ id: string; releaseId: string }>();
  const releaseId = params.releaseId;

  const [release, setRelease] = useState<Release | null>(null);
  const [error, setError] = useState<string | null>(null);

  const releasesClient = useMemo(
    () => (shell === null ? null : createReleasesClient(shell.apiBaseUrl)),
    [shell],
  );

  useEffect(() => {
    if (shell === null || releasesClient === null) return;
    let cancelled = false;
    releasesClient
      .get(shell.project.id, releaseId)
      .then((loaded) => {
        if (!cancelled) setRelease(loaded);
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        setError(
          err instanceof ApiError
            ? messageForReleaseCode(err.code)
            : t("release.error.load"),
        );
      });
    return () => {
      cancelled = true;
    };
    // `t` in the deps: the fallback sentence is resolved in the catch.
  }, [shell, releasesClient, releaseId, t]);

  if (shell === null) {
    // The shell only mounts tab content in its ready state; null means a
    // wiring error, not a user-visible page.
    return null;
  }
  const { project } = shell;

  if (error !== null) {
    return (
      <div className="release-detail-state" data-release-notfound>
        <div className="release-detail-state-icon" aria-hidden="true">
          <AlertIcon size={24} />
        </div>
        <h2 className="release-detail-state-title">{t("release.error.title")}</h2>
        <p className="release-detail-state-desc">{error}</p>
        <Link className="project-state-button" href={`/projects/${project.id}/releases`}>
          {t("release.back")}
        </Link>
      </div>
    );
  }

  if (release === null) {
    return (
      <div className="release-detail-state">
        <Spinner aria-label={t("release.loading")} />
      </div>
    );
  }

  return (
    <div className="release-detail" data-release-detail={release.version}>
      {/* T1101: the trail is the shared Sidebar. The current segment is a
          plain span (this is the page you are on) rather than a link; the
          spacer before it is the component's, not a hand-typed " / ". */}
      <Sidebar
        className="release-detail-breadcrumb"
        label={t("release.breadcrumb")}
        tone="accent"
        crumbs={[
          {
            key: "releases",
            content: <Link href={`/projects/${project.id}/releases`}>{t("releases.title")}</Link>,
          },
          { key: "version", content: release.version },
        ]}
      />
      <div className="release-detail-title-row">
        <h2 className="release-detail-title">
          <TagIcon size={16} aria-hidden="true" /> {release.title}
        </h2>
        <a
          className="release-manifest-link"
          href={releaseManifestHref(shell.apiBaseUrl, project.id, release.id)}
          data-release-manifest={release.version}
        >
          <DownloadIcon size={14} aria-hidden="true" /> {t("release.downloadManifest")}
        </a>
      </div>
      <p className="release-detail-note">
        {t("release.immutableNote")}
      </p>
      <dl className="release-detail-facts">
        <dt>{t("release.fact.version")}</dt>
        <dd>{release.version}</dd>
        <dt>{t("release.fact.state")}</dt>
        <dd>
          <code>{release.state_id}</code>
        </dd>
        <dt>{t("release.fact.projectPolicy")}</dt>
        <dd>{release.policy_version_id !== null ? <code>{release.policy_version_id}</code> : t("common.none")}</dd>
        <dt>{t("release.fact.orgPolicy")}</dt>
        <dd>
          {release.org_policy_version_id !== null ? (
            <code>{release.org_policy_version_id}</code>
          ) : (
            t("common.none")
          )}
        </dd>
        <dt>{t("release.fact.manifestHash")}</dt>
        <dd>
          <code data-release-manifest-hash>{release.manifest_hash}</code>
        </dd>
        <dt>{t("release.fact.created")}</dt>
        <dd>{release.created_at.slice(0, 10)}</dd>
        <dt>{t("release.fact.createdBy")}</dt>
        <dd>{release.created_by}</dd>
      </dl>
    </div>
  );
}
