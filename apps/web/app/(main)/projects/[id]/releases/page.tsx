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
import { useT } from "../../../../i18n-provider";
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
  const t = useT();

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
            : t("releases.error.load"),
        );
      });
    return () => {
      cancelled = true;
    };
    // `t` belongs in the deps: the fallback sentence is resolved in the
    // catch, so switching language must re-run the fetch's error path.
  }, [shell, releasesClient, t]);

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
      setNotice({ kind: "success", text: t("releases.created", { version: created.version }) });
    } catch (err: unknown) {
      // Keep the key: the next submit replays this exact create.
      setNotice({
        kind: "error",
        text:
          err instanceof ApiError
            ? messageForReleaseCode(err.code)
            : t("releases.error.create"),
      });
    } finally {
      setCreating(false);
    }
  }

  return (
    <div className="releases-page" data-project-tab-content="releases">
      <section className="releases-section">
        <h2 className="releases-section-title">
          <TagIcon size={16} aria-hidden="true" /> {t("releases.title")}
        </h2>
        <p className="releases-section-desc">
          {t("releases.intro")}
        </p>

        {listError !== null ? (
          <div className="releases-error" data-releases-error>
            <AlertIcon size={16} aria-hidden="true" /> {listError}
          </div>
        ) : null}

        {releases === null && listError === null ? (
          <div className="releases-loading">
            <Spinner aria-label={t("releases.loading")} />
          </div>
        ) : null}

        {releases !== null && releases.length === 0 ? (
          <div className="release-readiness" data-releases-empty>
            <div className="release-readiness-head">
              <div>
                <span className="release-readiness-kicker">{t("releases.candidate.kicker")}</span>
                <h3>{t("releases.candidate.title")}</h3>
                <p>
                  {t("releases.candidate.body")}
                </p>
              </div>
              <span className="release-readiness-state">{t("releases.candidate.blockers", { count: 3 })}</span>
            </div>
            <div className="release-readiness-grid">
              <div className="release-check release-check-ready">
                <CheckIcon size={16} aria-hidden="true" />
                <span><strong>{t("releases.check.state.title")}</strong>{t("releases.check.state.body")}</span>
              </div>
              <div className="release-check release-check-ready">
                <CheckIcon size={16} aria-hidden="true" />
                <span><strong>{t("releases.check.files.title")}</strong>{t("releases.check.files.body")}</span>
              </div>
              <div className="release-check release-check-blocked">
                <AlertIcon size={16} aria-hidden="true" />
                <span><strong>{t("releases.check.review.title")}</strong>{t("releases.check.review.body")}</span>
              </div>
              <div className="release-check release-check-blocked">
                <AlertIcon size={16} aria-hidden="true" />
                <span><strong>{t("releases.check.validation.title")}</strong>{t("releases.check.validation.body")}</span>
              </div>
              <div className="release-check release-check-blocked">
                <AlertIcon size={16} aria-hidden="true" />
                <span><strong>{t("releases.check.rights.title")}</strong>{t("releases.check.rights.body")}</span>
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
                  <DownloadIcon size={14} aria-hidden="true" /> {t("releases.manifest")}
                </a>
              </div>
            ))}
          </div>
        ) : null}
      </section>

      {canCreate ? (
        <section className="releases-section">
          <h2 className="releases-section-title">{t("releases.create.title")}</h2>
          <p className="releases-section-desc">
            {t("releases.create.body")}
          </p>
          <form className="releases-create-form" onSubmit={submitCreate}>
            <label className="releases-label" htmlFor="release-version">
              {t("releases.create.version")}
            </label>
            <input
              id="release-version"
              className="releases-input"
              data-release-version-input
              value={version}
              onChange={(event) => setVersion(event.target.value)}
              placeholder={t("releases.create.versionPlaceholder")}
              required
              disabled={creating}
            />
            <label className="releases-label" htmlFor="release-title">
              {t("releases.create.titleLabel")} <span className="releases-label-note">{t("releases.create.optional")}</span>
            </label>
            <input
              id="release-title"
              className="releases-input"
              data-release-title-input
              value={title}
              onChange={(event) => setTitle(event.target.value)}
              placeholder={t("releases.create.titlePlaceholder")}
              disabled={creating}
            />
            <div className="releases-create-row">
              <button
                type="submit"
                className="releases-create"
                data-release-create
                disabled={creating || version.trim() === ""}
              >
                {creating ? t("releases.create.submitting") : t("releases.create.submit")}
              </button>
              <span className="releases-create-note">
                {t("releases.create.note")}
              </span>
            </div>
          </form>
          {notice !== null ? (
            // T1104: creating a release is a write; this notice ("Release
            // v… created." vs. the failure text) is the only result the page
            // renders. Polite on success, alert on failure.
            <div
              className={`releases-notice releases-notice-${notice.kind}`}
              data-release-notice={notice.kind}
              role={notice.kind === "success" ? "status" : "alert"}
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
          {t("releases.readonly")}
        </p>
      )}
    </div>
  );
}
