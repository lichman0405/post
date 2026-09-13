import Link from "next/link";
import { GlobeIcon } from "@primer/octicons-react";
import "../nav/nav.css";

/**
 * Layout for the sign-in surface: the wordmark alone, like GitHub's login
 * page — the full global navigation would invite browsing from a page whose
 * single job is authentication. Styling reuses the global nav classes.
 */
export default function AuthLayout({
  children,
}: Readonly<{ children: React.ReactNode }>) {
  return (
    <>
      <header className="global-nav">
        <div className="global-nav-inner">
          <Link href="/" className="global-nav-logo" aria-label="POST home">
            <GlobeIcon size={20} aria-hidden />
            <span>POST</span>
          </Link>
        </div>
      </header>
      <main id="main" tabIndex={-1}>
        {children}
      </main>
    </>
  );
}
