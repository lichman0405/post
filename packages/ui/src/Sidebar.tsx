import type { HTMLAttributes, ReactNode } from "react";

import { cx } from "./classes";
import "./ui.css";

/**
 * The one rail POST renders (T1101): where the reader is, and what else
 * belongs beside the main column.
 *
 * # What it replaced
 *
 * Four breadcrumbs, four structures, four stylesheets:
 *
 *   .project-shell-breadcrumb   projects.css:138  <p>Link / slug</p>
 *   .files-breadcrumb           projects.css:601  <div> of <button> crumbs
 *   .release-detail-breadcrumb  projects.css:1713 <p>Link / version</p>
 *   .asset-breadcrumb           assets.css:174    <nav> of one link
 *
 * They agreed on the words and disagreed on everything else — element,
 * separator, font size (12px vs 13px), and whether a crumb was a link or a
 * control.
 *
 * # A slot for the crumb, not an href
 *
 * The shared layer must not depend on `next/link`: `@post/ui` is plain
 * React, and swapping a `Link` for an `<a>` would turn a client-side
 * navigation into a full page load — a behaviour change dressed as a
 * refactor. So a crumb's `content` is whatever the page already renders
 * (`Link`, `<button>`, or plain text) and this component owns the
 * geometry: the row, the separators, the type scale, and the hit target of
 * a crumb that is a control.
 *
 * # What this component does NOT cover yet
 *
 * docs/06 §3 asks detail pages for "main column + right metadata sidebar",
 * and two pages have that column: `aside.asset-column-side` and
 * `aside.files-context`. They are NOT this component's — their blocks
 * (`.asset-block`, `.files-context-group`) carry their own margins, title
 * scale and separators, and adopting a shared block shape here would move
 * pixels on two baseline pages for a purely structural gain. The rail is
 * the trail; the columns stay the pages'.
 */

/** One step of the trail. */
export interface SidebarCrumb {
  /** Stable identity; the index is used when omitted. */
  key?: string;
  /** The rendered crumb — a `Link`, a `<button className="post-sidebar-action">`, or text. */
  content: ReactNode;
}

export interface SidebarProps extends HTMLAttributes<HTMLElement> {
  /** Accessible name of the rail (`aria-label`). */
  label: string;
  /** The trail, first crumb first. Separators are inserted between crumbs. */
  crumbs?: SidebarCrumb[];
  /**
   * How the crumbs are laid out. `inline` (the default) is a line of text —
   * the shape of a `<p>` breadcrumb, where the separators are the spaces
   * around a "/" and the line box comes from the surrounding font. `row` is
   * a flex row of chips 2px apart, for a trail whose crumbs are controls
   * with a hit target (the files path).
   */
  trail?: "inline" | "row";
  /** `muted` (default) or the accent blue of a light breadcrumb. */
  tone?: "muted" | "accent";
  children?: ReactNode;
}

export function Sidebar({
  label,
  crumbs,
  trail = "inline",
  tone = "muted",
  className,
  children,
  ...rest
}: SidebarProps) {
  return (
    <nav
      {...rest}
      className={cx("post-sidebar", tone === "accent" && "post-sidebar-tone-accent", className)}
      aria-label={label}
    >
      {crumbs !== undefined && crumbs.length > 0 ? (
        <ol
          className={cx(
            "post-sidebar-trail",
            trail === "row" && "post-sidebar-trail-row",
          )}
        >
          {crumbs.map((crumb, index) => (
            <li className="post-sidebar-crumb" key={crumb.key ?? index}>
              {index > 0 ? (
                /* The separator is " / ", spaces included, because the space
                   is the spacing: an inline trail collapses it against the
                   crumb either side, and a row trail strips it and lets the
                   row's 2px gap do the same job. */
                <span className="post-sidebar-sep" aria-hidden="true">
                  {" / "}
                </span>
              ) : null}
              {crumb.content}
            </li>
          ))}
        </ol>
      ) : null}
      {children}
    </nav>
  );
}
