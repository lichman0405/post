"use client";

import { Heading, Label, Text } from "@primer/react";
import { BeakerIcon, ServerIcon } from "@primer/octicons-react";
import { DevStatus } from "@post/ui";
import type { DevStatusState } from "@post/ui";

export interface ServiceStatus {
  name: string;
  url: string;
  state: DevStatusState;
  detail: string;
}

/**
 * Primer-based development status surface (client component; receives plain
 * serializable data fetched server-side from the Go API).
 */
export function StatusPanel({
  webVersion,
  services,
  correlationId,
}: {
  webVersion: string;
  services: ServiceStatus[];
  /** The render's correlation id (T0007): grep every service log with it. */
  correlationId?: string;
}) {
  return (
    <div>
      <header className="status-header">
        <BeakerIcon size={24} aria-hidden />
        <Heading as="h1" className="status-title">
          POST — Platform for Open Science &amp; Technology
        </Heading>
        <Label>web {webVersion}</Label>
      </header>
      {/* A <div>, not a second <main>: (main)/layout.tsx already provides the
          page's single <main id="main"> landmark (docs/06 §10, one non-hidden
          main per document). */}
      <div className="status-main">
        <Heading as="h2" className="status-subtitle">
          Development status
        </Heading>
        <div className="status-list">
          {services.map((s) => (
            <div key={s.name} className="status-row">
              <ServerIcon size={16} aria-hidden />
              <div className="status-row-body">
                <Text className="status-row-title">{s.name}</Text>
                <Text as="p" className="status-row-detail">
                  {s.url} — {s.detail}
                </Text>
              </div>
              <DevStatus name={s.name} state={s.state} />
            </div>
          ))}
        </div>
        <Text as="p" className="status-note">
          The web app has no backend of its own: it only renders status fetched
          over HTTP from the Go API and the scientific adapter.
        </Text>
        {correlationId !== undefined && (
          <Text as="p" className="status-note">
            Request trace: <code>{correlationId}</code> — grep the API, worker
            and adapter logs with this id to follow this render.
          </Text>
        )}
      </div>
    </div>
  );
}
