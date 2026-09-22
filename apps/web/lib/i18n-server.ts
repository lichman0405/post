import { cookies } from "next/headers";

import { LOCALE_COOKIE, normalizeLocale, translate, type Locale } from "./i18n";

/**
 * The server-side half of the language preference (T1105).
 *
 * Server components cannot read React context — the provider in
 * app/providers.tsx is a client boundary — so they read the cookie directly
 * instead. This is the only place besides app/layout.tsx that does so, and
 * both go through normalizeLocale, so an unrecognized cookie value degrades
 * to the default in one place rather than per caller.
 *
 * Keeping this in its own module (rather than exporting it from lib/i18n.ts)
 * is deliberate: importing `next/headers` would make lib/i18n.ts
 * unimportable by the plain-Node unit tests in apps/web/lib/*.test.mjs,
 * which is how the catalog's parity and missing-key rules are pinned. The
 * mechanism stays testable by staying free of framework imports.
 */

export async function getLocale(): Promise<Locale> {
  const cookieStore = await cookies();
  return normalizeLocale(cookieStore.get(LOCALE_COOKIE)?.value);
}

/** The translator, bound to the request's locale, for server components.
 *  Same signature and same missing-key policy as the client `useT()`. */
export async function getT(): Promise<{
  locale: Locale;
  t: (key: string, vars?: Record<string, string | number>) => string;
}> {
  const locale = await getLocale();
  return {
    locale,
    t: (key, vars) => translate(locale, key, vars),
  };
}
