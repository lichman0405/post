/**
 * Next.js server startup hook (T0006).
 *
 * The validated web configuration is loaded before the server answers
 * anything: a missing or invalid environment throws ConfigError here and
 * the server fails to start — fail closed, with an error naming every
 * offending variable (POST_ENV, API_BASE_URL, SCIENTIFIC_ADAPTER_URL).
 * This is the runtime counterpart of the T0004 loader: configuration is
 * now in effect, not just validated.
 */
import { getWebConfig } from "./lib/server-config";

export async function register(): Promise<void> {
  getWebConfig();
}
