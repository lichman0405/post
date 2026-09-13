"use client";

import { useMemo, useState } from "react";
import { useRouter, useSearchParams } from "next/navigation";
import { Button, Flash, FormControl, Heading, Link, TextInput } from "@primer/react";
import { MarkGithubIcon } from "@primer/octicons-react";

import {
  ApiError,
  createAuthClient,
  messageForCode,
  messageForOIDCError,
  type AuthClient,
} from "../../../lib/auth";

/**
 * The sign-in card (client component): email+password login/signup and an
 * OIDC path. The API origin is resolved server-side by the page and passed
 * in as a plain string — client components never read the environment.
 *
 * Sessions are cookie-based, so no token ever lives in React state; the
 * CSRF token is kept by the auth client (sessionStorage) and echoed on
 * state changes exactly as the API guard requires.
 */
export function LoginCard({ apiBaseUrl }: { apiBaseUrl: string }) {
  const router = useRouter();
  const searchParams = useSearchParams();
  const [mode, setMode] = useState<"login" | "signup">("login");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  // The API redirects failed OIDC flows to /login?error=<code>; surface
  // the reason up front instead of a silent form.
  const [error, setError] = useState<string | null>(() => {
    const code = searchParams.get("error");
    return code ? messageForOIDCError(code) : null;
  });

  const client: AuthClient = useMemo(
    () => createAuthClient(apiBaseUrl),
    [apiBaseUrl],
  );

  function fail(err: unknown) {
    if (err instanceof ApiError) {
      setError(messageForCode(err.code));
    } else {
      setError(messageForCode("UNKNOWN"));
    }
  }

  async function submit() {
    setBusy(true);
    setError(null);
    try {
      if (mode === "login") {
        await client.login(email.trim(), password);
      } else {
        await client.signup({ email: email.trim(), password });
      }
      router.push("/");
      router.refresh();
    } catch (err) {
      fail(err);
    } finally {
      setBusy(false);
    }
  }

  async function continueWithOIDC() {
    setBusy(true);
    setError(null);
    try {
      const url = await client.oidcAuthorizeUrl();
      // Full-page redirect to the provider: the browser carries the state
      // cookie and returns to the API callback, which lands on "/".
      window.location.assign(url);
    } catch (err) {
      fail(err);
      setBusy(false);
    }
  }

  const heading = mode === "login" ? "Sign in to POST" : "Create your account";

  return (
    <div className="auth-card">
      <Heading as="h1" style={{ fontSize: 22, margin: "0 0 4px" }}>
        {heading}
      </Heading>
      <p className="auth-sub">
        Platform for Open Science &amp; Technology
      </p>

      {error !== null && (
        <Flash variant="danger" style={{ marginBottom: 12 }}>
          {error}
        </Flash>
      )}

      <form
        onSubmit={(event) => {
          event.preventDefault();
          void submit();
        }}
      >
        <FormControl required>
          <FormControl.Label>Email</FormControl.Label>
          <TextInput
            type="email"
            autoComplete="email"
            value={email}
            onChange={(event) => setEmail(event.target.value)}
            block
          />
        </FormControl>
        <FormControl required>
          <FormControl.Label>Password</FormControl.Label>
          <TextInput
            type="password"
            autoComplete={mode === "login" ? "current-password" : "new-password"}
            value={password}
            onChange={(event) => setPassword(event.target.value)}
            block
          />
        </FormControl>

        <Button
          type="submit"
          variant="primary"
          block
          disabled={busy || email.trim() === "" || password === ""}
          style={{ marginTop: 12 }}
        >
          {busy ? "Please wait…" : mode === "login" ? "Sign in" : "Create account"}
        </Button>
      </form>

      <Button
        variant="default"
        block
        disabled={busy}
        onClick={() => void continueWithOIDC()}
        style={{ marginTop: 8 }}
      >
        <MarkGithubIcon size={16} aria-hidden />
        <span style={{ marginLeft: 6 }}>
          Continue with your institution (OIDC)
        </span>
      </Button>

      <p className="auth-sub" style={{ marginTop: 16 }}>
        {mode === "login" ? (
          <>
            New to POST?{" "}
            <Link
              as="button"
              type="button"
              onClick={() => {
                setError(null);
                setMode("signup");
              }}
            >
              Create an account
            </Link>
          </>
        ) : (
          <>
            Already have an account?{" "}
            <Link
              as="button"
              type="button"
              onClick={() => {
                setError(null);
                setMode("login");
              }}
            >
              Sign in
            </Link>
          </>
        )}
      </p>
    </div>
  );
}
