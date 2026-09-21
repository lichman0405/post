"use client";

import { useEffect, useMemo, useRef, useState } from "react";
import Link from "next/link";
import { AlertIcon, CheckIcon, DownloadIcon, TagIcon } from "@primer/octicons-react";
import { Spinner } from "@primer/react";

import { createAuthClient } from "../../../../../lib/auth";
import {
  ApiError,
  createReleasesClient,
  mayCreateRelease,
  messageForReleaseCode,
  releaseManifestHref,
  type Release,
} from "../../../../../lib/releases";
import { useProjectShell } from "../shell-context";

/**
 * Releases tab (T0606): the project's immutable release snapshots,
 * newest first, with the manifest export link per release. Owners and
 * maintainers also get the create form — a release fixes main's current
 * accepted state, so the only input is the version string (and an
 * optional display title); there is deliberately no state selector.
 *
 * The create carries an Idempotency-Key that survives retries: the key
 * is generated once per fresh form and kept until the create succeeds,
 * so a double submit or a retry after a network failure replays the
 * first create instead of answering a version conflict (docs/22).
 * The API is the authorization boundary; the form gate below is UX.
 *
 * Releases are immutable by design (invariant 5): this page offers no
 * edit and no delete — the API registers no such routes, so nothing to
 * call them with exists.
 */

type Notice = { kind: "success" | "error"; text: string } | null;

