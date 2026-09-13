import { GlobalNav } from "../nav/global-nav";
import { getWebConfig } from "../../lib/server-config";

/**
 * Layout for the signed-in and public product surfaces: skip link, the
 * global header (T0107) and one <main id="main"> landmark per page. The
 * validated API origin is resolved once here and handed to the header's
 * session provider; pages keep resolving their own needs as before.
 */
export default function MainLayout({
  children,
}: Readonly<{ children: React.ReactNode }>) {
  const cfg = getWebConfig();
  return (
    <>
      <a href="#main" className="skip-link">
        Skip to content
      </a>
      <GlobalNav apiBaseUrl={cfg.apiBaseUrl} />
      {/* tabIndex={-1} lets the skip link actually focus the landmark. */}
      <main id="main" tabIndex={-1}>
        {children}
      </main>
    </>
  );
}
