import type { MetadataRoute } from "next";

import { webOrigin } from "../lib/origin";

/**
 * /robots.txt (T0801).
 *
 * What this file is for, and what it is NOT for:
 *
 *   - It is the sitemap advertisement and the "stay out of the
 *     personal/governance surfaces" list (docs/05: Notifications is a
 *     member inbox, project Settings is governance). Nothing here protects
 *     a private entity — a crawler that ignores robots.txt must still see
 *     nothing, and it does: those pages answer the anonymous caller with
 *     the existence-hiding 404 state and carry `noindex, nofollow`
 *     (lib/entity-meta.ts). Robots.txt is a request; the API's answer is
 *     the control.
 *   - It deliberately does NOT try to enumerate private URLs. It cannot:
 *     a robots.txt Disallow would both publish the ids it names and fail
 *     to cover the ids it does not, and Disallow alone would stop the
 *     crawler from ever reading the `noindex` meta that actually decides
 *     the question.
 *
 * The origin comes from the request (see lib/origin.ts): robots.txt has no
 * other way to know the host it is being served for, and absolute URLs are
 * required in the Sitemap line.
 */
export default async function robots(): Promise<MetadataRoute.Robots> {
  const origin = await webOrigin();
  return {
    rules: [
      {
        userAgent: "*",
        // Everything public is crawlable (docs/05 §1: browsing public
        // entities before login is a product promise, not a leak).
        allow: "/",
        disallow: [
          // The member inbox: nothing here for an anonymous crawler, and
          // the page is session-shaped.
          "/notifications",
          // Project governance; the page itself is permission-gated and
          // its URL names a project the crawler may not read.
          "/projects/*/settings",
        ],
      },
    ],
    sitemap: `${origin}/sitemap.xml`,
  };
}
