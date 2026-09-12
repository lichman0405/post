import { StatusPanel } from "./status-panel";
import type { ServiceStatus } from "./status-panel";
import { getWebConfig } from "../lib/server-config";
import pkg from "../package.json";

// The validated configuration is in effect at runtime (T0006 closure of the
// T0004 gap): API_BASE_URL and SCIENTIFIC_ADAPTER_URL come from
// loadWebConfig() via getWebConfig(), which throws ConfigError when the
// environment is missing or invalid. There is no silent localhost fallback
// — a missing variable fails the render and names itself. Fetching still
// happens server-side only; the web app has no backend of its own (docs/51).
const cfg = getWebConfig();

interface HealthPayload {
  service?: string;
  status?: string;
  version?: string;
  checks?: Record<string, { status?: string; detail?: string }>;
}

// fetchHealth reads a health endpoint for the status page. The body is read
// even on non-2xx: /readyz deliberately answers 503 with the per-dependency
// truth, and the status page must show that truth. An unreachable service
// reports "down" — the page never fails, but configuration failures fail
// the page (they are a startup error, not a service outage).
async function fetchHealth(baseUrl: string, path: string): Promise<HealthPayload | null> {
  try {
    const res = await fetch(`${baseUrl}${path}`, { cache: "no-store" });
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
  const checks = ready.checks ?? {};
  const names = Object.keys(checks);
  if (ready.status === "ready") return "ready";
  if (names.length === 0) return "not_ready";
  const states = names
    .map((n) => `${n}: ${checks[n]?.status ?? "?"}`)
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

export default async function Home() {
  const [apiHealth, apiReady, adapterHealth, adapterReady] = await Promise.all([
    fetchHealth(cfg.apiBaseUrl, "/healthz"),
    fetchHealth(cfg.apiBaseUrl, "/readyz"),
    fetchHealth(cfg.scientificAdapterUrl, "/healthz"),
    fetchHealth(cfg.scientificAdapterUrl, "/readyz"),
  ]);

  const services = [
    toServiceStatus("Go API", cfg.apiBaseUrl, apiHealth, apiReady),
    toServiceStatus(
      "Scientific adapter",
      cfg.scientificAdapterUrl,
      adapterHealth,
      adapterReady,
    ),
  ];

  return <StatusPanel webVersion={pkg.version} services={services} />;
}
