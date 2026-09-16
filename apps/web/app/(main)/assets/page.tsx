import type { Metadata } from "next";

import { getWebConfig } from "../../../lib/server-config";
import { AssetsBrowse } from "./assets-browse";

export const metadata: Metadata = {
  title: "Assets — POST",
};

/**
 * The asset hub (T0709, docs/11 §2): server component that resolves the
 * validated API origin and hands it to the client-side browse list. The
 * list itself reads over the wire — the web app has no backend of its own,
 * and no copy of the visibility rules.
 *
 * /explore — the aggregated discovery surface — is NOT this page; it is
 * T0802's, and its destination stays stubbed there
 * (app/(main)/explore/page.tsx).
 */
export default function AssetsPage() {
  const cfg = getWebConfig();
  return <AssetsBrowse apiBaseUrl={cfg.apiBaseUrl} />;
}
