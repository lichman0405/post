/**
 * POST web-app configuration (T0004 baseline).
 *
 * The web app validates its own environment, independently of the Go core
 * and the Python scientific adapter: this module reads only web-scoped
 * variables, so a missing web variable is never satisfied by another
 * service's variable and vice versa.
 *
 * Fail-closed contract:
 *  - POST_ENV is mandatory (dev | test | prod): a missing or ambiguous layer
 *    is a startup failure, never a guessed default;
 *  - when NODE_ENV is set (Next.js sets development/production), the layer
 *    must agree with it — NODE_ENV=production with POST_ENV=dev is ambiguous
 *    and refused;
 *  - API_BASE_URL and SCIENTIFIC_ADAPTER_URL are required and must be valid
 *    http(s) URLs — there is no silent localhost fallback;
 *  - errors name the offending variable and say what to do; a URL value is
 *    only ever echoed through redactUrl, so an embedded password can never
 *    reach an error message or a log.
 *
 * This module is standard-library free by construction: it has no imports.
 */

export const LAYERS = ["dev", "test", "prod"] as const;
export type Layer = (typeof LAYERS)[number];

/** One configuration failure: the offending variable, what is wrong, fix. */
export interface ConfigProblem {
  key: string;
  msg: string;
  fix: string;
}

/** Aggregate configuration error; errors never contain secret values. */
export class ConfigError extends Error {
  readonly problems: ConfigProblem[];

  constructor(problems: ConfigProblem[]) {
    super(formatProblems(problems));
    this.name = "ConfigError";
    this.problems = problems;
  }
}

function formatProblems(problems: ConfigProblem[]): string {
  if (problems.length === 1) {
    const p = problems[0];
    return `config: ${p.msg}; ${p.fix}`;
  }
  return (
    `config: ${problems.length} problems:\n` +
    problems.map((p) => `  - ${p.key}: ${p.msg}; ${p.fix}`).join("\n")
  );
}

/** The validated web configuration. */
export interface WebConfig {
  layer: Layer;
  apiBaseUrl: string;
  scientificAdapterUrl: string;
}

/**
 * Strip embedded credentials from a URL before it is ever emitted. Mirrors
 * scripts/speclib.py redact_url() exactly:
 *  - `scheme://user:secret@host` -> `scheme://user:***@host`
 *    (keeps the user, drops the secret);
 *  - token-only userinfo -> `scheme://***@host`;
 *  - scp-like `git@host:path` and URLs without userinfo are unchanged.
 */
export function redactUrl(url: string): string {
  if (!url.includes("://")) return url;
  const schemeIndex = url.indexOf("://");
  const scheme = url.slice(0, schemeIndex);
  const rest = url.slice(schemeIndex + 3);
  if (!rest.includes("@")) return url;
  const at = rest.lastIndexOf("@"); // a password may itself contain '@'
  const userinfo = rest.slice(0, at);
  const hostpart = rest.slice(at + 1);
  if (userinfo === "") return url;
  const colon = userinfo.indexOf(":");
  if (colon !== -1) {
    return `${scheme}://${userinfo.slice(0, colon)}:***@${hostpart}`;
  }
  return `${scheme}://***@${hostpart}`;
}

/** Validate one URL variable: must be an absolute http(s) URL with a host. */
function checkHttpUrl(raw: string): string | null {
  let parsed: URL;
  try {
    parsed = new URL(raw);
  } catch {
    return "must be a valid absolute http(s) URL with a host";
  }
  if (parsed.protocol !== "http:" && parsed.protocol !== "https:") {
    return "must use the http or https scheme";
  }
  if (parsed.hostname === "") {
    return "must be a valid absolute http(s) URL with a host";
  }
  return null;
}

/** Environment source: process.env or a controlled map (tests). */
export type EnvSource = Record<string, string | undefined>;

const REQUIRED_LAYER_FIX =
  "set POST_ENV to dev, test or prod (see .env.example)";

/**
 * Load and validate the web configuration. Throws ConfigError naming every
 * offending variable; succeeds only when the environment is unambiguous.
 */
export function loadWebConfig(env: EnvSource): WebConfig {
  const problems: ConfigProblem[] = [];

  const rawLayer = (env.POST_ENV ?? "").trim();
  let layer: Layer | undefined;
  if (rawLayer === "") {
    problems.push({
      key: "POST_ENV",
      msg: "the configuration layer is not set: refusing to guess",
      fix: REQUIRED_LAYER_FIX,
    });
  } else if (!LAYERS.includes(rawLayer as Layer)) {
    problems.push({
      key: "POST_ENV",
      msg: `unknown configuration layer ${JSON.stringify(rawLayer)}; valid layers are dev, test, prod`,
      fix: REQUIRED_LAYER_FIX,
    });
  } else {
    layer = rawLayer as Layer;
  }

  // Layer must agree with the Next.js runtime environment: a prod runtime on
  // dev configuration (or vice versa) is an ambiguous layer, refuse it.
  const nodeEnv = env.NODE_ENV ?? "";
  if (nodeEnv === "development" && layer !== undefined && layer !== "dev") {
    problems.push({
      key: "POST_ENV",
      msg: `ambiguous layer: NODE_ENV=development but POST_ENV=${layer}`,
      fix: "set POST_ENV=dev for the development runtime",
    });
  }
  if (nodeEnv === "production" && layer !== undefined && layer !== "prod") {
    problems.push({
      key: "POST_ENV",
      msg: `ambiguous layer: NODE_ENV=production but POST_ENV=${layer}`,
      fix: "set POST_ENV=prod for the production runtime",
    });
  }

  const apiBaseUrl = urlVar(env, problems, "API_BASE_URL",
    "the Go API base URL (the web app has no backend of its own)");
  const scientificAdapterUrl = urlVar(env, problems, "SCIENTIFIC_ADAPTER_URL",
    "the scientific adapter base URL");

  if (problems.length > 0 || layer === undefined) {
    throw new ConfigError(problems);
  }

  return {
    layer,
    apiBaseUrl: apiBaseUrl as string,
    scientificAdapterUrl: scientificAdapterUrl as string,
  };
}

/** Validate one required URL variable; returns the raw value or undefined. */
function urlVar(
  env: EnvSource,
  problems: ConfigProblem[],
  key: string,
  purpose: string,
): string | undefined {
  const raw = env[key] ?? "";
  if (raw.trim() === "") {
    problems.push({
      key,
      msg: `required configuration is missing: ${purpose}`,
      fix: `set ${key} to ${purpose} (see .env.example)`,
    });
    return undefined;
  }
  const problem = checkHttpUrl(raw);
  if (problem !== null) {
    // Echo the redacted form only: the URL may embed credentials.
    problems.push({
      key,
      msg: `${key} has an invalid value ${JSON.stringify(redactUrl(raw))}: ${problem}`,
      fix: `set ${key} to a valid absolute http(s) URL (see .env.example)`,
    });
    return undefined;
  }
  return raw;
}

/** One safe startup summary line: URLs pass through redactUrl. */
export function describeConfig(cfg: WebConfig): string {
  return (
    `web config: layer=${cfg.layer} ` +
    `api=${redactUrl(cfg.apiBaseUrl)} ` +
    `adapter=${redactUrl(cfg.scientificAdapterUrl)}`
  );
}
