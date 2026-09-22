import type { Metadata } from "next";
import { Suspense } from "react";
import { getWebConfig } from "../../../lib/server-config";
import { getT } from "../../../lib/i18n-server";
import { LoginCard } from "./login-card";

/** T1105: the tab title is copy — it follows the language preference like the
 *  card below it. `generateMetadata` rather than a `metadata` constant
 *  because the locale is only known per request. */
export async function generateMetadata(): Promise<Metadata> {
  const { t } = await getT();
  return { title: t("login.metaTitle") };
}

/**
 * The sign-in page: server component that resolves the validated API origin
 * and hands it to the client-side card. The web app has no backend of its
 * own — authentication runs against the Go API over the wire (T0101).
 *
 * The card reads ?error=<code> (OIDC failure redirects land here), which
 * needs useSearchParams — Suspense keeps the page statically renderable.
 */
export default function LoginPage() {
  const cfg = getWebConfig();
  return (
    <div className="auth-main">
      <Suspense fallback={null}>
        <LoginCard apiBaseUrl={cfg.apiBaseUrl} />
      </Suspense>
    </div>
  );
}
