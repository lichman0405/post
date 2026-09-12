/**
 * Tests for the web configuration validator (apps/web/lib/config.ts).
 *
 * Plain-ESM test file on purpose: tsc rejects ".ts" import specifiers unless
 * allowImportingTsExtensions is set (apps/web/tsconfig.json is not owned by
 * T0004), while Node ≥ 23.6 runs the imported .ts module directly through
 * type stripping. The module under test remains fully typechecked by
 * `pnpm --filter @post/web typecheck`.
 *
 * Run: node --test "apps/web/lib/config.test.mjs"
 */
import assert from "node:assert/strict";
import { test } from "node:test";

import {
  ConfigError,
  describeConfig,
  loadWebConfig,
  redactUrl,
} from "./config.ts";

// The canary is planted in every secret position; after any code path
// renders output, the output is swept for it.
const CANARY = "canary-web-7f3a9c2e-0004";

/** A minimal valid web environment for one layer. */
function validEnv(layer) {
  return {
    POST_ENV: layer,
    API_BASE_URL: "http://127.0.0.1:8080",
    SCIENTIFIC_ADAPTER_URL: "http://127.0.0.1:9000",
  };
}

function problemsOf(err) {
  assert.ok(err instanceof ConfigError, `expected ConfigError, got ${err}`);
  return err.problems.map((p) => p.key);
}

function assertSwept(what, output) {
  assert.ok(
    !output.includes(CANARY),
    `SECRET LEAK: canary on ${what} output:\n${output}`,
  );
}

test("missing POST_ENV fails fast and names the variable", () => {
  assert.throws(
    () => loadWebConfig({ API_BASE_URL: "http://127.0.0.1:8080" }),
    (err) => {
      const msg = String(err);
      return msg.includes("POST_ENV") && msg.includes("dev, test or prod");
    },
  );
});

test("unknown layer is refused with the valid list", () => {
  assert.throws(
    () => loadWebConfig(validEnv("staging")),
    (err) => {
      const msg = String(err);
      return msg.includes("staging") && msg.includes("dev, test, prod");
    },
  );
});

test("every layer is accepted with a full environment", () => {
  for (const layer of ["dev", "test", "prod"]) {
    const cfg = loadWebConfig(validEnv(layer));
    assert.equal(cfg.layer, layer);
    assert.equal(cfg.apiBaseUrl, "http://127.0.0.1:8080");
    assert.equal(cfg.scientificAdapterUrl, "http://127.0.0.1:9000");
  }
});

test("NODE_ENV/layer mismatch is ambiguous and refused", () => {
  // prod runtime on dev config
  assert.throws(
    () => loadWebConfig({ ...validEnv("dev"), NODE_ENV: "production" }),
    (err) => String(err).includes("ambiguous layer"),
  );
  // dev runtime on prod config
  assert.throws(
    () => loadWebConfig({ ...validEnv("prod"), NODE_ENV: "development" }),
    (err) => String(err).includes("ambiguous layer"),
  );
  // matching pairs pass
  assert.equal(
    loadWebConfig({ ...validEnv("dev"), NODE_ENV: "development" }).layer,
    "dev",
  );
  assert.equal(
    loadWebConfig({ ...validEnv("prod"), NODE_ENV: "production" }).layer,
    "prod",
  );
});

test("missing API_BASE_URL is named — no silent localhost fallback", () => {
  const env = validEnv("dev");
  delete env.API_BASE_URL;
  assert.throws(
    () => loadWebConfig(env),
    (err) => {
      const keys = problemsOf(err);
      return keys.includes("API_BASE_URL") &&
        String(err).includes(".env.example");
    },
  );
});

test("missing SCIENTIFIC_ADAPTER_URL is named independently", () => {
  const env = validEnv("dev");
  delete env.SCIENTIFIC_ADAPTER_URL;
  assert.throws(
    () => loadWebConfig(env),
    (err) => problemsOf(err).includes("SCIENTIFIC_ADAPTER_URL"),
  );
});

