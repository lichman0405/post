/**
 * Correlation ids for the web app (T0007, docs/26 §2).
 *
 * One id traces a page render across every boundary the render crosses:
 * the browser/user hits the web app (optionally carrying X-Correlation-ID),
 * the web app passes the id to the Go API and the scientific adapter via
 * the same header, and every service logs it. The id is also rendered on
 * the status page so a human can copy it and grep every log for it.
 *
 * Accepted shape is the shared one across the monorepo (Go internal/
 * observability, Python post_scientific_adapter.correlation): 8-64 chars
 * of letters, digits, '.', '_' or '-'. Anything else is rejected and a
 * fresh id is generated — an untrusted header must never reach a log.
 *
 * This module has no framework imports; it runs on the Node server side.
 */

import { randomUUID } from "node:crypto";

export const CORRELATION_HEADER = "X-Correlation-ID";

const CORRELATION_RE = /^[A-Za-z0-9][A-Za-z0-9._-]{7,63}$/;

/** Create a fresh correlation id (UUID v4 — 36 chars, within the shape). */
export function newCorrelationId(): string {
  return randomUUID();
}

/** Validate an incoming correlation id. */
export function isValidCorrelationId(id: string): boolean {
  return CORRELATION_RE.test(id);
}

/**
 * Resolve the correlation id for a request: honour a valid incoming header
 * value, otherwise create a fresh edge id.
 */
export function resolveCorrelationId(header: string | null | undefined): string {
  if (header !== null && header !== undefined && header !== "" && isValidCorrelationId(header)) {
    return header;
  }
  return newCorrelationId();
}

/** Fetch options carrying the correlation id (the id, never extra secrets). */
export function correlationHeaders(id: string): Record<string, string> {
  return { [CORRELATION_HEADER]: id };
}
