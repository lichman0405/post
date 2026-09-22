"use client";

import { useEffect, useMemo, useState } from "react";
import Link from "next/link";
import { useRouter, useSearchParams } from "next/navigation";
import {
  AlertIcon,
  EyeIcon,
  FileMediaIcon,
  LawIcon,
  LinkIcon,
  PackageIcon,
  ShieldIcon,
  XIcon,
} from "@primer/octicons-react";
import { Spinner } from "@primer/react";

import { RightsPanel } from "@/app/components/rights-panel";
import { createAuthClient } from "@/lib/auth";
import {
  ApiError,
  createPublishClient,
  decodeCandidateQuery,
  type AssetCandidate,
  type ImpactPreview,
  type PrivateDependency,
  messageForPublishCode,
} from "@/lib/publish";
import { useProjectShell } from "@/app/(main)/projects/[id]/shell-context";
import "./publish-confirm.css";

type Phase =
  | { kind: "loading"; message?: string }
  | { kind: "error"; message: string }
  | { kind: "ready"; preview: ImpactPreview };

type Notice = { kind: "error" | "success"; text: string } | null;

function sectionTitle(label: string, icon: React.ReactNode): React.ReactNode {
  return (
    <h2 className="publish-confirm-section-title">
      <span aria-hidden="true">{icon}</span>
      {label}
    </h2>
  );
}

function blockerClass(dep: PrivateDependency): string {
  return dep.blocking
    ? "publish-confirm-item-detail publish-confirm-item-blocking"
    : "publish-confirm-item-detail";
}

/**
 * The publish/visibility confirmation page (T1103).
 *
 * This is the independent confirmation surface docs/06 §8 requires: a page
 * with its own URL, not a modal, whose main area is the six-category impact
 * list. It receives a complete publish candidate in the URL (the product's
 * release detail page builds it) and calls the preview/publish API pair.
 */
