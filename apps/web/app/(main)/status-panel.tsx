"use client";

import { Heading, Label, Text } from "@primer/react";
import { BeakerIcon, ServerIcon } from "@primer/octicons-react";
import { DevStatus } from "@post/ui";
import type { DevStatusState } from "@post/ui";

import { useT } from "../i18n-provider";

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
  const t = useT();
  return (
    <div>
      <header className="status-header">
        <BeakerIcon size={24} aria-hidden />
        <Heading as="h1" className="status-title">
          {t("common.platformTitle")}
        </Heading>
        <Label>{t("home.webVersion", { version: webVersion })}</Label>
      </header>
      {/* A <div>, not a second <main>: (main)/layout.tsx already provides the
          page's single <main id="main"> landmark (docs/06 §10, one non-hidden
          main per document). */}
      <div className="status-main">
        <Heading as="h2" className="status-subtitle">
          {t("home.developmentStatus")}
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
          {t("home.statusNote")}
        </Text>
        {correlationId !== undefined && (
          <Text as="p" className="status-note">
            {/* Split around the <code> rather than interpolated: the id is
                an element here, and a catalog value can only carry text.
                The em dash between the two halves is punctuation, so it is
                the same character in both locales and stays in the markup
                — it is not copy and it is not a catalog entry. */}
            {t("home.requestTraceLabel")} <code>{correlationId}</code> —{" "}
            {t("home.requestTraceHelp")}
          </Text>
        )}
      </div>
    </div>
  );
}
