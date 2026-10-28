"use client";

import { useEffect, useMemo, useState, type FormEvent } from "react";
import Link from "next/link";
import { useRouter } from "next/navigation";
import { Spinner } from "@primer/react";

import { createAuthClient, loginHref } from "../../../lib/auth";
import { createProfileClient } from "../../../lib/profile";
import {
  ApiError,
  ORG_ROLES,
  createOrgClient,
  membershipActive,
  messageForOrgCode,
  roleLabel,
  type OrgRole,
  type Organization,
  type OrganizationMembership,
} from "./org-client";
import { useNavSession } from "../../nav/nav-session";
import "./organizations.css";

/**
 * The organization member page body (T1304 / WEB-03, G03).
 *
 * docs/04 §6 and the orgs application service draw the rules; this component
 * renders them and never re-implements them:
 *
 *   - every current member may READ the member list (current and historical
 *     rows — an ended affiliation keeps its row: 离职不删除历史);
 *   - only an owner may invite or change a role. The controls are rendered
 *     for everyone on purpose: the refusal is the API's, and a reader who
 *     tries is told in words why nothing happened instead of meeting a
 *     control that silently does nothing. The server check is the boundary,
 *     not this markup;
 *   - any member may end their OWN affiliation; an owner may end anyone's.
 *     The last active owner cannot leave (LAST_OWNER) — again the API's
 *     answer, shown as a refusal;
 *   - an invitation records the affiliation as the owner states it: the
 *     verified flag starts unchecked (inviting is not verifying) and only an
 *     explicit check sets it, once, for that invitation;
 *   - identity is resolved through the public user profile (GET
 *     /api/v1/users/{id}/profile): a membership row carries a stable user id,
 *     and a UUID is not a name. A failed lookup falls back to the id rather
 *     than hiding the row — the member list is a record, not a curated view.
 */

interface Identity {
  handle: string;
  displayName: string;
}

type Notice = { kind: "success" | "error"; text: string } | null;