export default function PublishConfirmPage() {
  const shell = useProjectShell();
  const router = useRouter();
  const searchParams = useSearchParams();

  const [phase, setPhase] = useState<Phase>({ kind: "loading", message: "Loading preview…" });
  const [publishing, setPublishing] = useState(false);
  const [notice, setNotice] = useState<Notice>(null);

  const candidateParam = searchParams.get("candidate");
  const candidate: AssetCandidate | null = useMemo(() => {
    if (candidateParam === null) return null;
    try {
      return decodeCandidateQuery(candidateParam);
    } catch {
      return null;
    }
  }, [candidateParam]);

  const authClient = useMemo(
    () => (shell === null ? null : createAuthClient(shell.apiBaseUrl)),
    [shell],
  );
  const publishClient = useMemo(
    () =>
      shell === null
        ? null
        : createPublishClient(shell.apiBaseUrl, {
            csrfToken: () => authClient?.csrfToken() ?? null,
          }),
    [shell, authClient],
  );

  // Refresh the CSRF token on arrival: a reload can leave sessionStorage empty
  // while the cookie is still valid.
  useEffect(() => {
    if (authClient === null) return;
    authClient.session().catch(() => null);
  }, [authClient]);

  // Load the impact preview from the candidate.
  useEffect(() => {
    if (shell === null || publishClient === null || candidate === null) return;

    let cancelled = false;
    publishClient
      .preview(shell.project.id, candidate)
      .then((preview) => {
        if (!cancelled) setPhase({ kind: "ready", preview });
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        if (err instanceof ApiError) {
          setPhase({ kind: "error", message: messageForPublishCode(err.code) });
        } else {
          setPhase({ kind: "error", message: "Could not load the publish preview." });
        }
      });

    return () => {
      cancelled = true;
    };
  }, [shell, publishClient, candidate]);

  if (shell === null) {
    return null;
  }
  const { project, role } = shell;
  const isOwner = role === "owner";

  if (candidate === null) {
    return (
      <div className="publish-confirm" data-publish-confirm-page>
        <div className="publish-confirm-notice danger" role="alert" data-publish-error>
          <AlertIcon size={16} aria-hidden="true" />
          This page needs a publish candidate.
        </div>
        <Link className="publish-confirm-cancel" href={`/projects/${project.id}/releases`}>
          Back to Releases
        </Link>
      </div>
    );
  }

  async function handleConfirm() {
    if (
      publishClient === null ||
      candidate === null ||
      phase.kind !== "ready" ||
      !phase.preview.publishable ||
      publishing
    ) {
      return;
    }
    setPublishing(true);
    setNotice(null);
    try {
      const published = await publishClient.publish(
        project.id,
        candidate,
        crypto.randomUUID(),
      );
      router.push(`/assets/${encodeURIComponent(published.asset_pid)}/${encodeURIComponent(published.version)}`);
      return;
    } catch (err: unknown) {
      setPublishing(false);
      if (err instanceof ApiError) {
        if (err.status === 409 && err.code === "ASSET_PUBLISH_BLOCKED") {
          // The 409 body carries a complete preview; re-render it so the
          // blockers the server-side re-check found are shown by category.
          const blocked = err.body as { preview?: ImpactPreview } | undefined;
          if (blocked?.preview) {
            setPhase({ kind: "ready", preview: blocked.preview });
            setNotice({
              kind: "error",
              text: "The publication was refused when it was executed. The blockers above were found during the server-side re-check.",
            });
            return;
          }
        }
        setNotice({ kind: "error", text: messageForPublishCode(err.code) });
      } else {
        setNotice({ kind: "error", text: "Could not publish. Please try again." });
      }
    }
  }

  const canConfirm =
    phase.kind === "ready" &&
    phase.preview.publishable &&
    isOwner &&
    !publishing;

  const blockerCount =
    phase.kind === "ready"
      ? phase.preview.publish_blockers.length +
        phase.preview.rights_blockers.length +
        phase.preview.private_dependencies.filter((d) => d.blocking).length
      : 0;

  // The three families a refusal can come from. They are rendered as one
  // "Blockers" list because that is how docs/23 §4 asks a reader to check it,
  // but each entry keeps the family it came from: a blocking private
  // dependency is neither a candidate-check failure nor a rights statement,
  // and a refusal that only names "blockers" in general is the generic message
  // docs/06 §8 forbids when it replaces the impact list.
  const blockingPrivateDependencies =
    phase.kind === "ready"
      ? phase.preview.private_dependencies.filter((d) => d.blocking)
      : [];
  const blockers: { category: string }[] =
    phase.kind === "ready"
      ? (
          [
            ...phase.preview.publish_blockers.map(() => ({ category: "candidate check" })),
            ...phase.preview.rights_blockers.map(() => ({ category: "rights" })),
            ...blockingPrivateDependencies.map(() => ({ category: "private dependency" })),
          ] as { category: string }[]
        )
      : [];
  const blockerCategories = [...new Set(blockers.map((b) => b.category))];

  return (
    <div className="publish-confirm" data-publish-confirm-page>
      <nav className="publish-confirm-breadcrumb" aria-label="Breadcrumb">
        {/* The reader arrived from the version's page, so the trail leads
            back there — not to Releases, which is not where the candidate
            came from. The asset is named by the pid the candidate carries,
            which is the only identity this page holds; a candidate that
            names no asset has no page to go back to, and the trail falls
            back to the project. */}
        {candidate?.asset_pid ? (
          <>
            <Link href="/assets">Assets</Link>
            {" / "}
            <Link href={`/assets/${candidate.asset_pid}`} data-publish-back-asset>
              {candidate.asset_pid}
            </Link>
            {" / "}
            <span>Publish</span>
          </>
        ) : (
          <>
            <Link href={`/projects/${project.id}`} data-publish-back-project>
              {project.name}
            </Link>
            {" / "}
            <span>Publish</span>
          </>
        )}
      </nav>

      <header className="publish-confirm-head">
        <h1 className="publish-confirm-title">Publish asset</h1>
        <p className="publish-confirm-lead">
          Review what would become public before confirming. This page is the
          explicit human checkpoint for any private→public publication.
        </p>
      </header>

      {notice?.kind === "error" ? (
        <div className="publish-confirm-notice danger" role="alert" data-publish-error>
          <AlertIcon size={16} aria-hidden="true" />
          {notice.text}
        </div>
      ) : null}

      {!isOwner ? (
        <div className="publish-confirm-notice info" data-publish-role-notice>
          <ShieldIcon size={16} aria-hidden="true" />
          Only the project owner can confirm a publication. The preview is shown
          because you can read the project, but the confirm action is not
          available.
        </div>
      ) : null}

      {phase.kind === "loading" ? (
        <div className="publish-confirm-spinner">
          <Spinner aria-label={phase.message ?? "Loading preview"} />
        </div>
      ) : phase.kind === "error" ? (
        <div className="publish-confirm-notice danger" role="alert" data-publish-error>
          <AlertIcon size={16} aria-hidden="true" />
          {phase.message}
        </div>
      ) : (
        <>
          <section className="publish-confirm-section" data-publish-section="summary">
            {sectionTitle("Summary", <PackageIcon size={16} />)}
            <ul className="publish-confirm-fact-list">
              <li className="publish-confirm-fact">
                <span className="publish-confirm-fact-label">Project</span>
                <span className="publish-confirm-fact-value">{project.name}</span>
              </li>
              <li className="publish-confirm-fact">
                <span className="publish-confirm-fact-label">Version</span>
                <span className="publish-confirm-fact-value">{phase.preview.version}</span>
              </li>
              <li className="publish-confirm-fact">
                <span className="publish-confirm-fact-label">Target visibility</span>
                <span className="publish-confirm-fact-value">
                  {phase.preview.target_visibility}
                </span>
              </li>
              <li className="publish-confirm-fact">
                <span className="publish-confirm-fact-label">Publishable</span>
                <span className="publish-confirm-fact-value">
                  {phase.preview.publishable ? "Yes" : `No (${blockerCount} blocker${blockerCount === 1 ? "" : "s"})`}
                </span>
              </li>
            </ul>
          </section>

          <section className="publish-confirm-section" data-publish-section="objects">
            {sectionTitle("Objects that would become public", <EyeIcon size={16} />)}
            {phase.preview.objects.length === 0 ? (
              <p className="publish-confirm-section-empty">No object versions listed.</p>
            ) : (
              <ul className="publish-confirm-list">
                {phase.preview.objects.map((o) => (
                  <li className="publish-confirm-list-item" key={o.object_version_id}>
                    <div className="publish-confirm-item-main">
                      <span className="publish-confirm-item-label">{o.title ?? "Untitled object"}</span>
                      <span className="publish-confirm-meta-value">{o.object_version_id}</span>
                    </div>
                    <p className="publish-confirm-item-meta">
                      current visibility: {o.current_visibility || "unknown"}
                    </p>
                  </li>
                ))}
              </ul>
            )}
          </section>

          <section className="publish-confirm-section" data-publish-section="metadata">
            {sectionTitle("Metadata that would become public", <FileMediaIcon size={16} />)}
            {phase.preview.metadata.length === 0 ? (
              <p className="publish-confirm-section-empty">No metadata keys declared.</p>
            ) : (
              <ul className="publish-confirm-list">
                {phase.preview.metadata.map((m) => (
                  <li className="publish-confirm-list-item" key={m.key}>
                    <div className="publish-confirm-item-main">
                      <span className="publish-confirm-item-label">{m.key}</span>
                      <span className="publish-confirm-item-meta">
                        {JSON.stringify(m.value)}
                      </span>
                    </div>
                  </li>
                ))}
              </ul>
            )}
          </section>

          <section className="publish-confirm-section" data-publish-section="blobs">
            {sectionTitle("Blob access", <FileMediaIcon size={16} />)}
            {phase.preview.blobs.length === 0 ? (
              <p className="publish-confirm-section-empty">No blobs declared.</p>
            ) : (
              <ul className="publish-confirm-list">
                {phase.preview.blobs.map((b) => (
                  <li className="publish-confirm-list-item" key={b.blob_id}>
                    <div className="publish-confirm-item-main">
                      <span className="publish-confirm-item-label">{b.blob_id}</span>
                      <span className="publish-confirm-item-meta">
                        current {b.current_access}, declared {b.declared_access}
                      </span>
                    </div>
                    {!b.resolved ? (
                      <p className="publish-confirm-item-detail">
                        This blob identifier does not resolve to a stored blob.
                      </p>
                    ) : null}
                  </li>
                ))}
              </ul>
            )}
          </section>

          <section className="publish-confirm-section" data-publish-section="dependencies">
            {sectionTitle("Dependencies and origin refs", <LinkIcon size={16} />)}
            {phase.preview.refs.length === 0 && phase.preview.dependencies.length === 0 ? (
              <p className="publish-confirm-section-empty">No refs or dependency pins declared.</p>
            ) : (
              <ul className="publish-confirm-list">
                {phase.preview.refs.map((r) => (
                  <li className="publish-confirm-list-item" key={`ref:${r.ref}`}>
                    <div className="publish-confirm-item-main">
                      <span className="publish-confirm-item-label">{r.ref}</span>
                      <span className="publish-confirm-item-meta">{r.kind ?? "unknown kind"}</span>
                    </div>
                    <p className="publish-confirm-item-detail">
                      {r.resolved ? `resolved, visibility ${r.current_visibility || "unknown"}` : "unresolved"}
                    </p>
                  </li>
                ))}
                {phase.preview.dependencies.map((d) => (
                  <li className="publish-confirm-list-item" key={`pin:${d.pin}`}>
                    <div className="publish-confirm-item-main">
                      <span className="publish-confirm-item-label">{d.pin}</span>
                      <span className="publish-confirm-item-meta">
                        {d.resolved ? `resolved, ${d.current_visibility || "unknown"}` : "unresolved"}
                      </span>
                    </div>
                  </li>
                ))}
              </ul>
            )}
            {phase.preview.private_dependencies.length > 0 ? (
              <div data-publish-private-dependencies>
                <h3 className="publish-confirm-subsection-title">
                  Private dependencies
                </h3>
                <ul className="publish-confirm-list">
                  {phase.preview.private_dependencies.map((d) => (
                    <li className="publish-confirm-list-item" key={`priv:${d.kind}:${d.ref}`}>
                      <div className="publish-confirm-item-main">
                        <span className="publish-confirm-item-label">{d.ref}</span>
                        <span className="publish-confirm-item-meta">{d.kind}</span>
                      </div>
                      <p className={blockerClass(d)}>{d.detail}</p>
                    </li>
                  ))}
                </ul>
              </div>
            ) : null}
          </section>

          <section className="publish-confirm-section" data-publish-section="rights">
            {sectionTitle("Rights / license", <LawIcon size={16} />)}
            <RightsPanel rights={candidate?.rights ?? null} />
          </section>

          <section className="publish-confirm-section" data-publish-section="attestation">
            {sectionTitle("Private evidence / attestation", <ShieldIcon size={16} />)}
            {/*
              docs/06 §8 names a sixth category. This platform has no
              attestation feature to list under it — specs/api/openapi.yaml and
              specs/mcp/tools.json have no `attest` route or tool, and
              internal/application/researchprofile/doc.go records why (the
              specification defines neither who may issue one, nor the
              disclosure-level vocabulary, nor when the leak count starts). An
              empty <ul> here would read as "checked, found none", which is a
              claim this page cannot make, so the category is rendered as the
              state of the platform instead.
            */}
            <p
              className="publish-confirm-attestation-status"
              data-publish-attestation-notice
              data-publish-attestation-status="not-implemented"
            >
              No attestation can be published on this platform in this version:
              the product has no private-evidence / attestation feature, so
              there is no attestation for this publication to disclose or
              withhold.{" "}
              {candidate?.rights === null || candidate?.rights === undefined
                ? "This version carries no rights declaration at all, so the Rights block above names none and the publication has nothing to declare."
                : "The rights declaration above is the only rights/license statement this version carries."}
            </p>
          </section>

          {blockers.length > 0 ? (
            <section
              className="publish-confirm-section"
              data-publish-section="blockers"
              data-publish-blocker-count={blockers.length}
            >
              {sectionTitle("Blockers", <XIcon size={16} />)}
              <p className="publish-confirm-section-empty">
                Any one of these refuses the publication. The same refusal is
                made again inside the publish, which re-runs this impact
                preview over the state at that moment.
              </p>
              <ul className="publish-confirm-list">
                {phase.preview.publish_blockers.map((b, i) => (
                  <li
                    className="publish-confirm-list-item"
                    key={`pb-${i}`}
                    data-publish-blocker-kind="publish"
                  >
                    <div className="publish-confirm-item-main">
                      <span className="publish-confirm-item-label">{b.code}</span>
                      <span className="publish-confirm-item-meta">{b.field || "candidate"}</span>
                    </div>
                    <p className="publish-confirm-item-detail">{b.detail}</p>
                  </li>
                ))}
                {phase.preview.rights_blockers.map((b, i) => (
                  <li
                    className="publish-confirm-list-item"
                    key={`rb-${i}`}
                    data-publish-blocker-kind="rights"
                  >
                    <div className="publish-confirm-item-main">
                      <span className="publish-confirm-item-label">{b.code}</span>
                      <span className="publish-confirm-item-meta">{b.field || "rights"}</span>
                    </div>
                    <p className="publish-confirm-item-detail">{b.detail}</p>
                  </li>
                ))}
                {blockingPrivateDependencies.map((d, i) => (
                  <li
                    className="publish-confirm-list-item"
                    key={`bd-${i}`}
                    data-publish-blocker-kind="private_dependency"
                    data-publish-blocker-ref={d.ref}
                  >
                    <div className="publish-confirm-item-main">
                      <span className="publish-confirm-item-label">{d.ref}</span>
                      <span className="publish-confirm-item-meta">
                        {`private dependency (${d.kind})`}
                      </span>
                    </div>
                    <p className="publish-confirm-item-detail publish-confirm-item-blocking">
                      {d.detail}
                    </p>
                  </li>
                ))}
              </ul>
            </section>
          ) : null}

          <div className="publish-confirm-actions">
            {canConfirm ? (
              <button
                type="button"
                className="publish-confirm-submit"
                data-publish-confirm
                onClick={handleConfirm}
                disabled={publishing}
              >
                {publishing ? "Publishing…" : "Confirm and publish"}
              </button>
            ) : phase.kind === "ready" && !phase.preview.publishable ? (
              // No confirm control at all when the preview refuses: the
              // publication is unreachable from this page, not merely
              // discouraged. The categories it was refused over are named, so
              // the reader knows which list to read.
              <span className="publish-confirm-section-empty" data-publish-blocked-notice>
                {`This publication cannot be confirmed. It is refused over ` +
                  `${blockers.length} item${blockers.length === 1 ? "" : "s"} ` +
                  `(${blockerCategories.join(", ")}); see the blockers section.`}
              </span>
            ) : null}
            <Link
              className="publish-confirm-cancel"
              href={`/projects/${project.id}/releases`}
            >
              Cancel
            </Link>
          </div>
        </>
      )}
    </div>
  );
}
