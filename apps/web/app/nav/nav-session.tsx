"use client";

import { createContext, useContext, useEffect, useMemo, useState } from "react";

import { createAuthClient, type AuthUser } from "../../lib/auth";

/**
 * Header session state (T0101 client, reused for the global nav): the
 * desktop account menu and the mobile drawer both need the current session,
 * so it is resolved once here and shared through context instead of issuing
 * one /session request per consumer. An unreachable API reads as signed-out,
 * exactly like the home page's auth strip — the nav never crashes on it.
 */
export interface NavSessionState {
  user: AuthUser | null;
  loading: boolean;
  busy: boolean;
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
        setBusy(false);
      });
  }

  return (
    <NavSessionContext.Provider value={{ user, loading, busy, signOut }}>
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
