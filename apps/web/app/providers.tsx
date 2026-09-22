"use client";

import { BaseStyles, ThemeProvider } from "@primer/react";

import { LocaleProvider } from "./i18n-provider";
import type { Locale } from "../lib/i18n";

/**
 * Primer theme + base styles (ADR-012: GitHub-style neutral UI). Client
 * component so the theme can react to system preferences at hydration.
 *
 * T1105 mounts the LocaleProvider here rather than in a second wrapper: this
 * is already the one client boundary the root layout crosses, so adding the
 * locale to it keeps the number of client providers at one. `locale` is a
 * plain prop from the server — see app/layout.tsx for where it is read.
 */
export function Providers({
  locale,
  children,
}: {
  locale: Locale;
  children: React.ReactNode;
}) {
  return (
    <LocaleProvider locale={locale}>
      <ThemeProvider colorMode="day">
        <BaseStyles>{children}</BaseStyles>
      </ThemeProvider>
    </LocaleProvider>
  );
}
