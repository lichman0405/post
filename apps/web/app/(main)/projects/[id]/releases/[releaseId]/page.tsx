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
import { useProjectShell } from "../../shell-context";

/**
 * Release detail (T0606): one immutable snapshot's fixed facts and the
 * manifest export. The manifest link is a plain anchor download — the
 * browser carries the session cookie, and the API answers the canonical
 * document bytes (verified against the stored hash server-side).
 *
 * Strictly read-only: an immutable release has no edit and no delete,
 * so the page offers neither (and the API registers no such routes).
 */
export default function ReleaseDetailPage() {
  const shell = useProjectShell();
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
            : "Could not load this release.",
        );
      });
    return () => {
      cancelled = true;
    };
  }, [shell, releasesClient, releaseId]);

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
        <h2 className="release-detail-state-title">Release unavailable</h2>
        <p className="release-detail-state-desc">{error}</p>
        <Link className="project-state-button" href={`/projects/${project.id}/releases`}>
          Back to Releases
        </Link>
      </div>
    );
  }

  if (release === null) {
    return (
      <div className="release-detail-state">
        <Spinner aria-label="Loading release" />
      </div>
    );
  }

  return (
    <div className="release-detail" data-release-detail={release.version}>
      <p className="release-detail-breadcrumb">
        <Link href={`/projects/${project.id}/releases`}>Releases</Link> / {release.version}
      </p>
      <div className="release-detail-title-row">
        <h2 className="release-detail-title">
          <TagIcon size={16} aria-hidden="true" /> {release.title}
        </h2>
        <a
          className="release-manifest-link"
          href={releaseManifestHref(shell.apiBaseUrl, project.id, release.id)}
          data-release-manifest={release.version}
        >
          <DownloadIcon size={14} aria-hidden="true" /> Download manifest
        </a>
      </div>
      <p className="release-detail-note">
        Immutable snapshot — the state, policy and review record fixed
        here never change, and later project work does not affect this
        release. There is no edit or delete.
      </p>
      <dl className="release-detail-facts">
        <dt>Version</dt>
        <dd>{release.version}</dd>
        <dt>State</dt>
        <dd>
          <code>{release.state_id}</code>
        </dd>
        <dt>Project policy</dt>
        <dd>{release.policy_version_id !== null ? <code>{release.policy_version_id}</code> : "none"}</dd>
        <dt>Organization policy</dt>
        <dd>
          {release.org_policy_version_id !== null ? (
            <code>{release.org_policy_version_id}</code>
          ) : (
            "none"
          )}
        </dd>
        <dt>Manifest hash</dt>
        <dd>
          <code data-release-manifest-hash>{release.manifest_hash}</code>
        </dd>
        <dt>Created</dt>
        <dd>{release.created_at.slice(0, 10)}</dd>
        <dt>Created by</dt>
        <dd>{release.created_by}</dd>
      </dl>
    </div>
  );
}
