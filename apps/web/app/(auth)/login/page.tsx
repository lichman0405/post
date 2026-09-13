import type { Metadata } from "next";
import { Suspense } from "react";
import { getWebConfig } from "../../../lib/server-config";
import { LoginCard } from "./login-card";

export const metadata: Metadata = {
  title: "Sign in — POST",
};

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
