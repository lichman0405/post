/**
 * Evidence write failures must be failures (docs/70 §6.4, WEB-01 review).
 *
 * A green run whose trace.zip is absent or empty cannot be reviewed, so the
 * journey calls verifyArtifact() after stopping the trace. These tests prove
 * the guard rejects a missing and an empty artifact and accepts a real one.
 */

import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";

import { Evidence } from "./evidence.mjs";

function evidenceFor(outDir) {
  return new Evidence({ runId: "unit", outDir });
}

test("verifyArtifact rejects a trace that was never written", async () => {
  const dir = await mkdtemp(path.join(os.tmpdir(), "web-v2-evidence-"));
  try {
    const missing = path.join(dir, "trace.zip");
    await assert.rejects(
      () => evidenceFor(dir).verifyArtifact(missing, "the Playwright trace"),
      /the Playwright trace was not written/,
    );
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

test("verifyArtifact rejects an empty trace", async () => {
  const dir = await mkdtemp(path.join(os.tmpdir(), "web-v2-evidence-"));
  try {
    const empty = path.join(dir, "trace.zip");
    await writeFile(empty, "");
    await assert.rejects(
      () => evidenceFor(dir).verifyArtifact(empty, "the Playwright trace"),
      /missing or empty/,
    );
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

test("verifyArtifact accepts a non-empty trace", async () => {
  const dir = await mkdtemp(path.join(os.tmpdir(), "web-v2-evidence-"));
  try {
    const trace = path.join(dir, "trace.zip");
    await writeFile(trace, "not a real zip, but a real file");
    const size = await evidenceFor(dir).verifyArtifact(trace, "the Playwright trace");
    assert.ok(size > 0, "a non-empty artifact reports its size");
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});
