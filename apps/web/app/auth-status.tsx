"use client";

import { useEffect, useMemo, useState } from "react";
import { Button, Link, Spinner, Text } from "@primer/react";
import { PersonIcon, SignInIcon } from "@primer/octicons-react";

import { createAuthClient, type AuthUser } from "../lib/auth";

/**
 * The sign-in state strip (client component): resolves the current session
 * from the API on mount and offers sign-in / sign-out. The API origin is
 * passed in from the server page; nothing here touches the environment.
 */
export function AuthStatus({ apiBaseUrl }: { apiBaseUrl: string }) {
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
      <div className="auth-status">
        <Spinner size="small" />
        <Text>Checking session…</Text>
      </div>
    );
  }

  if (user === null) {
    return (
      <div className="auth-status">
        <SignInIcon size={16} aria-hidden />
        <Text>
          <Link href="/login">Sign in</Link> to create and publish research
          objects.
        </Text>
      </div>
    );
  }

  return (
    <div className="auth-status">
      <PersonIcon size={16} aria-hidden />
      <Text>
        Signed in as <strong>{user.display_name || user.handle}</strong>{" "}
        <span className="auth-email">({user.email})</span>
      </Text>
      {/* The profile URL is id-keyed and stable (T0102): it never changes
          when the owner renames their handle. */}
      <Link href={`/users/${user.id}`}>View profile</Link>
      <Button variant="invisible" size="small" disabled={busy} onClick={() => void signOut()}>
        {busy ? "Signing out…" : "Sign out"}
      </Button>
    </div>
  );
}
