import type { MetadataRoute } from "next";

import { getWebConfig } from "../lib/server-config";
import { webOrigin } from "../lib/origin";
import { fetchPublicIndex, publicIndexEntries } from "../lib/entity-meta";

/**
 * /sitemap.xml (T0801): every public entity page, and only those.
 *
 * The list is read ANONYMOUSLY from the API's own public reads — the
 * project list an anonymous caller gets is the public projects only
 * (T0106), and the asset hub's browse list is the publicly published asset
 * versions (T0709). There is no separate "what is public" decision here:
 * whatever the API answers an anonymous caller with IS the sitemap, so a
 * private project cannot enter it, and a project that turns private leaves
 * it on the next read.
 *
 * Release, profile and knowledge pages are deliberately absent for V1:
 *
 *   - a release page is only reachable through its project, and there is
 *     no public "releases" index to enumerate (listing every release of
 *     every project would be one API read per project, on a route a
 *     crawler can hit at will);
 *   - profiles have no public index yet — the People directory is a stub
 *     until T0808, and the id-keyed profile pages it will list are
 *     already crawlable (`allow: /` in app/robots.ts);
 *   - there is no knowledge route at all yet (T0805).
 *
 * The pages that are absent are absent from the LIST, not from the crawl:
 * a crawler that reaches one through a link still reads a correct,
 * indexable <head> from the route's own metadata.
 */
export default async function sitemap(): Promise<MetadataRoute.Sitemap> {
  const cfg = getWebConfig();
  const [origin, index] = await Promise.all([webOrigin(), fetchPublicIndex(cfg.apiBaseUrl)]);
  return publicIndexEntries(origin, index);
}
