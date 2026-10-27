import { GlobalNav } from "../nav/global-nav";
import { NavSessionProvider } from "../nav/nav-session";
import { getWebConfig } from "../../lib/server-config";
import { getT } from "../../lib/i18n-server";

/**
 * Layout for the signed-in and public product surfaces: skip link, the
 * global header (T0107) and one <main id="main"> landmark per page. The
 * validated API origin is resolved once here and handed to the one session
 * provider that serves the header AND the page body (T1303: the home
 * workbench reads the same session, so the header and the page can never
 * disagree about who is signed in).
 */
export default async function MainLayout({
  children,
}: Readonly<{ children: React.ReactNode }>) {
  const cfg = getWebConfig();
  const { t } = await getT();
  return (
    <NavSessionProvider apiBaseUrl={cfg.apiBaseUrl}>
      <a href="#main" className="skip-link">
        {t("shell.skipToContent")}
      </a>
      <GlobalNav />
      {/* tabIndex={-1} lets the skip link actually focus the landmark. */}
      <main id="main" tabIndex={-1}>
        {children}
      </main>
    </NavSessionProvider>
  );
}
