"use client";

import { useEffect, useMemo, useState, type FormEvent } from "react";
import {
  Button,
  Flash,
  FormControl,
  Heading,
  Link,
  Spinner,
  Text,
  TextInput,
  Textarea,
} from "@primer/react";
import { PencilIcon, PersonIcon } from "@primer/octicons-react";

import { ApiError, createAuthClient, type AuthSession } from "../../../../lib/auth";
import {
  createProfileClient,
  messageForProfileCode,
  type ProfileUser,
} from "../../../../lib/profile";
import { formatCalendarDate } from "../../../../lib/i18n";
import { useT } from "../../../i18n-provider";

/**
 * The profile card (client component): fetches the public profile and the
 * current session in parallel, renders the public fields (handle, display
 * name, bio, joined date — email is not part of the public payload and is
 * never shown here), and offers the owner an inline edit form.
 *
 * Owner-only editing is enforced by the API (403 AUTH_FORBIDDEN); showing
 * the form only to the owner is UX, not the security boundary.
 */
export function ProfileCard({
  apiBaseUrl,
  userId,
}: {
  apiBaseUrl: string;
  userId: string;
}) {
  const t = useT();
  const profileClient = useMemo(
    () => createProfileClient(apiBaseUrl),
    [apiBaseUrl],
  );
  const authClient = useMemo(() => createAuthClient(apiBaseUrl), [apiBaseUrl]);

  const [profile, setProfile] = useState<ProfileUser | null>(null);
  const [session, setSession] = useState<AuthSession | null>(null);
  const [loading, setLoading] = useState(true);
  const [notFound, setNotFound] = useState(false);
  const [loadError, setLoadError] = useState<string | null>(null);

  const [editing, setEditing] = useState(false);
  const [handle, setHandle] = useState("");
  const [displayName, setDisplayName] = useState("");
  const [bio, setBio] = useState("");
  const [busy, setBusy] = useState(false);
  const [formError, setFormError] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);

  useEffect(() => {
    let cancelled = false;
    profileClient
      .get(userId)
      .then((p) => {
        if (cancelled) return;
        setProfile(p);
        setHandle(p.handle);
        setDisplayName(p.display_name);
        setBio(p.bio);
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        if (err instanceof ApiError && err.status === 404) {
          setNotFound(true);
        } else {
          setLoadError(
            messageForProfileCode(err instanceof ApiError ? err.code : "UNKNOWN"),
          );
        }
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    // The session decides whether the edit form appears (owner check) and
    // refreshes the shared CSRF token the PATCH needs.
    authClient
      .session()
      .then((s) => {
        if (!cancelled) setSession(s);
      })
      .catch(() => {
        /* a down API is not a crash; the card stays read-only */
      });
    return () => {
      cancelled = true;
    };
  }, [profileClient, authClient, userId]);

  async function save() {
    setBusy(true);
    setFormError(null);
    setSaved(false);
    try {
      const updated = await profileClient.update(userId, {
        handle: handle.trim(),
        display_name: displayName.trim(),
        bio,
      });
      setProfile(updated);
      setHandle(updated.handle);
      setDisplayName(updated.display_name);
      setBio(updated.bio);
      setEditing(false);
      setSaved(true);
    } catch (err: unknown) {
      setFormError(
        messageForProfileCode(err instanceof ApiError ? err.code : "UNKNOWN"),
      );
    } finally {
      setBusy(false);
    }
  }

  if (loading) {
    return (
      // role="status" is what announces the fetch being in flight; the visible
      // sentence is the announcement, so the spinner's own "Loading" srText is
      // suppressed rather than read out twice (Primer's instruction for this
      // case — same shape as search-answer.tsx).
      <div className="profile-card profile-loading" role="status" aria-busy="true">
        <Spinner size="small" srText={null} />
        <Text>{t("profile.loading")}</Text>
      </div>
    );
  }

  if (notFound) {
    return (
      <div className="profile-card">
        <Flash variant="danger">
          {messageForProfileCode("USER_NOT_FOUND")}{" "}
          <Link href="/">{t("profile.backToStatus")}</Link>
        </Flash>
      </div>
    );
  }

  if (profile === null || loadError !== null) {
    return (
      <div className="profile-card">
        <Flash variant="danger">
          {loadError ?? t("profile.error.load")}
        </Flash>
      </div>
    );
  }

  const isOwner = session !== null && session.user.id === profile.id;
  // T1105 / docs/28 §4. Was `toLocaleDateString()` with NO argument, so the
  // text depended on the runtime's default locale AND time zone — the same
  // row printed `9/22/2026` on one host and `22.09.2026` on another, and
  // nothing said which was right. `formatCalendarDate` is an explicit format
  // over the UTC calendar fields: one date per instant, on every host, in
  // both languages (lib/i18n.ts states the choice and its cost).
  const joined = formatCalendarDate(profile.created_at);

  return (
    <div className="profile-card">
      <div className="profile-header">
        <PersonIcon size={24} aria-hidden />
        <div className="profile-header-body">
          <Heading as="h1" className="profile-name">
            {profile.display_name || profile.handle}
          </Heading>
          <Text className="profile-handle">@{profile.handle}</Text>
        </div>
      </div>

      <p className="profile-joined">{t("profile.joined", { date: joined })}</p>

      <p className="profile-bio">
        {profile.bio === "" ? t("profile.noBio") : profile.bio}
      </p>

      {/* T1104: the PATCH's outcome. On success the edit form closes and this
          sentence is the entire report; on failure the form stays open and
          the Flash inside it is the entire report. Neither had a live role,
          so neither reached a screen reader. Success is polite; a rejected
          save is an alert. */}
      {saved && (
        <Flash variant="success" className="profile-flash" role="status">
          {t("profile.saved")}
        </Flash>
      )}

      {isOwner && !editing && (
        <Button
          variant="default"
          size="small"
          className="profile-edit"
          onClick={() => {
            setFormError(null);
            setSaved(false);
            setEditing(true);
          }}
        >
          <PencilIcon size={14} aria-hidden />
          <span className="profile-edit-label">{t("profile.edit")}</span>
        </Button>
      )}

      {isOwner && editing && (
        <form
          className="profile-form"
          onSubmit={(event: FormEvent) => {
            event.preventDefault();
            void save();
          }}
        >
          {formError !== null && (
            <Flash variant="danger" role="alert">
              {formError}
            </Flash>
          )}
          <FormControl>
            <FormControl.Label>{t("profile.handle")}</FormControl.Label>
            <TextInput
              value={handle}
              maxLength={200}
              onChange={(event) => setHandle(event.target.value)}
              block
            />
            {/* The apostrophe in the English copy is a straight one in the
                catalog, so the JSX entity is no longer needed here. */}
            <FormControl.Caption>{t("profile.handleHint")}</FormControl.Caption>
          </FormControl>
          <FormControl>
            <FormControl.Label>{t("profile.displayName")}</FormControl.Label>
            <TextInput
              value={displayName}
              maxLength={200}
              onChange={(event) => setDisplayName(event.target.value)}
              block
            />
          </FormControl>
          <FormControl>
            <FormControl.Label>{t("profile.bio")}</FormControl.Label>
            <Textarea
              value={bio}
              maxLength={2000}
              rows={4}
              onChange={(event) => setBio(event.target.value)}
              block
            />
          </FormControl>
          <div className="profile-form-actions">
            <Button type="submit" variant="primary" disabled={busy}>
              {busy ? t("profile.saving") : t("profile.save")}
            </Button>
            <Button
              type="button"
              variant="default"
              disabled={busy}
              onClick={() => {
                setFormError(null);
                setEditing(false);
              }}
            >
              {t("profile.cancel")}
            </Button>
          </div>
        </form>
      )}
    </div>
  );
}
