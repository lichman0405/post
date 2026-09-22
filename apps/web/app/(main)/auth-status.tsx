"use client";

import { useEffect, useMemo, useState } from "react";
import { Button, Link, Spinner, Text } from "@primer/react";
import { PersonIcon, SignInIcon } from "@primer/octicons-react";

import { createAuthClient, type AuthUser } from "../../lib/auth";
import { useT } from "../i18n-provider";

/**
 * The sign-in state strip (client component): resolves the current session
 * from the API on mount and offers sign-in / sign-out. The API origin is
 * passed in from the server page; nothing here touches the environment.
 */
export function AuthStatus({ apiBaseUrl }: { apiBaseUrl: string }) {
  const t = useT();
  const client = useMemo(() => createAuthClient(apiBaseUrl), [apiBaseUrl]);
  const [user, setUser] = useState<AuthUser | null>(null);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    let cancelled = false;
    client
      .session()
      .then((session) => {
        if (!cancelled) setUser(session?.user ?? null);
      })
      .catch(() => {
        if (!cancelled) setUser(null); // the API being down is not a crash
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [client]);

  async function signOut() {
    setBusy(true);
    try {
      await client.logout();
    } catch {
      // Even if revocation failed, the UI signs out locally; the session
      // store expiry still bounds the cookie's life.
    } finally {
      setUser(null);
      setBusy(false);
    }
  }

  if (loading) {
    return (
      <div className="auth-status" aria-live="polite" aria-busy="true">
        {/* The Spinner was never unlabelled: Primer's srText defaults to
            "Loading" (dist/Spinner/Spinner.js:46) and renders it in a
            VisuallyHidden span. But an `aria-label` here does NOTHING —
            Primer puts aria-hidden="true" on the <svg> unconditionally
            (:139) and skips the VisuallyHidden span as soon as an
            aria-label is passed (:50, :157), so the name would land on a
            hidden element and be announced by nobody. The visible
            sentence is the announcement; suppressing the spinner's own
            text is Primer's documented shape for that, and the shape the
            other loading states in this app already use. */}
        <Spinner size="small" srText={null} />
        <Text>{t("nav.checkingSession")}</Text>
      </div>
    );
  }

  if (user === null) {
    return (
      <div className="auth-status" aria-live="polite">
        <SignInIcon size={16} aria-hidden />
        <Text>
          {/* T1105: the sentence is split at the link, not interpolated —
              a catalog value can only carry text, and the link is an
              element. The joiner is a literal space in the markup so that
              neither language has to encode one. */}
          <Link href="/login">{t("nav.signIn")}</Link>{" "}
          {t("home.signInPrompt")}
        </Text>
      </div>
    );
  }

  return (
    <div className="auth-status" aria-live="polite">
      <PersonIcon size={16} aria-hidden />
      <Text>
        {t("nav.signedInAs", { name: user.display_name || user.handle })}{" "}
        <span className="auth-email">({user.email})</span>
      </Text>
      {/* The profile URL is id-keyed and stable (T0102): it never changes
          when the owner renames their handle. */}
      <Link href={`/users/${user.id}`}>{t("nav.viewProfile")}</Link>
      <Button variant="invisible" size="small" disabled={busy} onClick={() => void signOut()}>
        {busy ? t("nav.signingOut") : t("nav.signOut")}
      </Button>
    </div>
  );
}
