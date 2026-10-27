import { headers } from "next/headers";

import { getWebConfig } from "../../lib/server-config";
import { describeConfig } from "../../lib/config";
import {
  CORRELATION_HEADER,
  correlationHeaders,
  resolveCorrelationId,
} from "../../lib/correlation";
import { HomeWorkbench } from "../nav/home-workbench";

/**
 * Home (T1303 / WEB-02, G02).
 *
 * docs/05 §2 makes Home a workspace, not a status page: the reader's
 * projects, what needs attention and the entry into starting or continuing
 * research, with a sign-in / create-account entry when there is no session.
 * The body is the shared session-aware workbench (app/nav/home-workbench.tsx)
 * because the signed-in half of the page is client state — the session is a
 * cookie on the API origin, which this server can neither read nor assume.
 *
 * The development-status PANEL that used to be this page's main content moved
 * to /status (app/(auth)/status/page.tsx), the administration surface; the
 * home only links to it. What stays on the home is the render's trace
 * identity (docs/26 §2): the correlation id is resolved once here and travels
 * on the server-side reachability probes to the API and the scientific
 * adapter, and is rendered as a data attribute so an operator can copy it and
 * grep every log for one page render. Losing it turned the distributed-trace
 * gate red (tests/observability/trace-e2e.sh phase 4); the probes are the
 * propagation, not a diagnostic display.
 */

const cfg = getWebConfig();

interface HealthPayload {
  status?: string;
}

/**
 * fetchHealth performs one server-side reachability probe. Its body is
 * irrelevant to Home — the point is that the request carries the page's
 * correlation id into the service's own log. A failure is absorbed (a probe
 * never fails the render); a configuration failure still fails the page,
 * because getWebConfig() throws at startup.
 */
async function fetchHealth(
  baseUrl: string,
  path: string,
  correlationId: string,
): Promise<HealthPayload | null> {
  try {
    const res = await fetch(`${baseUrl}${path}`, {
      cache: "no-store",
      headers: correlationHeaders(correlationId),
    });
    const body: unknown = await res.json();
    if (typeof body !== "object" || body === null) return null;
    return body as HealthPayload;
  } catch {
    // Service unreachable: the probe reports nothing, the render continues.
    return null;
  }
}

export default async function Home() {
  // T0007: the correlation id is resolved once per render — honour the
  // caller's id when it is valid, otherwise create the edge id here — and
  // then propagates to the API and the adapter on every server-side call.
  const requestHeaders = await headers();
  const correlationId = resolveCorrelationId(
    requestHeaders.get(CORRELATION_HEADER),
  );

  // The probes carry the id across both boundaries. Their results are not
  // rendered here (the status panel lives at /status); they exist so one id
  // traces this render end to end.
  await Promise.all([
    fetchHealth(cfg.apiBaseUrl, "/healthz", correlationId),
    fetchHealth(cfg.apiBaseUrl, "/readyz", correlationId),
    fetchHealth(cfg.scientificAdapterUrl, "/healthz", correlationId),
    fetchHealth(cfg.scientificAdapterUrl, "/readyz", correlationId),
  ]);

  // One structured server log line per render; URLs are redacted via the
  // shared redactUrl (describeConfig) — never logged raw.
  //
  // The msg still says "status render" because tests/observability/trace-e2e.sh
  // (Supervisor scope) greps web.log for that exact substring in phase 4, and
  // renaming it here alone would turn the distributed-trace gate red. The
  // historical name predates T1303, when this page WAS the status page; the
  // status panel itself now lives at /status. Renaming both sides together is
  // a follow-up for the Supervisor, not a Worker-side change.
  console.log(
    JSON.stringify({
      msg: "web: status render",
      correlation_id: correlationId,
      config: describeConfig(cfg),
    }),
  );

  return (
    <>
      {/* The render's trace id. It is a public routing value, not a secret:
          an operator (or the observability harness) greps the service logs
          for it. A <meta> rather than a data-* attribute because the i18n
          suite requires every data-* value to be identical across locales,
          and this one is per request by design. */}
      <meta name="correlation-id" content={correlationId} />
      <HomeWorkbench apiBaseUrl={cfg.apiBaseUrl} />
    </>
  );
}