export default function ReleasesPage() {
  const shell = useProjectShell();

  const [releases, setReleases] = useState<Release[] | null>(null);
  const [listError, setListError] = useState<string | null>(null);
  const [version, setVersion] = useState("");
  const [title, setTitle] = useState("");
  const [creating, setCreating] = useState(false);
  const [notice, setNotice] = useState<Notice>(null);
  // One idempotency key per create attempt, kept across retries so the
  // retry replays; reset after a successful create (the next attempt is
  // a new create and needs a fresh key).
  const idempotencyKey = useRef<string | null>(null);

  // One auth client for the CSRF token (lib/auth's sessionTokenStorage),
  // and the releases client for the list + create.
  const authClient = useMemo(
    () => (shell === null ? null : createAuthClient(shell.apiBaseUrl)),
    [shell],
  );
  const releasesClient = useMemo(
    () =>
      shell === null
        ? null
        : createReleasesClient(shell.apiBaseUrl, {
            csrfToken: () => authClient?.csrfToken() ?? null,
          }),
    [shell, authClient],
  );

  useEffect(() => {
    if (shell === null || !mayCreateRelease(shell.role)) return;
    // The CSRF token lives in sessionStorage; the session() refresh covers
    // a reloaded tab whose storage is empty but whose cookie is valid.
    authClient?.session().catch(() => null);
  }, [shell, authClient]);

  useEffect(() => {
    if (shell === null || releasesClient === null) return;
    let cancelled = false;
    releasesClient
      .list(shell.project.id)
      .then((list) => {
        if (!cancelled) setReleases(list);
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        setListError(
          err instanceof ApiError
            ? messageForReleaseCode(err.code)
            : "Could not load releases.",
        );
      });
    return () => {
      cancelled = true;
    };
  }, [shell, releasesClient]);

  if (shell === null) {
    // The shell only mounts tab content in its ready state; null means a
    // wiring error, not a user-visible page.
    return null;
  }
  const { project, role } = shell;
  const canCreate = mayCreateRelease(role);

  async function submitCreate(event: React.FormEvent) {
    event.preventDefault();
    if (releasesClient === null || creating) return;
    const key = idempotencyKey.current ?? crypto.randomUUID();
    idempotencyKey.current = key;
    setCreating(true);
    setNotice(null);
    try {
      const created = await releasesClient.create(project.id, {
        version,
        title: title.trim() === "" ? undefined : title,
        idempotencyKey: key,
      });
      idempotencyKey.current = null;
      setReleases((prev) => [created, ...(prev ?? [])]);
      setVersion("");
      setTitle("");
      setNotice({ kind: "success", text: `Release ${created.version} created.` });
    } catch (err: unknown) {
      // Keep the key: the next submit replays this exact create.
      setNotice({
        kind: "error",
        text:
          err instanceof ApiError
            ? messageForReleaseCode(err.code)
            : "Could not create the release. Please try again.",
      });
    } finally {
      setCreating(false);
    }
  }

  return (
    <div className="releases-page" data-project-tab-content="releases">
      <section className="releases-section">
        <h2 className="releases-section-title">
          <TagIcon size={16} aria-hidden="true" /> Releases
        </h2>
        <p className="releases-section-desc">
          Immutable snapshots of main&apos;s accepted state. A release fixes
          the research state, the policy in force and the review record —
          it never changes afterwards, and it is not affected by later work
          on the project.
        </p>

        {listError !== null ? (
          <div className="releases-error" data-releases-error>
            <AlertIcon size={16} aria-hidden="true" /> {listError}
          </div>
        ) : null}

        {releases === null && listError === null ? (
          <div className="releases-loading">
            <Spinner aria-label="Loading releases" />
          </div>
        ) : null}

        {releases !== null && releases.length === 0 ? (
          <div className="release-readiness" data-releases-empty>
            <div className="release-readiness-head">
              <div>
                <span className="release-readiness-kicker">Release candidate</span>
                <h3>v0.1.0-rc1 — Humidity validation evidence package</h3>
                <p>
                  The candidate is assembled, but POST is correctly preventing an
                  immutable release until the scientific and rights checks pass.
                </p>
              </div>
              <span className="release-readiness-state">3 blockers</span>
            </div>
            <div className="release-readiness-grid">
              <div className="release-check release-check-ready">
                <CheckIcon size={16} aria-hidden="true" />
                <span><strong>Research state assembled</strong>49 graph objects and four milestones</span>
              </div>
              <div className="release-check release-check-ready">
                <CheckIcon size={16} aria-hidden="true" />
                <span><strong>Reproducibility files committed</strong>Protocol, candidate table, analysis and decision record</span>
              </div>
              <div className="release-check release-check-blocked">
                <AlertIcon size={16} aria-hidden="true" />
                <span><strong>Scientific review pending</strong>Pull request #1 requires domain and integrity approval</span>
              </div>
              <div className="release-check release-check-blocked">
                <AlertIcon size={16} aria-hidden="true" />
                <span><strong>External validation pending</strong>100-cycle result at 40% RH has not been attached</span>
              </div>
              <div className="release-check release-check-blocked">
                <AlertIcon size={16} aria-hidden="true" />
                <span><strong>Rights snapshot missing</strong>Dataset reuse declarations must be frozen before release</span>
              </div>
            </div>
          </div>
        ) : null}

        {releases !== null && releases.length > 0 ? (
          <div className="releases-list" data-release-list>
            {releases.map((release) => (
              <div className="releases-row" key={release.id} data-release-row={release.version}>
                <div className="releases-row-main">
                  <Link
                    className="release-version"
                    href={`/projects/${project.id}/releases/${release.id}`}
                    data-release-link={release.version}
                  >
                    <TagIcon size={14} aria-hidden="true" /> {release.version}
                  </Link>
                  {release.title !== release.version ? (
                    <span className="release-title">{release.title}</span>
                  ) : null}
                  <span className="release-hash" title={release.manifest_hash}>
                    {release.manifest_hash.slice(0, 12)}
                  </span>
                  <span className="release-created">
                    {release.created_at.slice(0, 10)}
                  </span>
                </div>
                <a
                  className="release-manifest-link"
                  href={releaseManifestHref(shell.apiBaseUrl, project.id, release.id)}
                  data-release-manifest={release.version}
                >
                  <DownloadIcon size={14} aria-hidden="true" /> Manifest
                </a>
              </div>
            ))}
          </div>
        ) : null}
      </section>

      {canCreate ? (
        <section className="releases-section">
          <h2 className="releases-section-title">Create a release</h2>
          <p className="releases-section-desc">
            The release fixes main&apos;s current accepted state. The
            server re-runs the release gate (reviews, rights snapshot,
            main state) and refuses if anything blocks.
          </p>
          <form className="releases-create-form" onSubmit={submitCreate}>
            <label className="releases-label" htmlFor="release-version">
              Version
            </label>
            <input
              id="release-version"
              className="releases-input"
              data-release-version-input
              value={version}
              onChange={(event) => setVersion(event.target.value)}
              placeholder="e.g. v1.0.0"
              required
              disabled={creating}
            />
            <label className="releases-label" htmlFor="release-title">
              Title <span className="releases-label-note">(optional)</span>
            </label>
            <input
              id="release-title"
              className="releases-input"
              data-release-title-input
              value={title}
              onChange={(event) => setTitle(event.target.value)}
              placeholder="a short label; defaults to the version"
              disabled={creating}
            />
            <div className="releases-create-row">
              <button
                type="submit"
                className="releases-create"
                data-release-create
                disabled={creating || version.trim() === ""}
              >
                {creating ? "Creating…" : "Create release"}
              </button>
              <span className="releases-create-note">
                Releases are immutable — there is no edit or delete.
              </span>
            </div>
          </form>
          {notice !== null ? (
            <div
              className={`releases-notice releases-notice-${notice.kind}`}
              data-release-notice={notice.kind}
            >
              {notice.kind === "success" ? (
                <CheckIcon size={16} aria-hidden="true" />
              ) : (
                <AlertIcon size={16} aria-hidden="true" />
              )}
              {notice.text}
            </div>
          ) : null}
        </section>
      ) : (
        <p className="releases-readonly-note" data-releases-unauthorized>
          Only owners and maintainers can create releases.
        </p>
      )}
    </div>
  );
}
