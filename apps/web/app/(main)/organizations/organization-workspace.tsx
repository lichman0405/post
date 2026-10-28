"use client";

import { useEffect, useMemo, useState, type FormEvent } from "react";
import Link from "next/link";
import { useRouter } from "next/navigation";
import { Heading, Spinner, Text } from "@primer/react";

import { createAuthClient, loginHref } from "../../../lib/auth";
import {
  ApiError,
  createOrgClient,
  messageForOrgCode,
  type Organization,
} from "./org-client";
import { useNavSession } from "../../nav/nav-session";
import "./organizations.css";

/**
 * The signed-in half of /organizations (T1304 / WEB-03, G03).
 *
 * The organizations index used to be a public lookup only, because there was
 * no organization directory and no way to create one. G03 adds the two
 * governed actions the page was missing: create an organization (the creator
 * becomes its owner) and open the organizations the reader belongs to, whose
 * member page is where invitations, roles and leaving live.
 *
 * The public lookup stays below this block: the read path did not change.
 * There is still NO public directory — GET /api/v1/organizations answers the
 * signed-in actor's own organizations (member-only read), and this component
 * renders exactly that answer rather than inventing a listing.
 */
export function OrganizationWorkspace({ apiBaseUrl }: { apiBaseUrl: string }) {
  const { user, loading } = useNavSession();
  const router = useRouter();
  const authClient = useMemo(() => createAuthClient(apiBaseUrl), [apiBaseUrl]);
  const orgClient = useMemo(
    () => createOrgClient(apiBaseUrl, { csrfToken: () => authClient.csrfToken() }),
    [apiBaseUrl, authClient],
  );

  const [orgs, setOrgs] = useState<Organization[] | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);

  const [slug, setSlug] = useState("");
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [creating, setCreating] = useState(false);
  const [createError, setCreateError] = useState<string | null>(null);

  useEffect(() => {
    if (user === null) return;
    // A reloaded tab may hold the session cookie but no CSRF token; the
    // session probe also refreshes the token every client here reads.
    void authClient.session().catch(() => null);
    let cancelled = false;
    orgClient
      .list()
      .then((list) => {
        if (!cancelled) setOrgs(list);
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        setLoadError(
          messageForOrgCode(err instanceof ApiError ? err.code : "UNKNOWN"),
        );
      });
    return () => {
      cancelled = true;
    };
  }, [user, authClient, orgClient]);

  async function submitCreate(event: FormEvent) {
    event.preventDefault();
    setCreateError(null);
    setCreating(true);
    try {
      const created = await orgClient.create({
        slug: slug.trim(),
        name: name.trim(),
        description: description.trim(),
      });
      setSlug("");
      setName("");
      setDescription("");
      // Straight to the member page: the next thing an owner does after
      // creating an organization is invite the people who join it.
      router.push(
        `/organizations/${encodeURIComponent(created.organization.slug)}/members`,
      );
    } catch (err: unknown) {
      setCreateError(
        messageForOrgCode(err instanceof ApiError ? err.code : "UNKNOWN"),
      );
    } finally {
      setCreating(false);
    }
  }

  return (
    <section className="org-workspace" data-org-workspace aria-labelledby="org-workspace-heading">
      <Heading as="h2" id="org-workspace-heading" className="org-workspace-title">
        Your organizations
      </Heading>

      {loading ? (
        <div className="org-workspace-loading" role="status" aria-busy="true">
          <Spinner size="small" srText={null} />
          <Text>Checking session…</Text>
        </div>
      ) : user === null ? (
        // An anonymous reader can still look up one public profile, but
        // creating and governing an organization needs an account — say so
        // and offer the entry rather than a form that can only fail.
        <p className="org-workspace-anon" data-org-anonymous>
          <Link href={loginHref("/organizations")} data-org-signin>
            Sign in
          </Link>{" "}
          to create an organization and manage its members.
        </p>
      ) : (
        <>
          {loadError !== null ? (
            <div className="org-workspace-error" role="alert">
              {loadError}
            </div>
          ) : orgs === null ? (
            <div className="org-workspace-loading" role="status" aria-busy="true">
              <Spinner size="small" srText={null} />
              <Text>Loading your organizations…</Text>
            </div>
          ) : orgs.length === 0 ? (
            <p className="org-workspace-empty" data-org-list="empty">
              No organizations yet. Create one below.
            </p>
          ) : (
            <ul className="org-workspace-list" data-org-list="list">
              {orgs.map((org) => (
                <li key={org.id} className="org-workspace-row" data-org-row={org.slug}>
                  <span className="org-workspace-row-main">
                    <span className="org-workspace-name">{org.name}</span>
                    <span className="org-workspace-slug">@{org.slug}</span>
                  </span>
                  <Link
                    href={`/organizations/${encodeURIComponent(org.slug)}/members`}
                    className="org-workspace-manage"
                    data-org-manage={org.slug}
                  >
                    Members
                  </Link>
                </li>
              ))}
            </ul>
          )}

          <Heading as="h3" className="org-create-title">
            Create an organization
          </Heading>
          <form className="org-create-form" data-org-create onSubmit={submitCreate}>
            <div className="org-field">
              <label className="org-label" htmlFor="org-create-name">
                Name
              </label>
              <input
                id="org-create-name"
                className="org-input"
                data-org-create-name
                value={name}
                onChange={(event) => setName(event.target.value)}
                maxLength={200}
                required
              />
            </div>
            <div className="org-field">
              <label className="org-label" htmlFor="org-create-slug">
                Slug
              </label>
              <input
                id="org-create-slug"
                className="org-input"
                data-org-create-slug
                value={slug}
                onChange={(event) => setSlug(event.target.value)}
                placeholder="institute-of-catalysis"
                maxLength={64}
                required
              />
              <p className="org-help">
                Lowercase letters, digits and dashes. The slug is the
                organization&apos;s public address and cannot be renamed.
              </p>
            </div>
            <div className="org-field">
              <label className="org-label" htmlFor="org-create-description">
                Description
              </label>
              <textarea
                id="org-create-description"
                className="org-input org-textarea"
                data-org-create-description
                value={description}
                onChange={(event) => setDescription(event.target.value)}
                maxLength={4000}
                rows={3}
              />
            </div>
            {createError !== null ? (
              <div className="org-form-error" role="alert" data-org-create-error>
                {createError}
              </div>
            ) : null}
            <div className="org-form-actions">
              <button
                type="submit"
                className="org-button org-button-primary"
                data-org-create-submit
                disabled={creating}
              >
                {creating ? "Creating…" : "Create organization"}
              </button>
            </div>
          </form>
        </>
      )}
    </section>
  );
}
