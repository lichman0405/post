"use client";

import { createContext, useContext, useEffect, useMemo, useState } from "react";

import { createAuthClient, type AuthUser } from "../../lib/auth";

/**
 * Header session state (T0101 client, reused for the global nav): the
 * desktop account menu and the mobile drawer both need the current session,
 * so it is resolved once here and shared through context instead of issuing
 * one /session request per consumer. An unreachable API reads as signed-out,
 * exactly like the home page's auth strip — the nav never crashes on it.
 *
 * T1303 adds the one distinction the anonymous strip could not make: a tab
 * that HAD a session the API no longer accepts. GET /auth/session answers
 * 401 for both an expired session and a revoked one, and for a visitor who
 * never signed in — but the CSRF token the sign-in stored in this tab's
 * sessionStorage is still there only in the first two cases. `ended` is that
 * fact, and the home workbench turns it into a recoverable prompt instead of
 * a silent sign-out.
 */
export interface NavSessionState {
  user: AuthUser | null;
  loading: boolean;
  busy: boolean;
  /** True when this tab had a session that the API no longer accepts. */
  ended: boolean;
  signOut: () => void;
}

const NavSessionContext = createContext<NavSessionState | null>(null);

export function NavSessionProvider({
  apiBaseUrl,
  children,
}: {
  apiBaseUrl: string;
  children: React.ReactNode;
}) {
  const client = useMemo(() => createAuthClient(apiBaseUrl), [apiBaseUrl]);
  const [user, setUser] = useState<AuthUser | null>(null);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [ended, setEnded] = useState(false);

  useEffect(() => {
    let cancelled = false;
    // Read BEFORE the probe: session() clears the stored token when the API
    // answers 401, so asking afterwards would always say "no token".
    const hadSession = client.csrfToken() !== null;
    client
      .session()
      .then((session) => {
        if (cancelled) return;
        setUser(session?.user ?? null);
        // A 401 with a token still in this tab's storage is a session that
        // ended, not a visitor who never signed in.
        setEnded(session === null && hadSession);
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

  function signOut() {
    setBusy(true);
    void client
      .logout()
      .catch(() => {
        // Even if revocation failed, the UI signs out locally; the session
        // store expiry still bounds the cookie's life.
      })
      .finally(() => {
        setUser(null);
        // A deliberate sign-out is not an expired session: no "your
        // session ended" prompt after it.
        setEnded(false);
        setBusy(false);
      });
  }

  return (
    <NavSessionContext.Provider
      value={{ user, loading, busy, ended, signOut }}
    >
      {children}
    </NavSessionContext.Provider>
  );
}

/** The shared session state; must be used inside <NavSessionProvider>. */
export function useNavSession(): NavSessionState {
  const state = useContext(NavSessionContext);
  if (state === null) {
    throw new Error("useNavSession must be used inside NavSessionProvider");
  }
  return state;
}
