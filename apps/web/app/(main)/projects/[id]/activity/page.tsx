"use client";

import { Suspense, useCallback, useEffect, useMemo, useRef, useState } from "react";
import Link from "next/link";
import { usePathname, useSearchParams } from "next/navigation";
import {
  AlertIcon,
  HistoryIcon,
  IssueReopenedIcon,
  TagIcon,
  XCircleIcon,
} from "@primer/octicons-react";
import { Spinner } from "@primer/react";

import {
  ACTIVITY_SOURCES,
  ApiError,
  activityActorHref,
  activityActorName,
  activityDetail,
  activityFamily,
  activityFilterLabel,
  activitySourceFromParam,
  activityTarget,
  activityTimestamp,
  activityTitle,
  activityVisibility,
  createActivityClient,
  messageForAuditCode,
  viaLabel,
  type ActivitySource,
  type AuditEntry,
} from "../../../../../lib/activity";
import { eventTypeDisplayName } from "../../../../../lib/inbox";
import { StateLabel, Timeline } from "@post/ui";
import type { TimelineEntry, Tone } from "@post/ui";
import { useProjectShell } from "../shell-context";
import "./activity.css";

/**
 * Activity tab (T0607): the project's history as ONE timeline — the
 * governance record (audit_log: who changed what, through which channel,
 * under which correlation id) and the research events (the state
 * transitions the platform emitted), newest first.
 *
 * The two are different records (docs/26 §1 makes the application log, the
 * domain audit log and the research events three separate record types) and
 * the page never merges them into one: every row says which registry it
 * came from, the filter selects between them, and the rendering of a row
 * follows its registry — a governance row shows the state it moved
 * (before → after) and the words its record carries, a research row shows
 * the event body it was emitted with.
 *
 * The FILTER LIVES IN THE URL (?source=governance|research), so a filtered
 * timeline is a link that can be shared and reloaded, and the page's state
 * has one source of truth. An unknown value is refused rather than
 * silently read as "everything": the API refuses a filter it does not
 * have, and a page that dropped one would answer a request nobody made.
 *
 * Paging is the API's keyset cursor, one window at a time ("Load more"),
 * so a page of a feed that mixes the two registries is never a
 * concatenation of two — the ordering and the seams belong to the read
 * path, not to the browser.
 */

/** One window of the API's default page size. The read path clamps
 *  anything larger, so this only says how much one "Load more" adds. */
const PAGE_SIZE = 25;

/**
 * What the page is asking for, and for which filter. The filter is part of
 * every answer: when the URL names another one, an answer to the previous
 * question is not an answer to this one, so it is not rendered — that is
 * how the loading state is derived, instead of being written by an effect
 * (which would cascade a render on every filter change).
 */
type View =
  | { kind: "loading" }
  | { kind: "loaded"; source: ActivitySource | null; entries: AuditEntry[]; nextCursor: string | null }
  | { kind: "error"; source: ActivitySource | null; message: string };

export default function ActivityPage() {
  // useSearchParams requires a Suspense boundary during prerender; the
  // inner component owns the filter read (lib/conflicts' pattern).
  return (
    <Suspense
      fallback={
        <div className="activity-state" data-activity-loading>
          <Spinner aria-label="Loading activity" />
        </div>
      }
    >
      <ActivityPageInner />
    </Suspense>
  );
}

