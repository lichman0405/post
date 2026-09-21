import type { NextConfig } from "next";

/**
 * The Content-Security-Policy the web app serves.
 *
 * Why it is here and not only in the API (T1106): the CSP is a statement
 * the browser reads from the document it is about to render, so it has to
 * be on the document's own response — apps/web's, not cmd/api's. The API
 * sets its own per-response policy (internal/security/headers.go) because
 * it serves JSON envelopes and rendered documents from one mux; this one
 * covers the Next app, where every response is a document.
 *
 * # What each directive buys
 *
 *   default-src 'self'      nothing loads from another origin unless a more
 *                           specific directive says so. This is the
 *                           directive that makes the rest meaningful: an
 *                           unlisted fetch type fails closed.
 *   script-src 'self' 'unsafe-inline'
 *                           'unsafe-inline' is REQUIRED and it is not an
 *                           oversight: Next injects its own bootstrap and
 *                           RSC payload as inline <script> tags in the
 *                           document, and a nonce would mean threading a
 *                           per-request value through the App Router's
 *                           rendering (middleware plus a header the
 *                           framework reads). Without it the app does not
 *                           boot. What is bought by keeping the rest tight:
 *                           no remote script origin is reachable and
 *                           'unsafe-eval' is absent, so the injection
 *                           vectors are limited to markup we already serve.
 *   style-src 'self' 'unsafe-inline'
 *                           Inline styles are how Primer-style components
 *                           ship. A style injection cannot execute.
 *   img-src 'self' data: blob:
 *                           data:/blob: are how the app renders a chart
 *                           canvas without a round trip.
 *   font-src 'self'         the app self-hosts its fonts.
 *   connect-src 'self' +    the API origin, computed — see below.
 *   object-src 'none'       no <embed>/<object>: the plugin surface is gone
 *                           from every browser and is pure attack surface.
 *   base-uri 'none'         an injected <base> cannot re-point every
 *                           relative URL on the page.
 *   form-action 'self'      a planted form cannot POST a visitor's data to
 *                           another origin. Every real form in the app
 *                           submits through fetch (onSubmit +
 *                           preventDefault) or is same-origin, so this
 *                           costs nothing.
 *   frame-ancestors 'none'  the app may not be framed — the clickjacking
 *                           defense, and the one directive that is NOT
 *                           inherited from default-src.
 *   frame-src 'none'        and it may not frame anything either.
 *
 * # connect-src and the cross-origin API
 *
 * The browser DOES call the API origin directly, so 'self' alone would
 * break the product. Client components are handed the server-resolved
 * API_BASE_URL as a prop and fetch it from the browser — today that is the
 * whole authenticated surface (lib/auth.ts:142 fetch(apiBaseUrl + path,
 * {credentials: "include"}), lib/projects.ts, lib/files.ts, lib/releases.ts,
 * lib/activity.ts, lib/conflicts.ts, lib/profile.ts, lib/discussions.ts,
 * lib/assets.ts), plus the raw file download link
 * (app/(main)/projects/[id]/files/page.tsx:238). The app origin and the API
 * origin differ in the default topology (3000 and 8080); the API's own CORS
 * allow-list — bound to POST_WEB_ORIGIN, never a wildcard — is what admits
 * the pair.
 *
 * So connect-src names that origin explicitly: 'self' plus the origin of
 * API_BASE_URL, when the variable is set. It is read here rather than
 * hard-coded because a hard-coded default would have to be wrong in every
 * deployment but the developer's. This is not a weakening: it is the same
 * single origin the API's CORS already trusts, named in one more place
 * instead of leaving the browser to discover the mismatch as a console
 * error on the first fetch.
 *
 * API_BASE_URL has no default anywhere in this app — lib/config.ts refuses
 * to start without it — so a running server always has it set. A build and
 * a `next start` that disagree about its value are a deployment error the
 * smoke target and CI avoid by passing the same environment to both.
 *
 * # Deliberately absent
 *
 * Cross-Origin-Embedder-Policy and Cross-Origin-Resource-Policy are NOT set.
 * COEP: require-corp would break every cross-origin subresource without a
 * CORP header of its own — including the API's responses, which the browser
 * fetches — and the isolation it buys (SharedArrayBuffer, high-resolution
 * timers) is not something this app uses. Setting a header that breaks the
 * product to satisfy a checklist is the failure this comment exists to
 * prevent.
 *
 * HSTS is likewise the API's edge's to send (internal/security); a browser
 * that receives it over http ignores it, and the dev topology is http.
 */

