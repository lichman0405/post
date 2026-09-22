import type { Metadata } from "next";

import { getWebConfig } from "../../../lib/server-config";
import { getT } from "../../../lib/i18n-server";
import { AssetsBrowse } from "./assets-browse";

/** T1105: the tab title is copy — it follows the language preference. The
 *  browse list below it is NOT localized (see the coverage table in the
 *  task's RESULT); this route is therefore half-covered on purpose and the
 *  catalog group it draws from says so. */
export async function generateMetadata(): Promise<Metadata> {
  const { t } = await getT();
  return { title: t("page.assets.metaTitle") };
}

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
