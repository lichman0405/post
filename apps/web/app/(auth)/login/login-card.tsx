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
import { useT } from "../../i18n-provider";

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
  const t = useT();
  const router = useRouter();
  const searchParams = useSearchParams();
  const [mode, setMode] = useState<"login" | "signup">("login");
  const [email, setEmail] = useState(() => searchParams.get("email") ?? "");
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

  const heading = mode === "login" ? t("login.heading") : t("login.signupHeading");

  return (
    <div className="auth-card">
      <Heading as="h1" className="auth-heading">
        {heading}
      </Heading>
      <p className="auth-sub">
        {t("login.subtitle")}
      </p>

      {/* T1104: this Flash is the ONLY report a rejected sign-in, sign-up or
          OIDC start ever produces — the form does not navigate away on
          success, it stays put on failure — so without a live role the
          answer to "why did nothing happen?" is silent. `alert` (assertive)
          because it reports the failure of an action the reader just took;
          Primer's Flash renders a plain <div> and has no role of its own
          (@primer/react 38.39.0 dist/Flash/Flash.js forwards ...rest). */}
      {error !== null && (
        <Flash variant="danger" className="auth-flash" role="alert">
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
          <FormControl.Label>{t("login.email")}</FormControl.Label>
          <TextInput
            type="email"
            autoComplete="email"
            value={email}
            onChange={(event) => setEmail(event.target.value)}
            block
          />
        </FormControl>
        <FormControl required>
          <FormControl.Label>{t("login.password")}</FormControl.Label>
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
          className="auth-submit"
        >
          {busy ? t("login.pleaseWait") : mode === "login" ? t("nav.signIn") : t("login.createAccount")}
        </Button>
      </form>

      <Button
        variant="default"
        block
        disabled={busy}
        onClick={() => void continueWithOIDC()}
        className="auth-oidc"
      >
        <MarkGithubIcon size={16} aria-hidden />
        <span className="auth-oidc-label">
          {t("login.oidc")}
        </span>
      </Button>

      <p className="auth-sub auth-sub-after">
        {mode === "login" ? (
          <>
            {t("login.newToPost")}{" "}
            <Link
              as="button"
              type="button"
              onClick={() => {
                setError(null);
                setMode("signup");
              }}
            >
              {t("login.createAnAccount")}
            </Link>
          </>
        ) : (
          <>
            {t("login.alreadyHaveAccount")}{" "}
            <Link
              as="button"
              type="button"
              onClick={() => {
                setError(null);
                setMode("login");
              }}
            >
              {t("nav.signIn")}
            </Link>
          </>
        )}
      </p>
      <p className="auth-sub auth-sub-after">
        Need a demo account? <Link href="/demo">Browse the eight demo roles</Link>.
      </p>
    </div>
  );
}
