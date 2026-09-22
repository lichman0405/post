"use client";

import { createContext, useContext, useMemo } from "react";

import { translate, type Locale } from "../lib/i18n";

/**
 * Carries the language preference from the server render to the client
 * components, and nothing else (T1105).
 *
 * The locale is READ ONCE, on the server, in app/layout.tsx — from the
 * `post_locale` cookie — and travels down as an ordinary prop. That is the
 * whole mechanism: there is no locale in the URL (URL shape belongs to
 * specs/ui), no Accept-Language negotiation, and no client-side detection.
 *
 * Why a context and not a module-scoped "current locale": a module-level
 * variable in a Next server process is shared by every concurrent request,
 * so two visitors asking for two languages would race and one of them would
 * be served the other's. React context is per-render-tree, which is the
 * property this needs.
 *
 * The default context value is null and `useLocale` throws on it rather than
 * quietly assuming English. A component rendered outside the provider is a
 * wiring bug — the provider sits in the root layout, so the only way to
 * reach that state is to break the layout — and a silent English fallback
 * would make the resulting half-translated page look like a translation
 * gap instead of a broken tree. Nothing in the app can hit it today; the
 * point is that it cannot become invisible if it ever happens.
 */
const LocaleContext = createContext<Locale | null>(null);

export function LocaleProvider({
  locale,
  children,
}: {
  locale: Locale;
  children: React.ReactNode;
}) {
  return <LocaleContext.Provider value={locale}>{children}</LocaleContext.Provider>;
}

export function useLocale(): Locale {
  const locale = useContext(LocaleContext);
  if (locale === null) {
    throw new Error(
      "useLocale() was called outside <LocaleProvider>. The provider is mounted by app/layout.tsx via app/providers.tsx; a component outside it would render untranslated.",
    );
  }
  return locale;
}

/** The translator, bound to the current locale.
 *
 *  `t` is memoized on the locale so a component that lists it in a
 *  dependency array is not re-run on every render. Signature is
 *  `(key, vars?) => string` — see lib/i18n.ts for the missing-key policy
 *  (the key is rendered, visibly, rather than silently falling back to
 *  English). */
export function useT(): (key: string, vars?: Record<string, string | number>) => string {
  const locale = useLocale();
  return useMemo(
    () => (key: string, vars?: Record<string, string | number>) => translate(locale, key, vars),
    [locale],
  );
}