function ActivityPageInner() {
  const shell = useProjectShell();
  const pathname = usePathname();
  const searchParams = useSearchParams();

  const rawSource = searchParams.get("source");
  // The URL is user-editable, so the value is validated before anything is
  // requested with it. A refused value is a page state of its own below —
  // never an API call, which would come back VALIDATION_FAILED and read as
  // a problem with the project rather than with the link.
  const filter: ActivitySource | null | "invalid" = useMemo(() => {
    try {
      return activitySourceFromParam(rawSource);
    } catch {
      return "invalid";
    }
  }, [rawSource]);

  const client = useMemo(
    () => (shell === null ? null : createActivityClient(shell.apiBaseUrl)),
    [shell],
  );
  const projectId = shell?.project.id ?? null;

  const [view, setView] = useState<View>({ kind: "loading" });
  const [loadingMore, setLoadingMore] = useState(false);
  // Retry re-runs the read without changing the filter (an event handler's
  // counter, so the effect below stays a pure subscription).
  const [reloadNonce, setReloadNonce] = useState(0);
  // Every read carries the sequence number of the request that started it,
  // so an answer that arrives after the filter changed (or after a newer
  // page was appended) is dropped instead of being rendered under the wrong
  // heading.
  const requestSeq = useRef(0);

  useEffect(() => {
    if (filter === "invalid" || client === null || projectId === null) return;
    const seq = ++requestSeq.current;
    client
      .projectActivity(projectId, {
        source: filter ?? undefined,
        limit: PAGE_SIZE,
      })
      .then((page) => {
        if (requestSeq.current !== seq) return;
        setView({ kind: "loaded", source: filter, entries: page.entries, nextCursor: page.next_cursor });
      })
      .catch((err: unknown) => {
        if (requestSeq.current !== seq) return;
        setView({
          kind: "error",
          source: filter,
          message:
            err instanceof ApiError
              ? messageForAuditCode(err.code)
              : "Could not load the activity. Please try again.",
        });
      });
  }, [client, projectId, filter, reloadNonce]);

  // loadMore appends the next window. The cursor comes from the API and is
  // handed back untouched — a cursor this page composed would be a second
  // implementation of the ordering the read path owns.
  const loadMore = useCallback(() => {
    if (client === null || projectId === null) return;
    if (view.kind !== "loaded" || view.nextCursor === null || loadingMore) return;
    const cursor = view.nextCursor;
    const seq = requestSeq.current; // an append belongs to the current list
    setLoadingMore(true);
    client
      .projectActivity(projectId, { source: view.source ?? undefined, cursor, limit: PAGE_SIZE })
      .then((page) => {
        if (requestSeq.current !== seq) return;
        setView((prev) =>
          prev.kind === "loaded"
            ? { ...prev, entries: [...prev.entries, ...page.entries], nextCursor: page.next_cursor }
            : prev,
        );
      })
      .catch((err: unknown) => {
        if (requestSeq.current !== seq) return;
        setView({
          kind: "error",
          source: view.source,
          message:
            err instanceof ApiError
              ? messageForAuditCode(err.code)
              : "Could not load more activity. Please try again.",
        });
      })
      .finally(() => {
        if (requestSeq.current === seq) setLoadingMore(false);
      });
  }, [client, projectId, view, loadingMore]);

  // filterHref is where a chip points: the filter lives in the URL, so each
  // chip is a real link — shareable, reloadable, and walkable with the back
  // button — and the page holds no navigation state of its own.
  const filterHref = useCallback(
    (source: ActivitySource | null) => {
      if (source === null) return pathname;
      const params = new URLSearchParams(searchParams.toString());
      params.set("source", source);
      return `${pathname}?${params.toString()}`;
    },
    [pathname, searchParams],
  );

  if (shell === null) {
    // The shell only mounts tab content in its ready state; null means a
    // wiring error, not a user-visible page.
    return null;
  }
  const { project } = shell;

  // The chips, and the states they switch between.
  const chips = (
    <div className="activity-filters" role="group" aria-label="Activity source" data-activity-filters>
      {[null, ...ACTIVITY_SOURCES].map((source) => {
        const selected = source === filter;
        return (
          <Link
            key={source ?? "all"}
            className={selected ? "activity-chip activity-chip-selected" : "activity-chip"}
            aria-current={selected ? "true" : undefined}
            href={filterHref(source)}
            data-activity-filter={source ?? "all"}
            data-activity-filter-selected={selected ? "true" : "false"}
          >
            {activityFilterLabel(source)}
          </Link>
        );
      })}
    </div>
  );

  if (filter === "invalid") {
    return (
      <div className="activity-page" data-project-tab-content="activity">
        <section className="activity-section">
          <h2 className="activity-section-title">
            <HistoryIcon size={16} aria-hidden="true" /> Activity
          </h2>
          <div className="activity-error" data-activity-error data-activity-filter-refused={rawSource ?? ""}>
            <AlertIcon size={16} aria-hidden="true" />
            <span>
              This timeline has no filter called <code>{rawSource}</code>. It reads the
              governance record and the research events together, or one of them.
            </span>
            <Link className="activity-retry" href={pathname} data-activity-filter-all>
              Show all activity
            </Link>
          </div>
        </section>
      </div>
    );
  }

  // The answer being rendered is the answer to the filter in the URL; an
  // answer to another question is shown as "loading", not as this filter's
  // history.
  const shown: View = view.kind !== "loading" && view.source !== filter ? { kind: "loading" } : view;
  const entries = shown.kind === "loaded" ? shown.entries : null;

  return (
    <div className="activity-page" data-project-tab-content="activity">
      <section className="activity-section">
        <h2 className="activity-section-title">
          <HistoryIcon size={16} aria-hidden="true" /> Activity
        </h2>
        <p className="activity-section-desc">
          What happened in this project, newest first: the governance record
          (who changed what, through which channel, under which correlation
          id) and the research events the platform emitted. Aborts, reopenings
          and releases show the state they moved and the reason they record.
        </p>

        {chips}

        {shown.kind === "error" ? (
          <div className="activity-error" data-activity-error>
            <AlertIcon size={16} aria-hidden="true" /> {shown.message}
            <button
              type="button"
              className="activity-retry"
              data-activity-retry
              onClick={() => setReloadNonce((n) => n + 1)}
            >
              Retry
            </button>
          </div>
        ) : null}

        {shown.kind === "loading" ? (
          <div className="activity-state" data-activity-loading>
            <Spinner aria-label="Loading activity" />
          </div>
        ) : null}

        {entries !== null && entries.length === 0 ? (
          <div className="activity-state" data-activity-empty>
            Nothing here yet.{" "}
            {filter === null
              ? "The project's governance record and its research events both begin with its first change."
              : `No ${activityFilterLabel(filter).toLowerCase()} in this project yet.`}
          </div>
        ) : null}

        {entries !== null && entries.length > 0 ? (
          <Timeline
            data-activity-list
            entries={entries.map((entry) => activityEntry(project.id, entry))}
          />
        ) : null}

        {shown.kind === "loaded" && shown.nextCursor !== null ? (
          <div className="activity-more">
            <button
              type="button"
              className="activity-load-more"
              data-activity-load-more
              disabled={loadingMore}
              onClick={loadMore}
            >
              {loadingMore ? "Loading…" : "Load more"}
            </button>
          </div>
        ) : null}
      </section>
    </div>
  );
}

