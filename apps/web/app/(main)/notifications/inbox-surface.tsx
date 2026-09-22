"use client";

import { useCallback, useEffect, useMemo, useState } from "react";
import Link from "next/link";
import { AlertIcon, BellIcon, CheckIcon, InboxIcon, MailIcon } from "@primer/octicons-react";
import { Spinner } from "@primer/react";

import { createAuthClient } from "../../../lib/auth";
import {
  ApiError,
  INBOX_FILTER_ALL,
  INBOX_FILTER_UNREAD,
  aggregationLabel,
  createInboxClient,
  entryTargetLabel,
  eventTypeDisplayName,
  messageForInboxCode,
  windowLabel,
  type InboxEntry,
  type InboxPage,
} from "../../../lib/inbox";
import "./inbox.css";

/**
 * The research inbox (T1003, docs/18 §3-4).
 *
 * Each row is an ENTRY, not a notification: the API groups the deliveries
 * of one target and one event type inside one clock hour into a single row
 * carrying a count. That is the answer to "不要每个低级 commit 都通知用户"
 * — forty commits on a followed project read as one line saying "40
 * events", not forty lines — and nothing is hidden by it: the count is on
 * the row, the links go to the same places, and the read view shows what
 * has not been read yet.
 *
 * Read state is the API's. "Mark read" names the entry's newest delivery
 * (latest_delivery_id — the row the browser was shown), so a notification
 * that arrived after this page rendered stays unread; after marking, the
 * page re-reads the list rather than decrementing a counter locally,
 * because the badge and the entries have to agree with the same source of
 * truth (a locally recomputed badge drifts on the next load).
 *
 * A signed-out browser gets the API's 401 and is shown a sign-in link, not
 * an empty inbox: "nothing happened" and "we could not ask" must not look
 * alike (docs/51).
 */

type Status =
  | { kind: "loading" }
  | { kind: "ready" }
  | { kind: "signed-out" }
  | { kind: "error"; text: string };

