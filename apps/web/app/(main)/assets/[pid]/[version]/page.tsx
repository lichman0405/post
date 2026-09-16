import type { Metadata } from "next";
import { Suspense } from "react";

import { getWebConfig } from "../../../../../lib/server-config";
import { AssetPage } from "../../asset-page";

export const metadata: Metadata = {
  title: "Asset version — POST",
};

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
 */
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
