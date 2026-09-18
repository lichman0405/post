"use client";

import { useEffect, useMemo, useState } from "react";
import Link from "next/link";
import {
  BeakerIcon,
  GitBranchIcon,
  LawIcon,
  LinkIcon,
  PackageIcon,
  ShieldCheckIcon,
  TagIcon,
  VersionsIcon,
} from "@primer/octicons-react";
import { Spinner } from "@primer/react";

import { RightsPanel } from "../../components/rights-panel";
import {
  ApiError,
  assetTypeLabel,
  createAssetsClient,
  creatorHandleLabel,
  messageForAssetCode,
  type AssetPage as AssetPageData,
} from "../../../lib/assets";
import "./assets.css";

/**
 * One research asset's page (T0709): the eleven blocks docs/42 §Asset Page
 * fixes, in that order — PID/version, type, origin, rights, creators,
 * metadata, dependencies, lineage, used/derived public links, versions,
 * network events — rendered straight from the API's answer.
 *
 * # Everything here is the server's answer
 *
 * The page renders what `GET /api/v1/assets/{pid}` returned and adds no
 * rule of its own. Which version is shown, whether a project is named,
 * which usages and events are listed, whether the viewer sees private
 * versions at all — all of it is decided by the API
 * (internal/assets.BuildPage), which is where the rules have tests. This
 * component has none of those rules, and that is on purpose: a front end
 * that hid a row the API sent would be the client-side hiding docs/23 §3
 * forbids, in the one layer with no test for it.
 *
 * A block is empty when the API sent `[]`. The page then says so in words
 * ("No public usage recorded") rather than by leaving the block out: an
 * absent block and an empty one look identical to a reader, and only one of
 * them is true. The page never says a list was shortened — that sentence is
 * the count of hidden things the API does not send.
 *
 * # Blobs
 *
 * No download link, for any blob, in any state. The platform has no blob
 * download route (cmd/api/fileshttp registers file reads, not blob
 * fetches), so a link here would answer 404; and docs/17 §5 wants every
 * fetch to pass the project/object/blob policy at fetch time, which a page
 * cannot do. The manifest's blob identifiers appear in the metadata block,
 * where the publisher declared them, and nowhere else.
 *
 * # 404
 *
 * One not-found state, for the API's one ASSET_NOT_FOUND code: no such pid,
 * a project the caller may not read, nothing visible, a version they may
 * not see. The page does not try to tell those apart — it cannot, and
 * trying would be the oracle that code exists not to be.
 */
/**
 * The answer on hand, tagged with the address it answers.
 *
 * The tag is what keeps a stale answer off the screen when the reader
 * follows a link from one version to another without a remount: the render
 * compares the tag to the address it was asked for and shows the spinner
 * otherwise, so nothing has to be reset from inside an effect (a
 * synchronous setState there is a cascading render, and a reset racing the
 * fetch is a spinner that lies).
 */
type PageAnswer =
  | { forKey: string; ok: true; page: AssetPageData }
  | { forKey: string; ok: false; message: string; status: number };