export function InboxSurface({ apiBaseUrl }: { apiBaseUrl: string }) {
  const [filter, setFilter] = useState<string>(INBOX_FILTER_UNREAD);
  const [page, setPage] = useState<InboxPage | null>(null);
  const [status, setStatus] = useState<Status>({ kind: "loading" });
  const [notice, setNotice] = useState<string | null>(null);
  const [marking, setMarking] = useState(false);

  const authClient = useMemo(() => createAuthClient(apiBaseUrl), [apiBaseUrl]);
  const inboxClient = useMemo(
    () => createInboxClient(apiBaseUrl, { csrfToken: () => authClient.csrfToken() }),
    [apiBaseUrl, authClient],
  );

  // load starts one read of one view. It sets state only from the
  // promise's callbacks: the effect below must not set state synchronously
  // in its body (react-hooks/set-state-in-effect), and "a view is
  // loading" is derived in the render from `shown` instead.
  const load = useCallback(
    (view: string) => {
      let cancelled = false;
      inboxClient
        .list(view)
        .then((next) => {
          if (cancelled) return;
          setPage(next);
          setStatus({ kind: "ready" });
        })
        .catch((err: unknown) => {
          if (cancelled) return;
          if (err instanceof ApiError && err.code === "AUTH_UNAUTHENTICATED") {
            setStatus({ kind: "signed-out" });
            return;
          }
          setStatus({
            kind: "error",
            text: messageForInboxCode(err instanceof ApiError ? err.code : "UNKNOWN"),
          });
        });
      return () => {
        cancelled = true;
      };
    },
    [inboxClient],
  );

  useEffect(() => load(filter), [load, filter]);

  // The CSRF token lives in sessionStorage; a reloaded tab whose storage is
  // empty but whose cookie is valid gets it back from the session refresh.
  useEffect(() => {
    authClient.session().catch(() => null);
  }, [authClient]);

  // One mark flow for both buttons: mark, re-read, say what happened.
  // `marking` guards re-entry, so a second click while a mark is in flight
  // is the same mark rather than a second one.
  async function runMark(marked: () => Promise<number>, nothingLeft: string) {
    if (marking) return;
    setMarking(true);
    setNotice(null);
    try {
      const n = await marked();
      // Re-read: the badge, the entries and the read state come from one
      // place, and the page must not hold a second opinion about them.
      load(filter);
      setNotice(n === 0 ? nothingLeft : `Marked ${n} notification${n === 1 ? "" : "s"} read.`);
    } catch (err: unknown) {
      setNotice(messageForInboxCode(err instanceof ApiError ? err.code : "UNKNOWN"));
    } finally {
      setMarking(false);
    }
  }

  /** Mark one entry read, named by the newest delivery the row showed. */
  function markEntryRead(deliveryID: string) {
    return runMark(
      () => inboxClient.markRead([deliveryID]),
      "Nothing left to mark — this entry was already read.",
    );
  }

  /**
   * Mark the whole inbox read. This is the API's own read-all
   * (POST /api/v1/inbox/read-all), not a page of anchors sent to the
   * single-entry route: the toolbar button says "all", and an inbox
   * holding more entries than one page would leave the rest unread (the
   * badge still counting them) while claiming otherwise.
   */
  function markEverythingRead() {
    return runMark(
      () => inboxClient.markAllRead(),
      "Nothing left to mark — your inbox is already read.",
    );
  }

  // The page on screen is the one served for the SELECTED view: a view
  // switch leaves the previous page in state until the new one arrives, so
  // without this a stale list would wear the new tab and a mark would name
  // deliveries the caller is no longer looking at. page.filter is the view
  // the API answered for, which is what makes the two comparable.
  const shown = page !== null && page.filter === filter ? page : null;
  const unreadCount = page?.unreadCount ?? 0;
  const loadingView = status.kind === "loading" || (status.kind === "ready" && shown === null);

  return (
    <div className="inbox-page" data-inbox-surface>
      <header className="inbox-head">
        <h1 className="inbox-title">
          <BellIcon size={20} aria-hidden="true" /> Notifications
        </h1>
        <p className="inbox-sub">
          What happened on the projects, assets and people you follow.
          Notifications of the same kind about the same subject within one
          hour arrive as one row; the count says how many were grouped.
        </p>
      </header>

      <div className="inbox-toolbar">
        <div className="inbox-filters" role="tablist" aria-label="Inbox view">
          <button
            type="button"
            role="tab"
            aria-selected={filter === INBOX_FILTER_UNREAD}
            className="inbox-filter"
            data-inbox-filter="unread"
            onClick={() => setFilter(INBOX_FILTER_UNREAD)}
          >
            Unread
            {filter === INBOX_FILTER_UNREAD && unreadCount > 0 ? (
              <span className="inbox-filter-count">{unreadCount}</span>
            ) : null}
          </button>
          <button
            type="button"
            role="tab"
            aria-selected={filter === INBOX_FILTER_ALL}
            className="inbox-filter"
            data-inbox-filter="all"
            onClick={() => setFilter(INBOX_FILTER_ALL)}
          >
            All
          </button>
        </div>
        <button
          type="button"
          className="inbox-mark-all"
          data-inbox-mark-all
          disabled={marking || status.kind !== "ready" || unreadCount === 0}
          onClick={markEverythingRead}
        >
          Mark all read
        </button>
      </div>

      {/* T1104: "Mark read" / "Mark all read" are writes, and this line is
          where BOTH outcomes land — the success sentence from runMark, or the
          mapped API error from its catch. Polite (role="status"): the reader
          asked for the mark, nothing is broken, and the unread badge
          vanishing is a visual-only confirmation otherwise. */}
      {notice !== null ? (
        <div className="inbox-notice" data-inbox-notice role="status">
          {notice}
        </div>
      ) : null}

      {loadingView ? (
        <div className="inbox-state" data-inbox-loading>
          <Spinner aria-label="Loading notifications" />
        </div>
      ) : null}

      {status.kind === "signed-out" ? (
        <div className="inbox-state" data-inbox-signed-out>
          <MailIcon size={16} aria-hidden="true" />
          <span>
            <Link href="/login">Sign in</Link> to read your notifications.
          </span>
        </div>
      ) : null}

      {status.kind === "error" ? (
        <div className="inbox-state inbox-state-error" data-inbox-error>
          <AlertIcon size={16} aria-hidden="true" />
          <span>{status.text}</span>
          <button type="button" className="inbox-retry" onClick={() => load(filter)}>
            Try again
          </button>
        </div>
      ) : null}

      {shown !== null && shown.entries.length === 0 ? (
        <div className="inbox-state" data-inbox-empty>
          <InboxIcon size={16} aria-hidden="true" />
          <span>
            {filter === INBOX_FILTER_UNREAD
              ? "Nothing unread. Everything you follow is quiet."
              : "No notifications yet. Follow a project, an asset or a person and what happens there lands here."}
          </span>
        </div>
      ) : null}

      {shown !== null && shown.entries.length > 0 ? (
        <ul className="inbox-list" data-inbox-list>
          {shown.entries.map((entry) => (
            <InboxRow
              key={`${entry.target_type}/${entry.target_id}/${entry.event_type}/${entry.window_start}`}
              entry={entry}
              busy={marking}
              onMarkRead={() => markEntryRead(entry.latest_delivery_id)}
            />
          ))}
        </ul>
      ) : null}
    </div>
  );
}

