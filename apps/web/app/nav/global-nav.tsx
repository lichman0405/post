import { Suspense } from "react";
import Link from "next/link";
import { GlobeIcon, SearchIcon, ThreeBarsIcon } from "@primer/octicons-react";

import { NavBell, NavBellFallback } from "./nav-bell";
import { MobileNavMenu } from "./mobile-nav-menu";
import { NavLinks } from "./nav-links";
import { NavLinksStatic } from "./nav-links-static";
import { NavSessionProvider } from "./nav-session";
import { NavUserArea } from "./nav-user-area";
import "./nav.css";

/** The drawer toggle as it appears before hydration: closed, inert. */
function MobileMenuToggleFallback() {
  return (
    <div className="global-nav-mobile">
      <button
        type="button"
        className="global-nav-icon global-nav-mobile-toggle"
        aria-label="Global navigation menu"
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
 */
export function GlobalNav({ apiBaseUrl }: { apiBaseUrl: string }) {
  return (
    <header className="global-nav">
      <div className="global-nav-inner">
        <NavSessionProvider apiBaseUrl={apiBaseUrl}>
          <div className="global-nav-start">
            <Link href="/" className="global-nav-logo" aria-label="POST home">
              <GlobeIcon size={20} aria-hidden />
              <span>POST</span>
            </Link>
            <form className="global-nav-search" role="search" action="/search">
              <label className="sr-only" htmlFor="global-search-input">
                Search POST
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
                placeholder="Search POST"
                autoComplete="off"
              />
            </form>
            <nav className="global-nav-links" aria-label="Global">
              <Suspense fallback={<NavLinksStatic />}>
                <NavLinks />
              </Suspense>
            </nav>
          </div>
          <div className="global-nav-end">
            <Suspense fallback={<NavBellFallback />}>
              <NavBell />
            </Suspense>
            <NavUserArea />
            <Suspense fallback={<MobileMenuToggleFallback />}>
              <MobileNavMenu />
            </Suspense>
          </div>
        </NavSessionProvider>
      </div>
    </header>
  );
}
