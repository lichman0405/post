import type { Metadata } from "next";
import { Suspense } from "react";

import { getWebConfig } from "../../../../../lib/server-config";
import { webOrigin } from "../../../../../lib/origin";
import {
  fetchPublicAssetMeta,
  hiddenPageMetadata,
  publicEntityPath,
  publicPageMetadata,
} from "../../../../../lib/entity-meta";
import { AssetPage } from "../../asset-page";

/**
 * One named version of one asset (T0709) — the persistent address
 * internal/assets/url.go fixes as /assets/{pid}/{version}, over the
 * contract's `GET /assets/{assetId}?version=` query.
 *
 * A version label the caller may not see is the API's ordinary
 * ASSET_NOT_FOUND, and the page renders the same single not-found state it
 * renders for an unknown pid. That is the point of the version being a
 * query rather than a path on the API side: a caller cannot tell "no such
 * version" from "not shared with you" by probing either address.
 *
 * T0801: the <head> is built from the same anonymous read the API answers
 * a crawler with, for the VERSION in the URL — a hidden version label
 * yields the unindexable head, exactly like the pid-only route.
 */
export async function generateMetadata({
  params,
}: {
  params: Promise<{ pid: string; version: string }>;
}): Promise<Metadata> {
  const cfg = getWebConfig();
  const { pid, version } = await params;
  const entity = await fetchPublicAssetMeta(cfg.apiBaseUrl, pid, version);
  if (entity === null) return hiddenPageMetadata();
  const origin = await webOrigin();
  return publicPageMetadata(entity, `${origin}${publicEntityPath("asset", pid, undefined, version)}`);
}

export default async function AssetVersionPageRoute({
  params,
}: {
  params: Promise<{ pid: string; version: string }>;
}) {
  const cfg = getWebConfig();
  const { pid, version } = await params;
  return (
    <Suspense fallback={null}>
      <AssetPage apiBaseUrl={cfg.apiBaseUrl} pid={pid} version={version} />
    </Suspense>
  );
}
