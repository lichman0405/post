import type { Metadata } from "next";
import { headers } from "next/headers";

import { StatusPanel } from "../../(main)/status-panel";
import type { ServiceStatus } from "../../(main)/status-panel";
import { describeConfig } from "../../../lib/config";
import {
  CORRELATION_HEADER,
  correlationHeaders,
  resolveCorrelationId,
} from "../../../lib/correlation";
import { getT } from "../../../lib/i18n-server";
import { getWebConfig } from "../../../lib/server-config";
import pkg from "../../../package.json";

/**
 * Service diagnostics — the administration surface (T1303 / WEB-02).
 *
 * This is the panel that used to BE the home page. docs/70 §3 (G02) asks for
 * the home's service diagnostics to move to a suitable administration
 * location once Home becomes the research workspace, and this is that
 * location: it reports the API's and the scientific adapter's own health,
 * the build version and the render's correlation id — the things an operator
 * greps for, and nothing a researcher needs on their home page.
 *
 * WHY IT LIVES IN THE (auth) ROUTE GROUP: the URL is /status (the group is
 * not part of the path), and that group's shell is the wordmark-only header
 * — the right chrome for a page that is not part of the product's browsing
 * surface. It is linked from the home page's muted administration link and
 * nowhere else.
 *
 * The validated configuration is in effect at runtime: API_BASE_URL and
 * SCIENTIFIC_ADAPTER_URL come from loadWebConfig() via getWebConfig(), which
 * throws ConfigError when the environment is missing or invalid. There is no
 * silent localhost fallback — a missing variable fails the render and names
 * itself. Fetching happens server-side only; the web app has no backend of
 * its own (docs/51).
 */
const cfg = getWebConfig();

interface HealthPayload {
  service?: string;
  status?: string;
  version?: string;
  checks?: Record<string, { status?: string; detail?: string }>;
}

/**
 * fetchHealth reads a health endpoint for the status page. The body is read
 * even on non-2xx: /readyz deliberately answers 503 with the per-dependency
 * truth, and the status page must show that truth. An unreachable service
 * reports "down" — the page never fails, but configuration failures fail
 * the page (they are a startup error, not a service outage).
 *
 * The page's correlation id travels in the X-Correlation-ID header on every
 * call, so the API and the adapter log it (docs/26 §2).
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
    // Service unreachable: the status page reports "down", it never fails.
    return null;
  }
}

/** One line of real readiness truth for the status panel. */
function describeReadiness(ready: HealthPayload | null): string {
  if (ready === null) return "readiness unknown";
  // Object.entries, not a computed key: `checks[name]` on an API-supplied
  // object is the dynamic-property-access pattern security/detect-object-
  // injection flags, and entries give the same name/status pairs with no
  // index expression at all.
  const entries = Object.entries(ready.checks ?? {});
  if (ready.status === "ready") return "ready";
  if (entries.length === 0) return "not_ready";
  const states = entries
    .map(([name, check]) => `${name}: ${check.status ?? "?"}`)
    .join(", ");
  return `not_ready (${states})`;
}

function toServiceStatus(
  name: string,
  url: string,
  health: HealthPayload | null,
  ready: HealthPayload | null,
): ServiceStatus {
  if (health && health.status === "ok") {
    return {
      name,
      url,
      state: "ok",
      detail:
        `service=${health.service ?? "?"} version=${health.version ?? "?"} ` +
        `readiness=${describeReadiness(ready)}`,
    };
  }
  return {
    name,
    url,
    state: "down",
    detail: "unreachable or unhealthy",
  };
}

/** The tab title follows the language preference like the panel below it. */
export async function generateMetadata(): Promise<Metadata> {
  const { t } = await getT();
  return { title: t("home.developmentStatus") };
}

export default async function StatusPage() {
  // The correlation id is resolved once per render — honour the caller's id
  // when it is valid, otherwise create the edge id here — and then
  // propagates to the API and the adapter on every fetch.
  const requestHeaders = await headers();
  const correlationId = resolveCorrelationId(
    requestHeaders.get(CORRELATION_HEADER),
  );

  const [apiHealth, apiReady, adapterHealth, adapterReady] = await Promise.all([
    fetchHealth(cfg.apiBaseUrl, "/healthz", correlationId),
    fetchHealth(cfg.apiBaseUrl, "/readyz", correlationId),
    fetchHealth(cfg.scientificAdapterUrl, "/healthz", correlationId),
    fetchHealth(cfg.scientificAdapterUrl, "/readyz", correlationId),
  ]);

  // One structured server log line per render; URLs are redacted via the
  // shared redactUrl (describeConfig) — never logged raw.
  console.log(
    JSON.stringify({
      msg: "web: status render",
      correlation_id: correlationId,
      config: describeConfig(cfg),
    }),
  );

  const services = [
    toServiceStatus("Go API", cfg.apiBaseUrl, apiHealth, apiReady),
    toServiceStatus(
      "Scientific adapter",
      cfg.scientificAdapterUrl,
      adapterHealth,
      adapterReady,
    ),
  ];

  return (
    <StatusPanel
      webVersion={pkg.version}
      services={services}
      correlationId={correlationId}
    />
  );
}
