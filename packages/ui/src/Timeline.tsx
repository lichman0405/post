import type { HTMLAttributes, ReactNode } from "react";

import { WithData } from "./attrs";
import { cx } from "./classes";
import type { Tone } from "./tokens";
import "./ui.css";

/**
 * The one activity stream POST renders (T1101).
 *
 * # What it replaced
 *
 * The project's Activity page grew its own `.activity-row*` vocabulary in
 * activity.css: an `<ol>` of hairlines, a 24px marker square per row whose
 * colour states the family, a head line of title + source pills + a
 * `<time>`, a meta line of actor/via/target, and a `<dl>` of the facts the
 * row records. The pull detail page needed the same stream for a head's
 * review history and had no shared shape to reuse, so it built its own
 * list beside it.
 *
 * The structure now lives here once. The page supplies CONTENT (which
 * octicon, which words, which links) and the `tone` that states the
 * family; the shared stylesheet owns the geometry.
 *
 * # Why the content is slots and not a schema
 *
 * An activity row is not a fixed record — it carries a link to an actor
 * that may be unknown, a correlation id that may be absent, and a family
 * that decides which icon and which facts appear. A component that
 * modelled those fields would be a second definition of the audit event,
 * which is what the page's own comment (activity.css header) warns
 * against. Slots keep the page the only place that decides what an entry
 * means, while the *shape* stops being re-invented.
 *
 * `attrs` keeps the `data-activity-*` markers on the row itself, so the
 * activity e2e harness keeps selecting exactly what it selected before.
 */

/** One row of the stream. */
export interface TimelineEntry {
  /** Stable identity. */
  key: string;
  /** The marker's icon; omitted renders an empty marker square. */
  marker?: ReactNode;
  /** Colour family of the marker: what kind of event this was. */
  tone?: Tone;
  /** The row's headline. */
  title: ReactNode;
  /** Pills between the headline and the time (registry, visibility, …). */
  labels?: ReactNode;
  /** Right-aligned timestamp. */
  time?: { dateTime: string; text: ReactNode; title?: string };
  /** The actor / via / target line. */
  meta?: ReactNode;
  /** The facts the row records, as label/value pairs. */
  details?: Array<{
    label: ReactNode;
    value: ReactNode;
    attrs?: WithData<HTMLAttributes<HTMLDivElement>>;
  }>;
  /** Attributes for the `<li>` (e.g. `data-activity-row`). */
  attrs?: WithData<HTMLAttributes<HTMLLIElement>>;
}

export interface TimelineProps extends HTMLAttributes<HTMLOListElement> {
  entries: TimelineEntry[];
}

export function Timeline({ entries, className, ...rest }: TimelineProps) {
  return (
    <ol {...rest} className={cx("post-timeline", className)}>
      {entries.map((entry) => {
        const { className: rowClassName, ...rowAttrs } = entry.attrs ?? {};
        return (
          <li
            key={entry.key}
            {...rowAttrs}
            className={cx(
              "post-timeline-row",
              entry.tone && `post-timeline-row-${entry.tone}`,
              rowClassName,
            )}
          >
            <div className="post-timeline-marker" aria-hidden="true">
              {entry.marker}
            </div>
            <div className="post-timeline-body">
              <div className="post-timeline-head">
                <span className="post-timeline-title">{entry.title}</span>
                {entry.labels !== undefined ? (
                  <span className="post-timeline-labels">{entry.labels}</span>
                ) : null}
                {entry.time !== undefined ? (
                  <time
                    className="post-timeline-time"
                    dateTime={entry.time.dateTime}
                    title={entry.time.title}
                  >
                    {entry.time.text}
                  </time>
                ) : null}
              </div>
              {entry.meta !== undefined ? (
                <div className="post-timeline-meta">{entry.meta}</div>
              ) : null}
              {entry.details !== undefined && entry.details.length > 0 ? (
                <dl className="post-timeline-details">
                  {entry.details.map((detail) => (
                    <div className="post-timeline-detail" key={String(detail.label)} {...detail.attrs}>
                      <dt>{detail.label}</dt>
                      <dd>{detail.value}</dd>
                    </div>
                  ))}
                </dl>
              ) : null}
            </div>
          </li>
        );
      })}
    </ol>
  );
}
