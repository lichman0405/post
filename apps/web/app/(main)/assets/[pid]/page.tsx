import type { Metadata } from "next";
import { Suspense } from "react";

import { getWebConfig } from "../../../../lib/server-config";
import { webOrigin } from "../../../../lib/origin";
import {
  fetchPublicAssetMeta,
  hiddenPageMetadata,
  publicEntityPath,
  publicPageMetadata,
} from "../../../../lib/entity-meta";
import { AssetPage } from "../asset-page";

/**
 * One asset, newest version the caller may see (T0709): server component
 * that resolves the validated API origin and hands it — plus the pid from
 * the URL — to the client-side page.
 *
 * The URL is pid-keyed by design (docs/11 §2): the pid is the asset's
 * persistent identity, so a citation keeps resolving when the title is
 * revised. The version lives in its own segment ([version]/page.tsx), which
 * is the shape internal/assets/url.go fixes for both the API and this app.
 *
 * The page's own 404 is the API's 404, rendered by the client component:
 * this component cannot decide whether an asset exists without asking, and
 * asking twice would be a second place for the visibility rule to live.
 * T0801's metadata read (below) does not change that: it answers for the
 * <head> a crawler reads — anonymously, so a pid the API does not publish
 * gets the unindexable head rather than a name — and the body keeps coming
 * from the client component's own ask, with whatever session the reader has.
 */
export async function generateMetadata({
  params,
}: {
  params: Promise<{ pid: string }>;
}): Promise<Metadata> {
  const cfg = getWebConfig();
  const { pid } = await params;
  const entity = await fetchPublicAssetMeta(cfg.apiBaseUrl, pid);
  if (entity === null) return hiddenPageMetadata();
  const origin = await webOrigin();
  return publicPageMetadata(entity, `${origin}${publicEntityPath("asset", pid)}`);
}

export default async function AssetPageRoute({
  params,
}: {
  params: Promise<{ pid: string }>;
}) {
  const cfg = getWebConfig();
  const { pid } = await params;
  return (
    <Suspense fallback={null}>
      <AssetPage apiBaseUrl={cfg.apiBaseUrl} pid={pid} />
    </Suspense>
  );
}