/**
 * The origin of the API the browser talks to: scheme://host[:port], path
 * stripped, or null when the variable is unset or unparsable.
 *
 * Returning null (rather than guessing a host) keeps the header honest: the
 * loader that requires API_BASE_URL fails the process before it can serve a
 * page, so the fallback is only ever reached by a config import outside a
 * running server — a unit test, or `next build` without an environment.
 */
function apiOrigin(): string | null {
  const raw = process.env.API_BASE_URL;
  if (raw === undefined || raw === "") {
    return null;
  }
  try {
    const url = new URL(raw);
    if (url.protocol !== "http:" && url.protocol !== "https:") {
      return null;
    }
    return url.origin;
  } catch {
    return null;
  }
}

/**
 * The full header set every document the web app serves carries. Value for
 * value the same decisions as the API's edge where the two overlap, so a
 * reader comparing them finds one policy rather than two that almost agree.
 */
function securityHeaders(): Array<{ key: string; value: string }> {
  const api = apiOrigin();
  const contentSecurityPolicy = [
    "default-src 'self'",
    "script-src 'self' 'unsafe-inline'",
    "style-src 'self' 'unsafe-inline'",
    "img-src 'self' data: blob:",
    "font-src 'self'",
    `connect-src 'self'${api === null ? "" : ` ${api}`}`,
    "object-src 'none'",
    "base-uri 'none'",
    "form-action 'self'",
    "frame-ancestors 'none'",
    "frame-src 'none'",
  ].join("; ");

  return [
    { key: "Content-Security-Policy", value: contentSecurityPolicy },
    // The app must not be sniffed into anything but what Next declared.
    { key: "X-Content-Type-Options", value: "nosniff" },
    { key: "X-Frame-Options", value: "DENY" },
    { key: "Referrer-Policy", value: "no-referrer" },
    {
      key: "Permissions-Policy",
      value: "accelerometer=(), autoplay=(), camera=(), display-capture=(), encrypted-media=(), fullscreen=(), geolocation=(), gyroscope=(), magnetometer=(), microphone=(), midi=(), payment=(), picture-in-picture=(), publickey-credentials-get=(), screen-wake-lock=(), usb=(), xr-spatial-tracking=()",
    },
  ];
}

const nextConfig: NextConfig = {
  // @post/ui ships TypeScript source (docs/65: shared web/ui components live
  // in the pnpm workspace, compiled by Next).
  transpilePackages: ["@post/ui"],

  // The framework's own advertising header, off — this is how the app stops
  // announcing its version. Next 16 no longer sends X-Powered-By by default;
  // saying so explicitly means a version bump that turns it back on is a
  // diff somebody sees rather than a header nobody does. (The equivalent
  // control on the API side is the edge's Server token, which carries no
  // version — see internal/security/headers.go.)
  poweredByHeader: false,

  // Security headers on every path. Next applies `headers()` to the whole
  // app (static assets included), so there is no route that can be reached
  // without them — the same "structural, not per-route opt-in" property the
  // API's guard has.
  //
  // The value is computed per call rather than frozen at module load, so a
  // server whose environment changes between the config import and the
  // header being written still names the origin it actually talks to.
  async headers() {
    return [{ source: "/:path*", headers: securityHeaders() }];
  },
};

export default nextConfig;
