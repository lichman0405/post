"use client";

import { useEffect, useMemo, useRef, useState } from "react";
import { AlertIcon, CheckIcon, InfoIcon } from "@primer/octicons-react";
import { Spinner } from "@primer/react";

import { createAuthClient } from "../../../../../lib/auth";
import {
  ApiError,
  createProjectsClient,
  mayManageSettings,
  messageForProjectCode,
  type ProjectMember,
  type ProjectRole,
} from "../../../../../lib/projects";
import { Table } from "@post/ui";
import { useProjectShell } from "../shell-context";

/**
 * Settings tab (T0109): member management, the visibility preview and the
 * project purpose/activity-status edit. The API is the authorization
 * boundary (server-side role gate + CSRF on every write); the UI gating
 * below is UX — the tab-level gate renders the not-authorized answer
 * (data-settings-unauthorized) for a direct URL hit below maintainer.
 *
 * Every accepted owner/maintainer write lands an audit placeholder in the
 * same transaction on the API side; the audit note tells the actor so.
 * Visibility is preview-only: the API refuses a change with
 * VISIBILITY_CHANGE_NOT_SUPPORTED (the publishing guard is a later
 * milestone), so the control here is disabled and the client never sends
 * it.
 */

const ROLE_OPTIONS: ProjectRole[] = ["owner", "maintainer", "contributor", "viewer"];

const STATUS_OPTIONS = ["planning", "active", "paused", "archived"] as const;

type Notice = { kind: "success" | "error"; text: string } | null;

