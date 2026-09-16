/**
 * The request's public web origin (T0801) — the absolute base every
 * canonical link, OpenGraph url and sitemap entry is built from.
 *
 * The decision itself is resolvePublicOrigin (lib/entity-meta.ts), which
 * takes the values rather than reading them, so it is unit-tested without
 * a request. This module is the thin server-only half: it reads the
 * configured origin and the forwarded/host headers of the CURRENT request.
 *
 * Server-only by construction: next/headers is not importable from a
 * client component, which is exactly the boundary this belongs behind —
 * the values it reads exist only during a server render.
 */

import { headers } from "next/headers";

import { resolvePublicOrigin } from "./entity-meta";

/**
 * The deployment's public origin for this request. Never throws: a request
 * with no usable Host yields "", and callers then emit relative paths
 * rather than a broken absolute one.
 */
export async function webOrigin(): Promise<string> {
  const h = await headers();
  return resolvePublicOrigin(process.env.POST_WEB_ORIGIN ?? "", {
    host: h.get("host"),
    forwardedHost: h.get("x-forwarded-host"),
    forwardedProto: h.get("x-forwarded-proto"),
  });
}
