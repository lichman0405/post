import Link from "next/link";
import { GlobeIcon } from "@primer/octicons-react";
import "../nav/nav.css";

import { getT } from "../../lib/i18n-server";
import { LanguageSwitcher } from "../nav/language-switcher";

/**
 * Layout for the sign-in surface: the wordmark alone, like GitHub's login
 * page — the full global navigation would invite browsing from a page whose
 * single job is authentication. Styling reuses the global nav classes.
 *
 * The language switcher is the one control this layout does share with the
 * product chrome, and deliberately: `/login` is a page a reader has to be
 * able to read before they can sign in, so a language preference that can
 * only be changed once you are already through the door is not a
 * preference. It sits in the same end-group position as in the full header.
 */
export default async function AuthLayout({
  children,
}: Readonly<{ children: React.ReactNode }>) {
  const { t } = await getT();
  return (
    <>
      <a href="#main" className="skip-link">
        {t("shell.skipToContent")}
      </a>
      <header className="global-nav">
        <div className="global-nav-inner">
          <Link href="/" className="global-nav-logo" aria-label={t("nav.logoLabel")}>
            <GlobeIcon size={20} aria-hidden />
            <span>POST</span>
          </Link>
          <div className="global-nav-end">
            <LanguageSwitcher />
          </div>
        </div>
      </header>
      {/* tabIndex={-1} lets the skip link actually focus the landmark. */}
      <main id="main" tabIndex={-1}>
        {children}
      </main>
    </>
  );
}
