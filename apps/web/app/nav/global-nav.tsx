import { Suspense } from "react";
import Link from "next/link";
import { GlobeIcon, SearchIcon, ThreeBarsIcon } from "@primer/octicons-react";

import { getT } from "../../lib/i18n-server";
import type { Translate } from "../../lib/i18n";
import { LanguageSwitcher } from "./language-switcher";
import { NavBell, NavBellFallback } from "./nav-bell";
import { MobileNavMenu } from "./mobile-nav-menu";
import { NavLinks } from "./nav-links";
import { NavLinksStatic } from "./nav-links-static";
import { NavSessionProvider } from "./nav-session";
import { NavUserArea } from "./nav-user-area";
import "./nav.css";

/** The drawer toggle as it appears before hydration: closed, inert. */
function MobileMenuToggleFallback({ t }: { t: Translate }) {
  return (
    <div className="global-nav-mobile">
      <button
        type="button"
        className="global-nav-icon global-nav-mobile-toggle"
        aria-label={t("nav.menuLabel")}
        aria-expanded={false}
      >
        <ThreeBarsIcon size={16} aria-hidden />
      </button>
    </div>
  );
}

/**
 * The global header (T0107): GitHub-style light bar with the POST wordmark,
 * the search form (submits to /search, role="search"), the destination row
 * from docs/05 §1 and the notifications/account end group. Everything but
 * the interactive state is server-rendered; the session is resolved once in
 * NavSessionProvider and shared by the desktop account area and the drawer.
 *
 * Search is the header form on desktop and the first drawer entry on narrow
 * screens; Notifications is the bell shortcut on desktop and a drawer entry
 * on narrow screens — each destination appears once per layout.
 *
 * T1105 added the language switcher as the last control in the end group.
 * It is deliberately the LAST tab stop in the header rather than the first:
 * it is a preference, not a destination, and putting it ahead of the
 * destinations would make every keyboard user tab past it to reach the
 * navigation. `async` since T1105 — this is a server component, so its own
 * copy comes from getT() while the interactive children take the client
 * context.
 */
export async function GlobalNav({ apiBaseUrl }: { apiBaseUrl: string }) {
  const { t } = await getT();
  return (
    <header className="global-nav">
      <div className="global-nav-inner">
        <NavSessionProvider apiBaseUrl={apiBaseUrl}>
          <div className="global-nav-start">
            <Link href="/" className="global-nav-logo" aria-label={t("nav.logoLabel")}>
              <GlobeIcon size={20} aria-hidden />
              {/* The wordmark is the product's name, not copy: it stays
                  "POST" in both locales and is therefore not a catalog
                  entry. */}
              <span>POST</span>
            </Link>
            <form className="global-nav-search" role="search" action="/search">
              <label className="sr-only" htmlFor="global-search-input">
                {t("nav.searchLabel")}
              </label>
              <SearchIcon
                className="global-nav-search-icon"
                size={16}
                aria-hidden
              />
              <input
                id="global-search-input"
                type="search"
                name="q"
                placeholder={t("nav.searchLabel")}
                autoComplete="off"
              />
            </form>
            <nav className="global-nav-links" aria-label={t("nav.landmarkLabel")}>
              <Suspense fallback={<NavLinksStatic />}>
                <NavLinks />
              </Suspense>
            </nav>
          </div>
          <div className="global-nav-end">
            <Suspense fallback={<NavBellFallback label={t("nav.notifications")} />}>
              <NavBell />
            </Suspense>
            <NavUserArea />
            <LanguageSwitcher />
            <Suspense fallback={<MobileMenuToggleFallback t={t} />}>
              <MobileNavMenu />
            </Suspense>
          </div>
        </NavSessionProvider>
      </div>
    </header>
  );
}
