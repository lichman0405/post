"use client";

import { BaseStyles, ThemeProvider } from "@primer/react";

/**
 * Primer theme + base styles (ADR-012: GitHub-style neutral UI). Client
 * component so the theme can react to system preferences at hydration.
 */
export function Providers({ children }: { children: React.ReactNode }) {
  return (
    <ThemeProvider colorMode="day">
      <BaseStyles>{children}</BaseStyles>
    </ThemeProvider>
  );
}
