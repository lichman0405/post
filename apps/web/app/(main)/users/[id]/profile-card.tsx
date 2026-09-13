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
      <div className="profile-card profile-loading">
        <Spinner size="small" />
        <Text>Loading profile…</Text>
      </div>
    );
  }

  if (notFound) {
    return (
      <div className="profile-card">
        <Flash variant="danger">
          {messageForProfileCode("USER_NOT_FOUND")}{" "}
          <Link href="/">Back to status</Link>
        </Flash>
      </div>
    );
  }

  if (profile === null || loadError !== null) {
    return (
      <div className="profile-card">
        <Flash variant="danger">
          {loadError ?? "The profile could not be loaded."}
        </Flash>
      </div>
    );
  }

  const isOwner = session !== null && session.user.id === profile.id;
  const joined = new Date(profile.created_at).toLocaleDateString();

  return (
    <div className="profile-card">
      <div className="profile-header">
        <PersonIcon size={24} aria-hidden />
        <div className="profile-header-body">
          <Heading as="h1" style={{ fontSize: 20, margin: 0 }}>
            {profile.display_name || profile.handle}
          </Heading>
          <Text className="profile-handle">@{profile.handle}</Text>
        </div>
      </div>

      <p className="profile-joined">Joined {joined}</p>

      <p className="profile-bio">
        {profile.bio === "" ? "No bio yet." : profile.bio}
      </p>

      {saved && (
        <Flash variant="success" style={{ marginTop: 12 }}>
          Profile updated.
        </Flash>
      )}

      {isOwner && !editing && (
        <Button
          variant="default"
          size="small"
          style={{ marginTop: 12 }}
          onClick={() => {
            setFormError(null);
            setSaved(false);
            setEditing(true);
          }}
        >
          <PencilIcon size={14} aria-hidden />
          <span style={{ marginLeft: 6 }}>Edit profile</span>
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
          {formError !== null && <Flash variant="danger">{formError}</Flash>}
          <FormControl>
            <FormControl.Label>Handle</FormControl.Label>
            <TextInput
              value={handle}
              maxLength={200}
              onChange={(event) => setHandle(event.target.value)}
              block
            />
            <FormControl.Caption>
              Lowercase letters, digits and dashes; changing it does not
              change this page&apos;s address.
            </FormControl.Caption>
          </FormControl>
          <FormControl>
            <FormControl.Label>Display name</FormControl.Label>
            <TextInput
              value={displayName}
              maxLength={200}
              onChange={(event) => setDisplayName(event.target.value)}
              block
            />
          </FormControl>
          <FormControl>
            <FormControl.Label>Bio</FormControl.Label>
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
              {busy ? "Saving…" : "Save"}
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
              Cancel
            </Button>
          </div>
        </form>
      )}
    </div>
  );
}