export function AssetPage({
  apiBaseUrl,
  pid,
  version,
}: {
  apiBaseUrl: string;
  pid: string;
  version?: string | null;
}) {
  const client = useMemo(() => createAssetsClient(apiBaseUrl), [apiBaseUrl]);
  const [answer, setAnswer] = useState<PageAnswer | null>(null);

  // The address as one comparable string. JSON rather than a separator
  // character: any separator could also occur inside a pid or a label, and
  // a key that two different addresses could share is worse than none.
  const key = JSON.stringify([pid, version ?? ""]);

  useEffect(() => {
    let cancelled = false;
    // setState only in the callbacks: the effect's body talks to the
    // network and to nothing else.
    client
      .page(pid, version ?? null)
      .then((page) => {
        if (!cancelled) setAnswer({ forKey: key, ok: true, page });
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        if (err instanceof ApiError) {
          setAnswer({ forKey: key, ok: false, message: messageForAssetCode(err.code), status: err.status });
        } else {
          setAnswer({ forKey: key, ok: false, message: messageForAssetCode("UNKNOWN"), status: 0 });
        }
      });
    return () => {
      cancelled = true;
    };
  }, [client, pid, version, key]);

  const current = answer !== null && answer.forKey === key ? answer : null;

  if (current === null) {
    return (
      <div className="asset-page" data-asset-page="loading">
        <Spinner aria-label="Loading asset" />
      </div>
    );
  }
  if (!current.ok) {
    return (
      <div className="asset-page" data-asset-page="error" data-asset-status={current.status}>
        <h1 className="assets-title">Asset</h1>
        <p className="asset-notice">{current.message}</p>
        <p className="asset-notice-sub">
          <Link href="/assets">Back to all assets</Link>
        </p>
      </div>
    );
  }

  const page = current.page;
  const { asset, version: rendered } = page;
  return (
    <div className="asset-page" data-asset-page="ready" data-asset-pid={asset.pid}>
      <nav className="asset-breadcrumb" aria-label="Breadcrumb">
        <Link href="/assets">Assets</Link>
      </nav>

      {/* 1. PID / version, 2. type — data-asset-block names the docs/42
          items this element carries, so a test can check the eleven
          one by one instead of trusting that the page still has them. */}
      <header className="asset-header" data-asset-block="asset version">
        <span className="asset-header-icon" aria-hidden="true">
          <PackageIcon size={24} />
        </span>
        <div className="asset-header-main">
          <h1 className="asset-title" data-asset-title={asset.slug}>
            {asset.title}
          </h1>
          <p className="asset-ident">
            <span className="assets-type" data-asset-type={asset.type}>
              {assetTypeLabel(asset.type)}
            </span>
            <code className="assets-pid" data-asset-pid-code>
              {asset.pid}
            </code>
            <span className="asset-version-chip" data-asset-version={rendered.version}>
              version {rendered.version}
            </span>
            <span className="asset-visibility" data-asset-visibility={rendered.visibility}>
              {rendered.visibility}
            </span>
          </p>
          <p className="asset-published">
            Published {rendered.published_at.slice(0, 10)}
            {rendered.published_by === null ? null : (
              <>
                {" by "}
                <span className="asset-actor">{rendered.published_by.display_name}</span>
                <span className="asset-handle">@{rendered.published_by.handle}</span>
              </>
            )}
            {asset.origin_project === null ? null : (
              <>
                {" in "}
                <Link href={`/projects/${asset.origin_project.id}`} data-asset-project={asset.origin_project.slug}>
                  {asset.origin_project.name}
                </Link>
              </>
            )}
          </p>
          {/* The published bytes' hash: a reader can check that what it was
              served is what was published (docs/11 §1). */}
          <p className="asset-hash">
            <ShieldCheckIcon size={14} aria-hidden="true" />{" "}
            <code data-asset-hash>{rendered.integrity_hash}</code>
          </p>
        </div>
      </header>

      <div className="asset-columns">
        <div className="asset-column-main">
          {/* 3. origin */}
          <section
            className="asset-block"
            aria-labelledby="asset-origin-heading"
            data-asset-block="origin"
          >
            <h2 className="asset-block-title" id="asset-origin-heading">
              <LinkIcon size={16} aria-hidden="true" /> Origin
            </h2>
            {page.origin.length === 0 ? (
              <p className="asset-block-empty">No resolvable origin recorded for this version.</p>
            ) : (
              <ul className="asset-rows">
                {page.origin.map((origin) => (
                  <li className="asset-row" key={origin.ref}>
                    <span className="asset-row-kind">{origin.kind}</span>
                    {origin.link === undefined ? (
                      <code className="asset-row-ref">{origin.ref}</code>
                    ) : (
                      <Link href={origin.link} className="asset-row-ref">
                        {origin.ref}
                      </Link>
                    )}
                    {origin.title === undefined ? null : (
                      <span className="asset-row-note">{origin.title}</span>
                    )}
                  </li>
                ))}
              </ul>
            )}
          </section>

          {/* 4. rights — the shared panel (T0703), not a copy of it. The
              wrapper carries the block name for the page's own tests; the
              panel itself is untouched. */}
          <div data-asset-block="rights">
            <RightsPanel rights={page.rights} />
          </div>

          {/* 5. creators */}
          <section
            className="asset-block"
            aria-labelledby="asset-creators-heading"
            data-asset-block="creators"
          >
            <h2 className="asset-block-title" id="asset-creators-heading">
              <BeakerIcon size={16} aria-hidden="true" /> Creators
            </h2>
            {page.creators.length === 0 ? (
              /* No stored credit row for this version — the honest state of
                 a version published before the credit table existed, said
                 in words rather than filled in with the publisher. */
              <p className="asset-block-empty">No credited party recorded for this version.</p>
            ) : (
              <ul className="asset-rows">
                {page.creators.map((creator) => (
                  <li className="asset-row" key={`${creator.kind}-${creator.party_id}-${creator.role}`}>
                    {/* A user has a profile page and links to it; an
                        organization has no /users page (it is not a user)
                        and is named without a link rather than linked to
                        somebody else's. */}
                    {creator.kind === "user" ? (
                      <Link href={`/users/${creator.party_id}`} className="asset-row-name">
                        {creator.display_name}
                      </Link>
                    ) : (
                      <span className="asset-row-name" data-asset-party-kind={creator.kind}>
                        {creator.display_name}
                      </span>
                    )}
                    {/* The handle is a mention only when the party is a
                        user; an organization's is a slug, rendered bare
                        (creatorHandleLabel). */}
                    <span className="asset-handle">{creatorHandleLabel(creator)}</span>
                    {/* The role and the kind are rendered, not implied: the
                        credit is the relationship the version declares
                        (creator, contributor, …), and the party is a user
                        or an organization. */}
                    <span className="asset-role" data-asset-role={creator.role}>
                      {creator.role}
                    </span>
                  </li>
                ))}
              </ul>
            )}
          </section>

          {/* 6. metadata */}
          <section
            className="asset-block"
            aria-labelledby="asset-metadata-heading"
            data-asset-block="metadata"
          >
            <h2 className="asset-block-title" id="asset-metadata-heading">
              <TagIcon size={16} aria-hidden="true" /> Metadata
            </h2>
            {page.metadata.length === 0 ? (
              <p className="asset-block-empty">This version declares no metadata.</p>
            ) : (
              <dl className="asset-metadata">
                {page.metadata.map((entry) => (
                  <div className="asset-metadata-row" key={entry.key} data-asset-metadata={entry.key}>
                    <dt>{entry.key}</dt>
                    <dd>
                      <code>{renderMetadataValue(entry.value)}</code>
                    </dd>
                  </div>
                ))}
              </dl>
            )}
          </section>

          {/* 7. dependencies */}
          <section
            className="asset-block"
            aria-labelledby="asset-dependencies-heading"
            data-asset-block="dependencies"
          >
            <h2 className="asset-block-title" id="asset-dependencies-heading">
              <GitBranchIcon size={16} aria-hidden="true" /> Dependencies
            </h2>
            {page.dependencies.length === 0 ? (
              <p className="asset-block-empty">This version pins no published dependency.</p>
            ) : (
              <ul className="asset-rows" data-asset-dependencies={page.dependencies.length}>
                {page.dependencies.map((dep) => (
                  <li className="asset-row" key={dep.pin}>
                    {dep.url === undefined ? (
                      <code className="asset-row-ref">{dep.pin}</code>
                    ) : (
                      <Link href={dep.url} className="asset-row-name">
                        {dep.title ?? dep.pin}
                      </Link>
                    )}
                    <code className="asset-row-ref">{dep.pin}</code>
                    {dep.resolved ? null : (
                      // The pin names a version this repository has no row
                      // for: a fact about the publisher's document, said as
                      // such.
                      <span className="asset-row-note">not in this repository</span>
                    )}
                  </li>
                ))}
              </ul>
            )}
          </section>

          {/* 8. lineage */}
          <section
            className="asset-block"
            aria-labelledby="asset-lineage-heading"
            data-asset-block="lineage"
          >
            <h2 className="asset-block-title" id="asset-lineage-heading">
              <VersionsIcon size={16} aria-hidden="true" /> Lineage
            </h2>
            {page.lineage.length === 0 ? (
              <p className="asset-block-empty">No fork or derive edge recorded for this version.</p>
            ) : (
              <ul className="asset-rows">
                {page.lineage.map((edge) => (
                  <li
                    className="asset-row"
                    key={`${edge.relation}-${edge.direction}-${edge.pid}-${edge.version}`}
                  >
                    <span className="asset-row-kind" data-asset-relation={edge.relation}>
                      {edge.relation}
                    </span>
                    <span className="asset-row-arrow" aria-hidden="true">
                      {edge.direction === "parent" ? "←" : "→"}
                    </span>
                    <Link href={edge.url} className="asset-row-name">
                      {edge.title}
                    </Link>
                    <code className="asset-row-ref">
                      {edge.pid}@{edge.version}
                    </code>
                  </li>
                ))}
              </ul>
            )}
          </section>

          {/* 9. used/derived public links */}
          <section
            className="asset-block"
            aria-labelledby="asset-usages-heading"
            data-asset-block="used_by"
          >
            <h2 className="asset-block-title" id="asset-usages-heading">
              <LinkIcon size={16} aria-hidden="true" /> Used by
            </h2>
            {page.used_by.length === 0 ? (
              <p className="asset-block-empty">No public usage recorded for this version.</p>
            ) : (
              <ul className="asset-rows" data-asset-usages={page.used_by.length}>
                {page.used_by.map((usage) => (
                  <li
                    className="asset-row"
                    key={`${usage.project_id}-${usage.dependency_type}`}
                    data-asset-usage-project={usage.project_slug}
                  >
                    <Link href={`/projects/${usage.project_id}`} className="asset-row-name">
                      {usage.project_name}
                    </Link>
                    <span className="asset-row-kind">{usage.dependency_type}</span>
                    <span className="assets-date">{usage.created_at.slice(0, 10)}</span>
                  </li>
                ))}
              </ul>
            )}
          </section>
        </div>

        <aside className="asset-column-side">
          {/* 10. versions */}
          <section
            className="asset-block"
            aria-labelledby="asset-versions-heading"
            data-asset-block="versions"
          >
            <h2 className="asset-block-title" id="asset-versions-heading">
              <VersionsIcon size={16} aria-hidden="true" /> Versions
            </h2>
            <ul className="asset-versions" data-asset-versions={page.versions.length}>
              {page.versions.map((entry) => (
                <li
                  className="asset-version-row"
                  key={entry.version}
                  data-asset-version-row={entry.version}
                  data-asset-version-visibility={entry.visibility}
                  aria-current={entry.current ? "true" : undefined}
                >
                  <Link href={entry.url} className="asset-version-label">
                    {entry.version}
                  </Link>
                  {/* Two versions of one asset are told apart by their
                      labels AND their hashes: the label is the name, the
                      hash is the content. */}
                  <code className="asset-version-hash">{shortHash(entry.integrity_hash)}</code>
                  <span className="asset-visibility">{entry.visibility}</span>
                  <span className="assets-date">{entry.published_at.slice(0, 10)}</span>
                </li>
              ))}
            </ul>
          </section>

          {/* 11. network events */}
          <section
            className="asset-block"
            aria-labelledby="asset-events-heading"
            data-asset-block="events"
          >
            <h2 className="asset-block-title" id="asset-events-heading">
              <LawIcon size={16} aria-hidden="true" /> Network events
            </h2>
            {page.events.length === 0 ? (
              <p className="asset-block-empty">No public event recorded for this asset.</p>
            ) : (
              <ul className="asset-events">
                {page.events.map((event) => (
                  <li
                    className="asset-event"
                    key={`${event.type}-${event.occurred_at}-${event.version ?? ""}`}
                    data-asset-event={event.type}
                  >
                    <span className="asset-event-type">{event.type}</span>
                    {event.version === undefined ? null : (
                      <span className="asset-event-version">version {event.version}</span>
                    )}
                    <span className="assets-date">{event.occurred_at.slice(0, 10)}</span>
                    {event.actor === null ? null : (
                      <span className="asset-handle">@{event.actor.handle}</span>
                    )}
                  </li>
                ))}
              </ul>
            )}
          </section>
        </aside>
      </div>
    </div>
  );
}

/**
 * One metadata value, rendered as text.
 *
 * A value may be any JSON (the manifest's metadata is an open map), so the
 * rendering is: a string as itself, a number or boolean as its JSON form,
 * and an array or object as its compact JSON. Never "[object Object]" —
 * which is what React would print for a bare object — and never a sentence
 * about the value being unreadable, because the value IS readable and it is
 * the publisher's.
 */
function renderMetadataValue(value: unknown): string {
  if (typeof value === "string") return value;
  if (value === null || value === undefined) return "null";
  if (typeof value === "number" || typeof value === "boolean") return String(value);
  try {
    return JSON.stringify(value);
  } catch {
    return String(value);
  }
}

/** The first twelve characters of a hash, for the dense versions list. */
function shortHash(hash: string): string {
  const body = hash.startsWith("sha256:") ? hash.slice("sha256:".length) : hash;
  return `${body.slice(0, 12)}…`;
}