/** One aggregated entry: what happened, where, when, and what is unread. */
function InboxRow({
  entry,
  busy,
  onMarkRead,
}: {
  entry: InboxEntry;
  busy: boolean;
  onMarkRead: () => void;
}) {
  const badge = aggregationLabel(entry.count);
  const label = entryTargetLabel(entry);
  const window = windowLabel(entry.window_start);

  return (
    <li className="inbox-row" data-inbox-row={`${entry.target_type}/${entry.event_type}`}>
      <span
        className={entry.read ? "inbox-dot inbox-dot-read" : "inbox-dot"}
        aria-hidden="true"
        data-inbox-unread={entry.unread > 0 ? "true" : "false"}
      />
      <div className="inbox-row-main">
        <div className="inbox-row-head">
          <span className="inbox-event" data-inbox-event={entry.event_type}>
            {eventTypeDisplayName(entry.event_type)}
          </span>
          {badge !== null ? <span className="inbox-count" data-inbox-count={entry.count}>{badge}</span> : null}
          <span className="inbox-target-sep" aria-hidden="true">
            ·
          </span>
          {entry.url === "" ? (
            // A target with no page yet (knowledge, organizations): the row
            // shows the identity instead of a link that would 404.
            <span className="inbox-target" data-inbox-target-nolink>
              {label}
            </span>
          ) : (
            <Link className="inbox-target" href={entry.url} data-inbox-target-link={entry.url}>
              {label}
            </Link>
          )}
        </div>
        <div className="inbox-row-meta">
          {window !== "" ? <span className="inbox-window">{window}</span> : null}
          {entry.count > 1 ? (
            <span className="inbox-readstate" data-inbox-unread-count={entry.unread}>
              {entry.read ? "all read" : `${entry.unread} of ${entry.count} unread`}
            </span>
          ) : entry.read ? (
            <span className="inbox-readstate">read</span>
          ) : (
            <span className="inbox-readstate">unread</span>
          )}
        </div>
      </div>
      {entry.read ? (
        <span className="inbox-row-action inbox-row-action-done" data-inbox-read>
          <CheckIcon size={14} aria-hidden="true" /> Read
        </span>
      ) : (
        <button
          type="button"
          className="inbox-row-action"
          data-inbox-mark-read={entry.latest_delivery_id}
          disabled={busy}
          onClick={onMarkRead}
        >
          Mark read
        </button>
      )}
    </li>
  );
}