export function OrganizationMembers({
  apiBaseUrl,
  slug,
}: {
  apiBaseUrl: string;
  slug: string;
}) {
  const router = useRouter();
  const { user, loading } = useNavSession();

  const authClient = useMemo(() => createAuthClient(apiBaseUrl), [apiBaseUrl]);
  const orgClient = useMemo(
    () => createOrgClient(apiBaseUrl, { csrfToken: () => authClient.csrfToken() }),
    [apiBaseUrl, authClient],
  );
  const profileClient = useMemo(() => createProfileClient(apiBaseUrl), [apiBaseUrl]);

  const [org, setOrg] = useState<Organization | null>(null);
  const [orgError, setOrgError] = useState<string | null>(null);
  const [orgMissing, setOrgMissing] = useState(false);
  const [members, setMembers] = useState<OrganizationMembership[] | null>(null);
  const [membersError, setMembersError] = useState<string | null>(null);

  // A Map, not a Record: every lookup is keyed by a membership's user id,
  // and a Map keeps those keys out of dynamic property access (SAST
  // security/detect-object-injection) while still being a plain immutable
  // value React can replace.
  const [identities, setIdentities] = useState<Map<string, Identity>>(
    () => new Map(),
  );
  const [drafts, setDrafts] = useState<Record<string, OrgRole>>({});
  const [busyUser, setBusyUser] = useState<string | null>(null);
  const [confirmUser, setConfirmUser] = useState<string | null>(null);
  const [notice, setNotice] = useState<Notice>(null);

  const [inviteHandle, setInviteHandle] = useState("");
  const [inviteRole, setInviteRole] = useState<OrgRole>("contributor");
  // Unchecked by default, and reset after every successful invite: the
  // verified flag is the OWNER'S STATEMENT that they checked the person's
  // affiliation, not something inviting implies. The server's default is
  // unverified and the flag is shown on the row, so a form that pre-asserts
  // it would put the owner's name behind a claim they never made.
  const [inviteVerified, setInviteVerified] = useState(false);
  const [inviting, setInviting] = useState(false);

  // Resolve the organization the URL names from the reader's own list: the
  // API is id-keyed, and this caller may only resolve an id they are a
  // member of. A slug that is not in the list is left as "not a member"
  // (which is also the answer for an organization that does not exist — the
  // existence of an organization is not disclosed to outsiders, docs/04).
  useEffect(() => {
    if (user === null) return;
    void authClient.session().catch(() => null);
    let cancelled = false;
    orgClient
      .list()
      .then(async (list) => {
        if (cancelled) return;
        const found = list.find((o) => o.slug === slug);
        if (found === undefined) {
          setOrgMissing(true);
          return;
        }
        setOrg(found);
        const rows = await orgClient.listMembers(found.id);
        if (!cancelled) setMembers(rows);
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        setOrgError(messageForOrgCode(err instanceof ApiError ? err.code : "UNKNOWN"));
      });
    return () => {
      cancelled = true;
    };
  }, [user, authClient, orgClient, slug]);

  // A failed member reload is reported next to the table; the page keeps
  // whatever it already rendered.
  function reloadMembers(organizationId: string): Promise<void> {
    return orgClient
      .listMembers(organizationId)
      .then((rows) => {
        setMembers(rows);
        // A fresh read is the new baseline: every stored role is what the
        // selects should show, so no row keeps an unsaved draft.
        setDrafts({});
        setMembersError(null);
      })
      .catch((err: unknown) => {
        setMembersError(
          messageForOrgCode(err instanceof ApiError ? err.code : "UNKNOWN"),
        );
      });
  }

  // Resolve display identities once per member. The profile read is public;
  // a failure is not fatal (the row keeps the stable id).
  useEffect(() => {
    if (members === null) return;
    const unique = [...new Set(members.map((m) => m.user_id))];
    const missing = unique.filter((id) => !identities.has(id));
    if (missing.length === 0) return;
    let cancelled = false;
    void Promise.all(
      missing.map((id) =>
        profileClient
          .get(id)
          .then(
            (p): [string, Identity] => [
              id,
              { handle: p.handle, displayName: p.display_name },
            ],
          )
          .catch((): [string, Identity] => [id, { handle: id, displayName: "" }]),
      ),
    ).then((pairs) => {
      if (cancelled) return;
      setIdentities((prev) => {
        const next = new Map(prev);
        for (const [id, identity] of pairs) next.set(id, identity);
        return next;
      });
    });
    return () => {
      cancelled = true;
    };
  }, [members, identities, profileClient]);

  function identityOf(userId: string): Identity {
    return identities.get(userId) ?? { handle: userId, displayName: "" };
  }

  const myMembership = members?.find((m) => m.user_id === user?.id) ?? null;
  const myRole = myMembership?.role ?? null;
  const isOwner = myMembership !== null && membershipActive(myMembership) && myRole === "owner";

  async function changeRole(member: OrganizationMembership, role: OrgRole) {
    if (org === null || role === member.role) return;
    setBusyUser(member.user_id);
    setNotice(null);
    try {
      const updated = await orgClient.updateRole(org.id, member.user_id, role);
      setMembers((prev) =>
        prev === null
          ? prev
          : prev.map((m) => (m.user_id === member.user_id ? { ...m, role: updated.role } : m)),
      );
      setDrafts((prev) => {
        const next = { ...prev };
        delete next[member.user_id];
        return next;
      });
      setNotice({
        kind: "success",
        text: `Role for @${identityOf(member.user_id).handle} updated to ${roleLabel(updated.role)}.`,
      });
    } catch (err: unknown) {
      // Nothing changed, so the control goes back to what is stored: a
      // select left showing "Owner" after a refused change would be the UI
      // disagreeing with the record. Dropping the draft makes the select
      // derive from the stored role again.
      setDrafts((prev) => {
        const next = { ...prev };
        delete next[member.user_id];
        return next;
      });
      setNotice({
        kind: "error",
        text: messageForOrgCode(err instanceof ApiError ? err.code : "UNKNOWN"),
      });
    } finally {
      setBusyUser(null);
    }
  }

  async function endAffiliation(member: OrganizationMembership) {
    if (org === null) return;
    setBusyUser(member.user_id);
    setNotice(null);
    try {
      await orgClient.endAffiliation(org.id, member.user_id);
      setConfirmUser(null);
      if (member.user_id === user?.id) {
        // The reader just left; the member page is no longer theirs to read,
        // so return to the organizations they still belong to.
        router.push("/organizations");
        return;
      }
      await reloadMembers(org.id);
      setNotice({
        kind: "success",
        text: `Affiliation for @${identityOf(member.user_id).handle} ended. The membership remains in the record.`,
      });
    } catch (err: unknown) {
      setConfirmUser(null);
      setNotice({
        kind: "error",
        text: messageForOrgCode(err instanceof ApiError ? err.code : "UNKNOWN"),
      });
    } finally {
      setBusyUser(null);
    }
  }

  async function submitInvite(event: FormEvent) {
    event.preventDefault();
    if (org === null) return;
    const handle = inviteHandle.trim().replace(/^@/, "");
    if (handle === "") {
      setNotice({ kind: "error", text: "Enter the account handle to invite." });
      return;
    }
    setInviting(true);
    setNotice(null);
    try {
      const created = await orgClient.invite(org.id, {
        handle,
        role: inviteRole,
        verified: inviteVerified,
      });
      setInviteHandle("");
      // The assertion is per-invitation: a checked box left behind would
      // make the NEXT invite verified without a new statement.
      setInviteVerified(false);
      await reloadMembers(org.id);
      setNotice({
        kind: "success",
        text: `Invited @${identityOf(created.user_id).handle} as ${roleLabel(created.role)}.`,
      });
    } catch (err: unknown) {
      setNotice({
        kind: "error",
        text: messageForOrgCode(err instanceof ApiError ? err.code : "UNKNOWN"),
      });
    } finally {
      setInviting(false);
    }
  }

  if (loading) {
    return (
      <div className="org-members-state" role="status" aria-busy="true">
        <Spinner size="small" srText={null} />
        <span>Checking session…</span>
      </div>
    );
  }

  if (user === null) {
    return (
      <div className="org-members-state" data-org-signed-out>
        <p>
          <Link href={loginHref(`/organizations/${slug}/members`)}>Sign in</Link>{" "}
          to see this organization&apos;s members.
        </p>
        <p>
          <Link href={`/organizations/${encodeURIComponent(slug)}`}>
            View the public organization profile
          </Link>
        </p>
      </div>
    );
  }

  if (orgMissing) {
    return (
      <div className="org-members-state" data-org-not-member>
        <h1 className="org-members-title">Organization members</h1>
        <p>
          You are not a member of this organization, so its member list is not
          yours to read (it may also not exist).
        </p>
        <p>
          <Link href="/organizations">Back to organizations</Link>
        </p>
      </div>
    );
  }

  if (orgError !== null) {
    return (
      <div className="org-members-state">
        <div className="org-form-error" role="alert">
          {orgError}
        </div>
        <p>
          <Link href="/organizations">Back to organizations</Link>
        </p>
      </div>
    );
  }

  if (org === null || members === null) {
    return (
      <div className="org-members-state" role="status" aria-busy="true">
        <Spinner size="small" srText={null} />
        <span>Loading members…</span>
      </div>
    );
  }

  return (
    <div className="org-members" data-org-members-page>
      <p className="org-members-breadcrumb">
        <Link href="/organizations">Organizations</Link>
        {" / "}
        <Link href={`/organizations/${encodeURIComponent(org.slug)}`}>{org.name}</Link>
        {" / Members"}
      </p>
      <div className="org-members-heading">
        <h1 className="org-members-title">{org.name}</h1>
        <span className="org-members-slug">@{org.slug}</span>
        {myRole !== null ? (
          <span className="org-members-my-role" data-org-my-role={myRole}>
            Your role: {roleLabel(myRole)}
          </span>
        ) : null}
      </div>

      {isOwner ? (
        <p className="org-members-lede">
          You are an owner: invitations, roles and affiliations on this page
          are yours to manage.
        </p>
      ) : (
        <div className="org-manage-hint" role="note" data-org-manage-hint>
          Only organization owners can invite members or change roles. Your
          role is {myRole === null ? "not recorded" : roleLabel(myRole)}; an
          attempt to change something is refused by the server and explained
          here.
        </div>
      )}

      {notice !== null ? (
        <div
          className={notice.kind === "error" ? "org-form-error" : "org-form-success"}
          role={notice.kind === "error" ? "alert" : "status"}
          data-org-notice={notice.kind}
        >
          {notice.text}
        </div>
      ) : null}

      <section className="org-invite" aria-labelledby="org-invite-heading">
        <h2 className="org-invite-title" id="org-invite-heading">
          Invite a member
        </h2>
        <form className="org-invite-form" data-org-invite onSubmit={submitInvite}>
          <div className="org-field">
            <label className="org-label" htmlFor="org-invite-handle">
              Account handle
            </label>
            <input
              id="org-invite-handle"
              className="org-input"
              data-org-invite-handle
              value={inviteHandle}
              onChange={(event) => setInviteHandle(event.target.value)}
              placeholder="@handle"
              autoComplete="off"
            />
            <p className="org-help">
              The person must already have a POST account; invitations to
              addresses without an account are not offered here.
            </p>
          </div>
          <div className="org-field">
            <label className="org-label" htmlFor="org-invite-role">
              Role
            </label>
            <select
              id="org-invite-role"
              className="org-select"
              data-org-invite-role
              value={inviteRole}
              onChange={(event) => setInviteRole(event.target.value as OrgRole)}
            >
              {ORG_ROLES.map((role) => (
                <option key={role} value={role}>
                  {roleLabel(role)}
                </option>
              ))}
            </select>
          </div>
          <div className="org-field">
            <label className="org-checkbox">
              <input
                type="checkbox"
                data-org-invite-verified
                checked={inviteVerified}
                onChange={(event) => setInviteVerified(event.target.checked)}
              />{" "}
              I have personally verified this affiliation
            </label>
            <p className="org-help" data-org-invite-verified-help>
              Leave this unchecked unless you confirmed yourself that this
              person is affiliated with the organization. Unchecked records the
              affiliation as <strong>unverified</strong>, which is the honest
              default: inviting someone is not a verification. The flag is
              shown on the member&apos;s row.
            </p>
          </div>
          <div className="org-form-actions">
            <button
              type="submit"
              className="org-button org-button-primary"
              data-org-invite-submit
              disabled={inviting}
            >
              {inviting ? "Inviting…" : "Invite member"}
            </button>
          </div>
        </form>
      </section>

      <section className="org-member-list" aria-labelledby="org-member-list-heading">
        <h2 className="org-invite-title" id="org-member-list-heading">
          Members
        </h2>
        {membersError !== null ? (
          <div className="org-form-error" role="alert">
            {membersError}
          </div>
        ) : null}
        <table className="org-members-table" data-org-members>
          <caption className="sr-only">
            Members of {org.name}, current and former
          </caption>
          <thead>
            <tr>
              <th scope="col">Person</th>
              <th scope="col">Role</th>
              <th scope="col">Affiliated since</th>
              <th scope="col">Status</th>
              <th scope="col">Affiliation</th>
              <th scope="col">Actions</th>
            </tr>
          </thead>
          <tbody>
            {members.map((member) => {
              const identity = identityOf(member.user_id);
              const active = membershipActive(member);
              const self = member.user_id === user.id;
              return (
                <tr
                  key={member.user_id}
                  data-member-user={member.user_id}
                  data-member-handle={identity.handle}
                  data-member-role={member.role}
                  data-member-status={active ? "active" : "ended"}
                  data-member-self={self ? "true" : undefined}
                >
                  <td>
                    <span className="org-member-handle">@{identity.handle}</span>
                    {identity.displayName !== "" ? (
                      <span className="org-member-name">{identity.displayName}</span>
                    ) : null}
                  </td>
                  <td>
                    <span data-member-role-text>{roleLabel(member.role)}</span>
                  </td>
                  <td data-member-start={member.affiliation_start}>
                    {member.affiliation_start}
                  </td>
                  <td data-member-status-text>
                    {active ? "Active" : `Left ${member.affiliation_end}`}
                  </td>
                  <td
                    data-member-verified={member.verified ? "true" : "false"}
                    data-member-verified-text
                  >
                    {member.verified ? "Verified" : "Unverified"}
                  </td>
                  <td>
                    <div className="org-member-actions">
                    {active ? (
                      <>
                        <select
                          className="org-select"
                          aria-label={`Role for @${identity.handle}`}
                          data-role-select={identity.handle}
                          value={drafts[member.user_id] ?? member.role}
                          onChange={(event) =>
                            setDrafts((prev) => ({
                              ...prev,
                              [member.user_id]: event.target.value as OrgRole,
                            }))
                          }
                        >
                          {ORG_ROLES.map((role) => (
                            <option key={role} value={role}>
                              {roleLabel(role)}
                            </option>
                          ))}
                        </select>
                        <button
                          type="button"
                          className="org-button"
                          data-role-save={identity.handle}
                          disabled={busyUser === member.user_id}
                          onClick={() =>
                            void changeRole(member, drafts[member.user_id] ?? member.role)
                          }
                        >
                          Save role
                        </button>
                        {isOwner || self ? (
                          confirmUser === member.user_id ? (
                            <>
                              <button
                                type="button"
                                className="org-button org-button-danger"
                                data-affiliation-confirm={identity.handle}
                                disabled={busyUser === member.user_id}
                                onClick={() => void endAffiliation(member)}
                              >
                                {self ? "Confirm leave" : "Confirm end"}
                              </button>
                              <button
                                type="button"
                                className="org-button"
                                onClick={() => setConfirmUser(null)}
                              >
                                Cancel
                              </button>
                            </>
                          ) : (
                            <button
                              type="button"
                              className="org-button"
                              data-affiliation-start={identity.handle}
                              onClick={() => setConfirmUser(member.user_id)}
                            >
                              {self ? "Leave organization" : "End affiliation"}
                            </button>
                          )
                        ) : null}
                      </>
                    ) : null}
                    </div>
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
        <p className="org-help">
          Roles govern what a member may do. Ending an affiliation stops the
          role and keeps the record: past contributions still name this
          organization.
        </p>
      </section>
    </div>
  );
}
