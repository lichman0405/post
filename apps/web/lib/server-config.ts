/**
 * Server-side configuration access (T0006 fail-closed closure).
 *
 * lib/config.ts ships a validated loader; this module is the thing that
 * actually calls it at runtime. getWebConfig() loads and validates once per
 * server process and throws ConfigError naming every offending variable —
 * there is no silent localhost fallback anywhere.
 *
 * instrumentation.ts calls getWebConfig() during server startup so a broken
 * environment kills the process before it answers anything; the page
 * imports the same cached instance. Module-level caching makes the loader's
 * cost a one-time startup event, not a per-request one.
 */
import { describeConfig, loadWebConfig } from "./config";
import type { WebConfig } from "./config";

let cached: WebConfig | null = null;

/**
 * The validated web configuration, loaded exactly once. Throws ConfigError
 * (which names the offending variable and its fix) when the environment is
 * missing or invalid.
 */
export function getWebConfig(): WebConfig {
  if (cached === null) {
    cached = loadWebConfig(process.env);
    // Safe summary line: URLs pass through redactUrl, never raw values.
    console.log(describeConfig(cached));
  }
  return cached;
}
