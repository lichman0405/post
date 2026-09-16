import type { Metadata } from "next";
import { Suspense } from "react";

import { getWebConfig } from "../../../../lib/server-config";
import { AssetPage } from "../asset-page";

export const metadata: Metadata = {
  title: "Asset — POST",
};

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
 */
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
