"use client";

import { useEffect, useMemo, useState } from "react";
import Link from "next/link";
import { PackageIcon } from "@primer/octicons-react";
import { Spinner } from "@primer/react";

import {
  ApiError,
  ASSET_TYPES,
  assetTypeLabel,
  createAssetsClient,
  messageForAssetCode,
  type AssetBrowseList,
  type AssetType,
} from "../../../lib/assets";
import "./assets.css";

/**
 * The asset hub's browse list (T0709): every asset with something public
 * behind it, newest publication first, filterable by the four V1 types.
 *
 * Three things this component deliberately does NOT do.
 *
 * It does not filter the response. The API's list is already the public
 * index (internal/assets.BuildBrowse), and a client that dropped rows would
 * be a second implementation of the disclosure rule in the layer nobody
 * tests — the shape of front-end hiding docs/23 §3 forbids.
 *
 * It does not ask per row. Whether an asset's project is named is the
 * server's answer, rendered as it arrives: either a project (public) or
 * null (withheld). The page shows the row without a project and does not
 * say "hidden project", because that sentence is the count of withheld
 * things the same rule exists to keep off the wire.
 *
 * It does not render a second number. `public_versions` is the count of
 * versions this list can open; there is no total to subtract it from, and
 * none is invented here.
 */
/**
 * The answer on hand, tagged with the filter it answers.
 *
 * The tag is what lets the render decide "is this answer about what the
 * reader is looking at?" without a second piece of state that an effect
 * would have to reset — state an effect sets synchronously is a cascading
 * render, and a reset that races the fetch is a spinner that lies. An
 * answer whose tag is not the current filter is simply not shown: it is
 * stale, and the spinner says so.
 */
type BrowseAnswer =
  | { for: AssetType | null; ok: true; list: AssetBrowseList }
  | { for: AssetType | null; ok: false; message: string };

export function AssetsBrowse({ apiBaseUrl }: { apiBaseUrl: string }) {
  const client = useMemo(() => createAssetsClient(apiBaseUrl), [apiBaseUrl]);
  const [type, setType] = useState<AssetType | null>(null);
  const [answer, setAnswer] = useState<BrowseAnswer | null>(null);

  useEffect(() => {
    let cancelled = false;
    // setState only in the callbacks: the effect's body talks to the
    // network and to nothing else.
    client
      .browse(type)
      .then((list) => {
        if (!cancelled) setAnswer({ for: type, ok: true, list });
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        setAnswer({
          for: type,
          ok: false,
          message: messageForAssetCode(err instanceof ApiError ? err.code : "UNKNOWN"),
        });
      });
    return () => {
      cancelled = true;
    };
  }, [client, type]);

  // The answer is about the current filter, or it is not shown at all.
  const current = answer !== null && answer.for === type ? answer : null;

  // The filter control is built from the set the SERVER offers once an
  // answer has arrived, and from the module's copy of the closed V1 set
  // before that — the same four values either way, and never a value the
  // API would refuse.
  const types =
    current !== null && current.ok && current.list.types.length > 0
      ? current.list.types
      : [...ASSET_TYPES];

  return (
    <div className="assets-browse">
      <h1 className="assets-title">Assets</h1>
      <p className="assets-sub">
        Published research assets — datasets, protocols, material collections
        and benchmarks — with their immutable versions, rights and lineage.
      </p>

      <nav className="assets-filter" aria-label="Filter assets by type">
        <button
          type="button"
          className="assets-filter-button"
          aria-pressed={type === null}
          data-asset-filter="all"
          onClick={() => setType(null)}
        >
          All types
        </button>
        {types.map((candidate) => (
          <button
            key={candidate}
            type="button"
            className="assets-filter-button"
            aria-pressed={type === candidate}
            data-asset-filter={candidate}
            onClick={() => setType(candidate)}
          >
            {assetTypeLabel(candidate)}
          </button>
        ))}
      </nav>

      {current === null ? (
        <Spinner aria-label="Loading assets" />
      ) : !current.ok ? (
        <div className="assets-empty">{current.message}</div>
      ) : current.list.assets.length === 0 ? (
        <div className="assets-empty">
          {type === null
            ? "No published assets yet."
            : `No published ${assetTypeLabel(type).toLowerCase()} assets yet.`}
        </div>
      ) : (
        <ul className="assets-list" data-asset-count={current.list.assets.length}>
          {current.list.assets.map((asset) => (
            <li className="assets-row" key={asset.pid} data-asset-pid={asset.pid}>
              <span className="assets-row-icon" aria-hidden="true">
                <PackageIcon size={16} />
              </span>
              <span className="assets-row-main">
                <Link href={asset.url} className="assets-row-title" data-asset-title={asset.slug}>
                  {asset.title}
                </Link>
                <span className="assets-row-facts">
                  <span className="assets-type" data-asset-type={asset.type}>
                    {assetTypeLabel(asset.type)}
                  </span>
                  {/* The pid is the asset's persistent identity (docs/11 §2):
                      a reader can cite it even when the title is revised. */}
                  <code className="assets-pid">{asset.pid}</code>
                  {asset.origin_project === null ? null : (
                    <Link
                      href={`/projects/${asset.origin_project.id}`}
                      className="assets-project"
                      data-asset-project={asset.origin_project.slug}
                    >
                      {asset.origin_project.name}
                    </Link>
                  )}
                  <Link href={asset.latest_url} className="assets-version-link">
                    latest {asset.latest_version}
                  </Link>
                  <span className="assets-counts">
                    {asset.public_versions} public version
                    {asset.public_versions === 1 ? "" : "s"}
                  </span>
                  <span className="assets-date">
                    {asset.latest_published_at.slice(0, 10)}
                  </span>
                </span>
              </span>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