test("malformed URLs are named with the offending variable", () => {
  for (const [key, bad] of [
    ["API_BASE_URL", "not-a-url"],
    ["API_BASE_URL", "ftp://127.0.0.1:8080"],
    ["API_BASE_URL", "http://"],
    ["SCIENTIFIC_ADAPTER_URL", "127.0.0.1:9000"],
  ]) {
    assert.throws(
      () => loadWebConfig({ ...validEnv("dev"), [key]: bad }),
      (err) => {
        const msg = String(err);
        return msg.includes(key) && msg.includes("invalid value");
      },
      `${key}=${bad} should be rejected`,
    );
  }
});

test("web variables are never satisfied by Python adapter variables", () => {
  // A fully-configured *Python* environment must not start the web app:
  // the web variables are missing and stay missing.
  const pythonOnly = {
    POST_ENV: "dev",
    POST_SCIENTIFIC_ADAPTER_HOST: "127.0.0.1",
    POST_SCIENTIFIC_ADAPTER_PORT: "9000",
  };
  assert.throws(
    () => loadWebConfig(pythonOnly),
    (err) => {
      const keys = problemsOf(err);
      return keys.includes("API_BASE_URL") &&
        keys.includes("SCIENTIFIC_ADAPTER_URL");
    },
  );
});

test("redactUrl mirrors the speclib precedent", () => {
  const cases = [
    ["https://user:pw@example.com/x", "https://user:***@example.com/x"],
    [
      "postgres://app:p%40ss@db.internal:5432/post",
      "postgres://app:***@db.internal:5432/post",
    ],
    ["https://token-only@host/path", "https://***@host/path"],
    // last '@' delimits userinfo (passwords may contain '@')
    ["https://u:p@ss@word@host/path", "https://u:***@host/path"],
    ["git@github.com:owner/name.git", "git@github.com:owner/name.git"],
    ["http://127.0.0.1:8080", "http://127.0.0.1:8080"],
    ["", ""],
  ];
  for (const [input, want] of cases) {
    assert.equal(redactUrl(input), want, `redactUrl(${input})`);
  }
});

test("canary sweep: secrets never reach web config output", () => {
  // 1. Malformed credential-bearing URL: the error echoes the redacted form.
  const env = {
    ...validEnv("prod"),
    API_BASE_URL: `https://u:${CANARY}@/no-host`,
  };
  let message = "";
  try {
    loadWebConfig(env);
    assert.fail("expected ConfigError");
  } catch (err) {
    message = String(err);
  }
  assertSwept("ConfigError message", message);
  assert.ok(message.includes("***"), "error should show the redacted URL");

  // 2. describeConfig() redacts credential-bearing URLs.
  const cfg = loadWebConfig({
    POST_ENV: "dev",
    API_BASE_URL: `https://u:${CANARY}@127.0.0.1:8080`,
    SCIENTIFIC_ADAPTER_URL: "http://127.0.0.1:9000",
  });
  assertSwept("describeConfig", describeConfig(cfg));
  assert.ok(describeConfig(cfg).includes("***"));

  // 3. Aggregate errors never echo values.
  try {
    loadWebConfig({});
    assert.fail("expected ConfigError");
  } catch (err) {
    assertSwept("aggregate error", String(err));
  }
});

test("config errors aggregate all problems", () => {
  try {
    loadWebConfig({});
    assert.fail("expected ConfigError");
  } catch (err) {
    const keys = problemsOf(err);
    assert.ok(keys.includes("POST_ENV"), "POST_ENV missing from problems");
    assert.ok(keys.includes("API_BASE_URL"), "API_BASE_URL missing");
    assert.ok(
      keys.includes("SCIENTIFIC_ADAPTER_URL"),
      "SCIENTIFIC_ADAPTER_URL missing",
    );
  }
});
