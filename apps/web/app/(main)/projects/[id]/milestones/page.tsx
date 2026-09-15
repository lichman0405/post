"use client";

import { useEffect, useMemo, useRef, useState } from "react";
import Link from "next/link";
import { AlertIcon, CheckIcon, ClockIcon, MilestoneIcon } from "@primer/octicons-react";
import { Spinner } from "@primer/react";

import { createAuthClient } from "../../../../../lib/auth";
import {
  ApiError,
  createMilestonesClient,
  mayCreateMilestone,
  messageForMilestoneCode,
  MILESTONE_KINDS,
  milestoneDisplayName,
  type Milestone,
} from "../../../../../lib/milestones";
import { useProjectShell } from "../shell-context";

/**
 * Milestones tab (T0609): the project's research timeline — dated facts
 * (candidate selected, paper submitted, patent filed, external
 * validation, custom labels) rendered in research order, which is the
 * API's occurred_at ascending order. A milestone may name a release (the
 * link is optional) but it is NEVER a project lifecycle state: the list
 * carries no completed state and no milestone of any kind forces one.
 *
 * Owners and maintainers get the record form (the API is the
 * authorization boundary; the gate below is UX). The create carries an
 * Idempotency-Key that survives retries: the key is generated once per
 * fresh form and kept until the create succeeds, so a double submit or a
 * retry after a network failure replays the first create instead of
 * duplicating a timeline entry (docs/22). On success the page re-fetches
 * the list — the timeline order is the API's contract and the recorded
 * date may fall anywhere on it, so local insertion would have to
 * re-derive that order.
 *
 * A recorded milestone is a fact on the timeline: this page offers no
 * edit and no delete — the API registers no such routes, so nothing to
 * call them with exists.
 */

type Notice = { kind: "success" | "error"; text: string } | null;