/** The marker of one row, by the family it belongs to: a retraction, a
 *  reopening, a release, or the plain history mark. */
function familyIcon(entry: AuditEntry) {
  switch (activityFamily(entry)) {
    case "abort":
      return <XCircleIcon size={14} />;
    case "reopen":
      return <IssueReopenedIcon size={14} />;
    case "release":
      return <TagIcon size={14} />;
    default:
      return <HistoryIcon size={14} />;
  }
}

/** The marker colour family of a row: what kind of event this was
 *  (docs/46). The shared Timeline paints the marker from this, instead of
 *  each page spelling `.activity-row-<family> .activity-row-marker`. */
function familyTone(entry: AuditEntry): Tone {
  switch (activityFamily(entry)) {
    case "abort":
      return "danger";
    case "reopen":
      return "success";
    case "release":
      return "accent";
    default:
      return "neutral";
  }
}

/**
 * One timeline row, as a shared `Timeline` entry. Everything it renders
 * comes from one entry through the helpers in lib/activity.ts
 * (unit-tested), so the page holds no rendering rule of its own — and no
 * row is rendered from a field its registry does not have.
 *
 * The row is built here rather than in JSX because `Timeline` owns the
 * shape (marker, head, time, meta, facts) and the page owns the content,
 * including every `data-activity-*` marker the activity e2e selects on.
 */
function activityEntry(projectId: string, entry: AuditEntry): TimelineEntry {
  const actorHref = activityActorHref(entry);
  const target = activityTarget(projectId, entry);
  const details = activityDetail(entry);
  const visibility = activityVisibility(entry);
  const family = activityFamily(entry);

  return {
    key: entry.id,
    marker: familyIcon(entry),
    tone: familyTone(entry),
    title: <span data-activity-title>{activityTitle(entry, eventTypeDisplayName)}</span>,
    labels: (
      <>
        <StateLabel
          shape="meta"
          tone={entry.source === "research" ? "accent" : "neutral"}
          data-activity-source={entry.source}
        >
          {activityFilterLabel(entry.source)}
        </StateLabel>
        {visibility !== null ? (
          <StateLabel shape="meta" tone="neutral" data-activity-visibility={visibility}>
            {visibility}
          </StateLabel>
        ) : null}
      </>
    ),
    time: {
      dateTime: entry.occurred_at,
      text: activityTimestamp(entry.occurred_at),
      title: entry.occurred_at,
    },
    meta: (
      <>
        {actorHref !== null ? (
          <Link className="activity-actor" href={actorHref} data-activity-actor={entry.actor_id}>
            {activityActorName(entry)}
          </Link>
        ) : (
          <span className="activity-actor activity-actor-unknown" data-activity-actor="">
            {activityActorName(entry)}
          </span>
        )}
        <span className="activity-via" data-activity-via={entry.via}>
          {viaLabel(entry.via)}
        </span>
        {target !== null ? (
          target.href !== null ? (
            <Link className="activity-target" href={target.href} data-activity-target={entry.target_ref}>
              {target.label}
            </Link>
          ) : (
            // No page shows this target (an object, an organization, the
            // project itself): the ref renders as text. A link to a route
            // that does not exist would be a promise the app cannot keep.
            <span className="activity-target" data-activity-target={entry.target_ref}>
              {target.label}
            </span>
          )
        ) : null}
        <span className="activity-correlation" data-activity-correlation title="Correlation id">
          {entry.correlation_id}
        </span>
      </>
    ),
    details: details.map((detail) => ({
      label: detail.label,
      value: detail.value,
      attrs: { "data-activity-detail": detail.label },
    })),
    attrs: {
      "data-activity-row": entry.id,
      "data-activity-row-source": entry.source,
      "data-activity-row-family": family,
    },
  };
}
