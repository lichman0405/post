/**
 * Tests for the web correlation-id module (apps/web/lib/correlation.ts).
 *
 * Same pattern as config.test.mjs: plain ESM importing the real .ts module
 * through Node's type stripping; the module remains fully typechecked by
 * `pnpm --filter @post/web typecheck`.
 *
 * Run: node --test "apps/web/lib/correlation.test.mjs"
 */
import { test } from "node:test";
import assert from "node:assert/strict";

import {
  CORRELATION_HEADER,
  isValidCorrelationId,
  newCorrelationId,
  resolveCorrelationId,
  correlationHeaders,
} from "./correlation.ts";

test("header name is the shared X-Correlation-ID", () => {
  assert.equal(CORRELATION_HEADER, "X-Correlation-ID");
});

test("generated ids are valid, unique and within the shared shape", () => {
  const seen = new Set();
  for (let i = 0; i < 100; i++) {
    const id = newCorrelationId();
    assert.ok(isValidCorrelationId(id), `generated id ${id} invalid`);
    assert.ok(!seen.has(id), `duplicate generated id ${id}`);
    seen.add(id);
  }
});

test("validation accepts the shared shape and rejects hostile input", () => {
  const valid = [
    "0123456789abcdef0123456789abcdef",
    "3f9c21e5-b8d4-4c0a-9f1e-7d3b2a91c4f8",
    "web-trace-abc123",
    "a.b_c-1x",
  ];
  for (const id of valid) assert.ok(isValidCorrelationId(id), `rejected ${id}`);

  const invalid = [
    "",
    "short",
    "a".repeat(65),
    "has space",
    "../../etc/passwd",
    "a;log-injection",
    "a\nb",
    "-leading",
    "日本国",
  ];
  for (const id of invalid) {
    assert.ok(!isValidCorrelationId(id), `accepted hostile input ${JSON.stringify(id)}`);
  }
});

test("resolve honours a valid incoming id", () => {
  assert.equal(resolveCorrelationId("web-trace-abc123"), "web-trace-abc123");
});

test("resolve generates a fresh valid id for absent or invalid input", () => {
  for (const input of [null, undefined, "", "bad id with spaces", "x"]) {
    const id = resolveCorrelationId(input);
    assert.ok(isValidCorrelationId(id), `resolve(${JSON.stringify(input)}) = ${id} invalid`);
    assert.notEqual(id, input ?? "");
  }
});

test("correlationHeaders carries the id under the shared header", () => {
  assert.deepEqual(correlationHeaders("abc-12345678"), {
    "X-Correlation-ID": "abc-12345678",
  });
});
