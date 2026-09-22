import type { HTMLAttributes, ReactNode } from "react";

import { WithData } from "./attrs";
import { cx } from "./classes";
import type { Tone } from "./tokens";
import "./ui.css";

/**
 * The one difference view POST renders (T1101).
 *
 * # Two layouts, because the app asks two questions
 *
 *   entries  "what changed?" — a vertical list of changes, each with a
 *            kind chip, a type, a title and the fields that moved. This is
 *            the pull detail's research-state diff, written twice there
 *            (objects and relations) with the same `<ul className=
 *            "pull-changes">` shell and the same `<li className=
 *            "pull-change">` body.
 *
 *   sides    "what does each side have?" — the conflicts page's evidence
 *            context: the same question answered from base and from
 *            source, side by side, each column with its own heading and
 *            its own list (conflicts.css's `.conflicts-evidence-columns`).
 *
 * Both are difference views of one shape — a set of items with a label and
 * a body — so they share the entry type and the chip geometry, and a page
 * picks the layout it is asking about.
 *
 * # The chip
 *
 * `.post-diff-kind` is StateLabel's geometry (1px 8px, pill, 12px/18px) in
 * its own class, because a diff chip is not a standalone label: it colours
 * by the CHANGE kind (created / aborted / moved), and the callers already
 * pass those words. Keeping the geometry in one stylesheet is the point;
 * folding it into StateLabel's element would have meant a second way to
 * spell a label tone.
 */

/** One changed item. */
export interface DiffEntry {
  key: string;
  /** The kind chip's text ("created", "aborted", …). */
  kind?: ReactNode;
  /** Colour family for the kind chip. */
  tone?: Tone;
  /** The changed thing's type (usually a monospaced noun). */
  type?: ReactNode;
  /** The changed thing's headline. */
  title: ReactNode;
  /** A short trailing note on the row (e.g. "target branch moved this too"). */
  note?: ReactNode;
  /** Attributes for the note element (e.g. `data-change-moved`). */
  noteAttrs?: WithData<HTMLAttributes<HTMLSpanElement>>;
  /** The identifier / lifecycle line under the headline. */
  meta?: ReactNode;
  /** The changed fields, when the diff carries them. */
  fields?: ReactNode;
  /** Attributes for the `<li>` (e.g. `data-change-kind`). */
  attrs?: WithData<HTMLAttributes<HTMLLIElement>>;
}

/** One side of a side-by-side comparison. */
export interface DiffSide {
  key: string;
  heading: ReactNode;
  entries: DiffEntry[];
  /** Attributes for the side's panel (e.g. `data-evidence-side`). */
  attrs?: WithData<HTMLAttributes<HTMLDivElement>>;
  /** Rendered instead of the list when the side has no entries. */
  empty?: ReactNode;
}

/* Exactly one of the two layouts: `sides?: never` makes "both" and
   "neither" type errors rather than a runtime surprise. */
export type DiffProps = { className?: string } & (
  | { entries: DiffEntry[]; sides?: never }
  | { sides: DiffSide[]; entries?: never }
);

export function Diff(props: DiffProps) {
  if (props.sides !== undefined) {
    return <DiffSides sides={props.sides} className={props.className} />;
  }
  return <DiffEntries entries={props.entries} className={props.className} />;
}

function DiffEntries({ entries, className }: { entries: DiffEntry[]; className?: string }) {
  return (
    <ul className={cx("post-diff", className)}>
      {entries.map((entry) => {
        const { className: rowClassName, ...rowAttrs } = entry.attrs ?? {};
        return (
          <li key={entry.key} {...rowAttrs} className={cx("post-diff-entry", rowClassName)}>
            <div className="post-diff-head">
              {entry.kind !== undefined ? (
                <span
                  className={cx(
                    "post-diff-kind",
                    entry.tone && entry.tone !== "neutral" && `post-diff-kind-${entry.tone}`,
                  )}
                >
                  {entry.kind}
                </span>
              ) : null}
              {entry.type !== undefined ? <span className="post-diff-type">{entry.type}</span> : null}
              <span className="post-diff-title">{entry.title}</span>
              {entry.note !== undefined ? (
                <span className="post-diff-note" {...entry.noteAttrs}>
                  {entry.note}
                </span>
              ) : null}
            </div>
            {entry.meta !== undefined || entry.fields !== undefined ? (
              <div className="post-diff-meta">
                {entry.meta}
                {entry.fields !== undefined ? (
                  <span className="post-diff-fields">{entry.fields}</span>
                ) : null}
              </div>
            ) : null}
          </li>
        );
      })}
    </ul>
  );
}

function DiffSides({ sides, className }: { sides: DiffSide[]; className?: string }) {
  return (
    <div className={cx("post-diff-sides", className)}>
      {sides.map((side) => {
        const { className: sideClassName, ...sideAttrs } = side.attrs ?? {};
        return (
          <div key={side.key} {...sideAttrs} className={cx("post-diff-side", sideClassName)}>
            <p className="post-diff-side-heading">{side.heading}</p>
            {side.entries.length === 0 ? (
              <p className="post-diff-side-empty">{side.empty}</p>
            ) : (
              <ul className="post-diff-side-list">
                {side.entries.map((entry) => {
                  const { className: rowClassName, ...rowAttrs } = entry.attrs ?? {};
                  return (
                    <li
                      key={entry.key}
                      {...rowAttrs}
                      className={cx("post-diff-side-entry", rowClassName)}
                    >
                      <p className="post-diff-side-title">{entry.title}</p>
                      {entry.meta !== undefined ? (
                        <p className="post-diff-side-meta">{entry.meta}</p>
                      ) : null}
                    </li>
                  );
                })}
              </ul>
            )}
          </div>
        );
      })}
    </div>
  );
}
