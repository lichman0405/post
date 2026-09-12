import { StatusPanel } from "./status-panel";
import type { ServiceStatus } from "./status-panel";
import pkg from "../package.json";

// Server-side service discovery for the status page. These fetches run in the
// Next.js server, never in the browser; the web app contains no API or
// business logic of its own (docs/51: all mutation goes through the Go API).
const API_BASE_URL = process.env.API_BASE_URL ?? "http://127.0.0.1:8080";
const ADAPTER_BASE_URL =
  process.env.SCIENTIFIC_ADAPTER_URL ?? "http://127.0.0.1:9000";

interface HealthPayload {
  service?: string;
  status?: string;
  version?: string;
}

async function fetchHealth(baseUrl: string): Promise<HealthPayload | null> {
  try {
    const res = await fetch(`${baseUrl}/healthz`, { cache: "no-store" });
    if (!res.ok) return null;
    return (await res.json()) as HealthPayload;
  } catch {
    // Service unreachable: the status page reports "down", it never fails.
    return null;
  }
}

function toServiceStatus(
  name: string,
  url: string,
  health: HealthPayload | null,
): ServiceStatus {
  if (health && health.status === "ok") {
    return {
      name,
      url,
      state: "ok",
      detail: `service=${health.service ?? "?"} version=${health.version ?? "?"}`,
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
  const [apiHealth, adapterHealth] = await Promise.all([
    fetchHealth(API_BASE_URL),
    fetchHealth(ADAPTER_BASE_URL),
  ]);

  const services = [
    toServiceStatus("Go API", API_BASE_URL, apiHealth),
    toServiceStatus("Scientific adapter", ADAPTER_BASE_URL, adapterHealth),
  ];

  return <StatusPanel webVersion={pkg.version} services={services} />;
}