export default function MilestonesPage() {
  const shell = useProjectShell();

  const [milestones, setMilestones] = useState<Milestone[] | null>(null);
  const [listError, setListError] = useState<string | null>(null);
  const [kind, setKind] = useState<string>("candidate_selected");
  const [label, setLabel] = useState("");
  const [occurredAt, setOccurredAt] = useState("");
  const [releaseId, setReleaseId] = useState("");
  const [recording, setRecording] = useState(false);
  const [notice, setNotice] = useState<Notice>(null);
  // One idempotency key per create attempt, kept across retries so the
  // retry replays; reset after a successful create (the next attempt is
  // a new record and needs a fresh key).
  const idempotencyKey = useRef<string | null>(null);

  // One auth client for the CSRF token (lib/auth's sessionTokenStorage),
  // and the milestones client for the list + create.
  const authClient = useMemo(
    () => (shell === null ? null : createAuthClient(shell.apiBaseUrl)),
    [shell],
  );
  const milestonesClient = useMemo(
    () =>
      shell === null
        ? null
        : createMilestonesClient(shell.apiBaseUrl, {
            csrfToken: () => authClient?.csrfToken() ?? null,
          }),
    [shell, authClient],
  );

  useEffect(() => {
    if (shell === null || !mayCreateMilestone(shell.role)) return;
    // The CSRF token lives in sessionStorage; the session() refresh covers
    // a reloaded tab whose storage is empty but whose cookie is valid.
    authClient?.session().catch(() => null);
  }, [shell, authClient]);

  // loadTimeline refetches the timeline — the API's research order is
  // the render order, both on mount and after a successful record.
  function loadTimeline() {
    if (shell === null || milestonesClient === null) return;
    milestonesClient
      .list(shell.project.id)
      .then((list) => setMilestones(list))
      .catch((err: unknown) => {
        setListError(
          err instanceof ApiError
            ? messageForMilestoneCode(err.code)
            : "Could not load milestones.",
        );
      });
  }

  useEffect(() => {
    if (shell === null || milestonesClient === null) return;
    let cancelled = false;
    milestonesClient
      .list(shell.project.id)
      .then((list) => {
        if (!cancelled) setMilestones(list);
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        setListError(
          err instanceof ApiError
            ? messageForMilestoneCode(err.code)
            : "Could not load milestones.",
        );
      });
    return () => {
      cancelled = true;
    };
  }, [shell, milestonesClient]);

  if (shell === null) {
    // The shell only mounts tab content in its ready state; null means a
    // wiring error, not a user-visible page.
    return null;
  }
  const { project, role } = shell;
  const canRecord = mayCreateMilestone(role);
  const labelRequired = kind === "custom";
  const formComplete = occurredAt.trim() !== "" && (!labelRequired || label.trim() !== "");

  async function submitCreate(event: React.FormEvent) {
    event.preventDefault();
    if (milestonesClient === null || recording) return;
    const key = idempotencyKey.current ?? crypto.randomUUID();
    idempotencyKey.current = key;
    setRecording(true);
    setNotice(null);
    try {
      await milestonesClient.create(project.id, {
        kind,
        label: label.trim() === "" ? undefined : label,
        occurredAt: occurredAt.trim(),
        releaseId: releaseId.trim() === "" ? undefined : releaseId.trim(),
        idempotencyKey: key,
      });
      idempotencyKey.current = null;
      setKind("candidate_selected");
      setLabel("");
      setOccurredAt("");
      setReleaseId("");
      setNotice({ kind: "success", text: "Milestone recorded." });
      loadTimeline();
    } catch (err: unknown) {
      // Keep the key: the next submit replays this exact create.
      setNotice({
        kind: "error",
        text:
          err instanceof ApiError
            ? messageForMilestoneCode(err.code)
            : "Could not record the milestone. Please try again.",
      });
    } finally {
      setRecording(false);
    }
  }

  return (
    <div className="milestones-page" data-project-tab-content="milestones">
      <section className="milestones-section">
        <h2 className="milestones-section-title">
          <MilestoneIcon size={16} aria-hidden="true" /> Milestones
        </h2>
        <p className="milestones-section-desc">
          The research timeline: dated facts about the project — a
          candidate selected, a paper submitted, a patent filed, an
          external validation, or a custom marker. Milestones document
          what happened when; they are not lifecycle states and the
          project has no completed state that a milestone could force.
        </p>

        {listError !== null ? (
          <div className="milestones-error" data-milestone-error>
            <AlertIcon size={16} aria-hidden="true" /> {listError}
          </div>
        ) : null}

        {milestones === null && listError === null ? (
          <div className="milestones-loading">
            <Spinner aria-label="Loading milestones" />
          </div>
        ) : null}

        {milestones !== null && milestones.length === 0 ? (
          <div className="milestones-empty" data-milestone-empty>
            No milestones yet. Owners and maintainers can record the first
            one below.
          </div>
        ) : null}

        {milestones !== null && milestones.length > 0 ? (
          <div className="milestones-list" data-milestone-list>
            {milestones.map((milestone) => (
              <div className="milestones-row" key={milestone.id} data-milestone-row={milestone.id}>
                <div className="milestones-row-icon" aria-hidden="true">
                  <ClockIcon size={14} />
                </div>
                <div className="milestones-row-main">
                  <span className="milestone-name">
                    {milestoneDisplayName(milestone.kind, milestone.label)}
                  </span>
                  {milestone.kind === "custom" && milestone.label !== null && milestone.label !== "" ? (
                    <span className="milestone-kind">Custom</span>
                  ) : null}
                  <span className="milestone-date">
                    {milestone.occurred_at.slice(0, 10)}
                  </span>
                  {milestone.release_id !== null ? (
                    <Link
                      className="milestone-release"
                      href={`/projects/${project.id}/releases/${milestone.release_id}`}
                      data-milestone-release={milestone.id}
                    >
                      Release
                    </Link>
                  ) : null}
                </div>
              </div>
            ))}
          </div>
        ) : null}
      </section>

      {canRecord ? (
        <section className="milestones-section">
          <h2 className="milestones-section-title">Record a milestone</h2>
          <p className="milestones-section-desc">
            A milestone documents a dated fact on the timeline. The custom
            kind takes the label you give it; the canonical kinds render
            their own names. Linking a release is optional — the release
            must exist in this project.
          </p>
          <form className="milestones-create-form" onSubmit={submitCreate}>
            <label className="milestones-label" htmlFor="milestone-kind">
              Kind
            </label>
            <select
              id="milestone-kind"
              className="milestones-input"
              data-milestone-kind-input
              value={kind}
              onChange={(event) => setKind(event.target.value)}
              disabled={recording}
            >
              {MILESTONE_KINDS.map((entry) => (
                <option key={entry.kind} value={entry.kind}>
                  {entry.name}
                </option>
              ))}
            </select>
            <label className="milestones-label" htmlFor="milestone-label">
              Label{" "}
              <span className="milestones-label-note">
                ({labelRequired ? "required for custom" : "optional"})
              </span>
            </label>
            <input
              id="milestone-label"
              className="milestones-input"
              data-milestone-label-input
              value={label}
              onChange={(event) => setLabel(event.target.value)}
              placeholder={labelRequired ? "the marker's name" : "e.g. JACS 2026"}
              disabled={recording}
            />
            <label className="milestones-label" htmlFor="milestone-occurred-at">
              Occurred at{" "}
              <span className="milestones-label-note">(RFC 3339, the timeline position)</span>
            </label>
            <input
              id="milestone-occurred-at"
              className="milestones-input"
              data-milestone-date-input
              value={occurredAt}
              onChange={(event) => setOccurredAt(event.target.value)}
              placeholder="e.g. 2026-09-14T10:00:00Z"
              disabled={recording}
            />
            <label className="milestones-label" htmlFor="milestone-release">
              Release <span className="milestones-label-note">(optional)</span>
            </label>
            <input
              id="milestone-release"
              className="milestones-input"
              data-milestone-release-input
              value={releaseId}
              onChange={(event) => setReleaseId(event.target.value)}
              placeholder="the release id, when this milestone names one"
              disabled={recording}
            />
            <div className="milestones-create-row">
              <button
                type="submit"
                className="milestones-create"
                data-milestone-create
                disabled={recording || !formComplete}
              >
                {recording ? "Recording…" : "Record milestone"}
              </button>
              <span className="milestones-create-note">
                Milestones are facts on the timeline — there is no edit or
                delete.
              </span>
            </div>
          </form>
          {notice !== null ? (
            <div
              className={`milestones-notice milestones-notice-${notice.kind}`}
              data-milestone-notice={notice.kind}
            >
              {notice.kind === "success" ? (
                <CheckIcon size={16} aria-hidden="true" />
              ) : (
                <AlertIcon size={16} aria-hidden="true" />
              )}
              {notice.text}
            </div>
          ) : null}
        </section>
      ) : (
        <p className="milestones-readonly-note" data-milestones-unauthorized>
          Only owners and maintainers can record milestones.
        </p>
      )}

      {milestones !== null && milestones.length > 0 ? (
        <p className="milestones-order-note" data-milestone-order-note>
          The timeline renders in research order — by the milestones&apos;
          occurred-at dates, oldest first, with creation order breaking
          date ties.
        </p>
      ) : null}
    </div>
  );
}