export default function SettingsPage() {
  const shell = useProjectShell();

  const [members, setMembers] = useState<ProjectMember[] | null>(null);
  const [membersError, setMembersError] = useState<string | null>(null);
  const [busyMember, setBusyMember] = useState<string | null>(null);
  const [baseline, setBaseline] = useState<{ purpose: string; activityStatus: string } | null>(null);
  const [purpose, setPurpose] = useState("");
  const [activityStatus, setActivityStatus] = useState("planning");
  const [saving, setSaving] = useState(false);
  const [notice, setNotice] = useState<Notice>(null);

  // One auth client for the CSRF token (lib/auth's sessionTokenStorage,
  // refreshed from the API when the tab reloaded without one), and the
  // projects client for the settings reads + writes.
  const authClient = useMemo(
    () => (shell === null ? null : createAuthClient(shell.apiBaseUrl)),
    [shell],
  );
  const projectsClient = useMemo(
    () =>
      shell === null
        ? null
        : createProjectsClient(shell.apiBaseUrl, {
            csrfToken: () => authClient?.csrfToken() ?? null,
          }),
    [shell, authClient],
  );

  // Seed the edit form once per project (a re-render after a save must
  // not clobber what the user is typing).
  const seededFor = useRef<string | null>(null);
  useEffect(() => {
    if (shell === null || seededFor.current === shell.project.id) return;
    seededFor.current = shell.project.id;
    setBaseline({ purpose: shell.project.purpose, activityStatus: shell.project.activity_status });
    setPurpose(shell.project.purpose);
    setActivityStatus(shell.project.activity_status);
  }, [shell]);

  useEffect(() => {
    if (shell === null || !mayManageSettings(shell.role)) return;
    // The CSRF token lives in sessionStorage; the session() refresh covers
    // a reloaded tab whose storage is empty but whose cookie is valid.
    authClient?.session().catch(() => null);
  }, [shell, authClient]);

  useEffect(() => {
    if (shell === null || projectsClient === null || !mayManageSettings(shell.role)) return;
    let cancelled = false;
    projectsClient
      .listMembers(shell.project.id)
      .then((list) => {
        if (!cancelled) setMembers(list);
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        setMembersError(
          err instanceof ApiError ? messageForProjectCode(err.code) : "Could not load members.",
        );
      });
    return () => {
      cancelled = true;
    };
  }, [shell, projectsClient]);

  if (shell === null) {
    // The shell only mounts tab content in its ready state; null means a
    // wiring error, not a user-visible page.
    return null;
  }
  if (!mayManageSettings(shell.role)) {
    return (
      <div className="settings-unauthorized" data-settings-unauthorized>
        You do not have permission to manage this project&apos;s settings.
      </div>
    );
  }
  // Narrowed once for the closures below (function declarations do not
  // inherit the guard's narrowing).
  const { project, role, userId, applyProject } = shell;

  async function changeRole(member: ProjectMember, newRole: ProjectRole) {
    if (projectsClient === null || newRole === member.role) return;
    setBusyMember(member.user_id);
    setNotice(null);
    try {
      const updated = await projectsClient.setMemberRole(project.id, member.user_id, newRole);
      setMembers((prev) =>
        prev === null
          ? prev
          : prev.map((m) => (m.user_id === member.user_id ? { ...m, role: updated.role } : m)),
      );
      setNotice({
        kind: "success",
        text: `Role for ${member.display_name || member.handle} updated to ${newRole}.`,
      });
    } catch (err) {
      setNotice({
        kind: "error",
        text: err instanceof ApiError
          ? messageForProjectCode(err.code)
          : "Could not change the role.",
      });
    } finally {
      setBusyMember(null);
    }
  }

  async function saveSettings() {
    if (projectsClient === null || baseline === null) return;
    const input: { purpose?: string; activity_status?: string } = {};
    if (purpose !== baseline.purpose) input.purpose = purpose;
    if (activityStatus !== baseline.activityStatus) input.activity_status = activityStatus;
    if (Object.keys(input).length === 0) return;
    setSaving(true);
    setNotice(null);
    try {
      const updated = await projectsClient.updateSettings(project.id, input);
      setBaseline({ purpose: updated.purpose, activityStatus: updated.activity_status });
      setPurpose(updated.purpose);
      setActivityStatus(updated.activity_status);
      applyProject(updated);
      setNotice({ kind: "success", text: "Settings saved." });
    } catch (err) {
      setNotice({
        kind: "error",
        text: err instanceof ApiError
          ? messageForProjectCode(err.code)
          : "Could not save the settings.",
      });
    } finally {
      setSaving(false);
    }
  }

  const dirty = baseline !== null &&
    (purpose !== baseline.purpose || activityStatus !== baseline.activityStatus);

  // A maintainer governs non-owner roles; the owner role stays owner-only
  // (the API enforces both — the option list mirrors it).
  const roleOptions = role === "owner" ? ROLE_OPTIONS : ROLE_OPTIONS.filter((r) => r !== "owner");

  return (
    <div className="settings-page" data-settings>
      <section className="settings-section" data-members-section>
        <h2 className="settings-section-title">Members</h2>
        <p className="settings-section-desc">
          Members of {project.name}, oldest membership first. Role changes take
          effect immediately and are recorded in the audit log.
        </p>
        {membersError !== null ? (
          <div className="settings-members-error" data-members-error>{membersError}</div>
        ) : members === null ? (
          <Spinner aria-label="Loading members" />
        ) : (
          /* T1101: the members list was the second byte-identical copy of
             the pulls-table stylesheet; both are the shared Table now, and
             this page keeps only what is its own — which columns, and the
             role control inside one of them. */
          <Table
            className="settings-members"
            columns={[
              {
                key: "member",
                header: "Member",
                render: (m) => (
                  <>
                    <span className="settings-member-name">{m.display_name || m.handle}</span>
                    <span className="settings-member-handle">@{m.handle}</span>
                  </>
                ),
                cellAttrs: () => ({ className: "settings-member-identity" }),
              },
              {
                key: "role",
                header: "Role",
                render: (m) => {
                  const isSelf = userId !== null && m.user_id === userId;
                  const ownerLocked = role === "maintainer" && m.role === "owner";
                  return isSelf || ownerLocked ? (
                    <span className="settings-role-static" data-role-static={m.role}>
                      {m.role}
                      {isSelf ? <span className="settings-role-note"> (you)</span> : null}
                    </span>
                  ) : (
                    <select
                      className="settings-role-select"
                      data-role-select
                      aria-label={`Role for ${m.display_name || m.handle}`}
                      value={m.role}
                      disabled={busyMember === m.user_id}
                      onChange={(e) => changeRole(m, e.target.value as ProjectRole)}
                    >
                      {roleOptions.map((r) => (
                        <option key={r} value={r}>{r}</option>
                      ))}
                    </select>
                  );
                },
                cellAttrs: () => ({ className: "settings-member-role" }),
              },
              {
                key: "joined",
                header: "Joined",
                render: (m) => m.joined_at.slice(0, 10),
                cellAttrs: () => ({ className: "settings-member-joined" }),
              },
            ]}
            rows={members}
            rowKey={(m) => m.user_id}
            rowAttrs={(m) => ({ "data-member-row": m.user_id })}
          />
        )}
      </section>

      <section className="settings-section" data-visibility-section>
        <h2 className="settings-section-title">Visibility</h2>
        <select
          className="settings-visibility-select"
          data-visibility-preview
          aria-label="Project visibility"
          value={project.visibility}
          disabled
        >
          <option value="public">public</option>
          <option value="private">private</option>
        </select>
        <p className="settings-section-note" data-visibility-note>
          <InfoIcon size={14} aria-hidden="true" />
          Visibility is preview-only here — changing it needs the publishing guard
          (a later milestone), so the API refuses a change for now.
        </p>
      </section>

      <section className="settings-section" data-details-section>
        <h2 className="settings-section-title">Project details</h2>
        <label className="settings-label" htmlFor="settings-purpose">Purpose</label>
        <textarea
          id="settings-purpose"
          className="settings-input settings-purpose-input"
          data-purpose-input
          rows={4}
          maxLength={4000}
          value={purpose}
          onChange={(e) => setPurpose(e.target.value)}
        />
        <label className="settings-label" htmlFor="settings-activity">Activity status</label>
        <select
          id="settings-activity"
          className="settings-input settings-activity-select"
          data-activity-select
          value={activityStatus}
          onChange={(e) => setActivityStatus(e.target.value)}
        >
          {STATUS_OPTIONS.map((s) => (
            <option key={s} value={s}>{s}</option>
          ))}
        </select>
        <div className="settings-save-row">
          <button
            type="button"
            className="settings-save"
            data-save-settings
            disabled={!dirty || saving}
            onClick={saveSettings}
          >
            {saving ? "Saving…" : "Save settings"}
          </button>
        </div>
      </section>

      {notice !== null ? (
        // T1104: "Settings saved." / "Could not save settings." is the whole
        // report of the PATCH behind the Save button — the button only stops
        // saying "Saving…". Role follows the kind: a saved setting is a
        // polite status, a rejected write is an alert. (Only this notice
        // changed; the visibility control itself is T1103's and is untouched.)
        <div
          className={
            notice.kind === "success" ? "settings-notice settings-notice-success" : "settings-notice settings-notice-error"
          }
          data-settings-notice={notice.kind}
          role={notice.kind === "success" ? "status" : "alert"}
        >
          {notice.kind === "success" ? (
            <CheckIcon size={14} aria-hidden="true" />
          ) : (
            <AlertIcon size={14} aria-hidden="true" />
          )}
          {notice.text}
        </div>
      ) : null}

      <p className="settings-audit-note" data-audit-note>
        <InfoIcon size={14} aria-hidden="true" />
        Role and settings changes by owners and maintainers are recorded in the
        project audit log.
      </p>
    </div>
  );
}
