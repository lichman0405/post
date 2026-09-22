import type { ComponentType, HTMLAttributes, ReactNode, SVGProps } from "react";

import { cx } from "./classes";
import "./ui.css";

/**
 * The one element POST renders a state word in (T1101).
 *
 * # What it replaced
 *
 * Five hand-written families said the same thing five ways:
 *
 *   .badge                    projects.css:94    Public / Private / Frozen main
 *   .conflicts-badge-*        conflicts.css:137  clean / N need a decision / recorded
 *   .conflicts-advisory-badge conflicts.css:236  "advisory only"
 *   .files-readonly-badge     projects.css:569   the Files page's read-only mark
 *   .files-kind-badge         projects.css:735   the previewed blob's kind
 *   .activity-row-source      activity.css       the activity stream's source/visibility
 *
 * What they agreed on is the point of the component: a span, a tone drawn
 * from docs/41's colour roles, an optional 12px Octicon, and every attribute
 * the caller passes landing on the span unchanged. What they DISAGREED
 * about is geometry — padding (0, 1px, 2px), weight (400, 500, 600), size
 * (11px, 12px), radius (2em, 999px) and whether the thing was a flex box or
 * a plain inline span.
 *
 * # Why the geometry is a prop instead of a decision
 *
 * The first attempt of this task "unified" that geometry — one shape for all
 * five families — and was rejected: AC3 requires the replacement to render
 * like what it replaced, and a 4px-wider, 2px-taller label is a visible
 * change on every page that draws one. The 11px/12px split is not a mistake
 * either family made; it is what those two pages look like. So each family
 * keeps its own geometry, named by `shape`, and ui.css reproduces the rule
 * it came from declaration for declaration. The shared part is the API, the
 * tone vocabulary and the class naming — not the pixels.
 *
 * `shape` therefore chooses a geometry, and `tone` chooses a colour family.
 * Not every pairing exists in POST: a shape's accepted tones are the ones
 * its family actually drew, which the prop type states rather than leaving
 * to a comment (a `readonly` label in danger red is not something this
 * component is allowed to invent).
 *
 * # Behaviour that is deliberately preserved
 *
 * The rendered element stays a `<span>`, and every attribute a caller
 * passes through — above all the `data-*` markers the e2e harnesses select
 * on (`data-files-readonly`, `data-conflicts-clean`, `data-badge`, …) —
 * lands on that span unchanged, because the props spread before
 * `className`. This component does NOT emit any `data-*` of its own: the
 * marker is the caller's, and `data-badge="public"` in the DOM is the
 * project header passing it, not StateLabel inventing it.
 */

/** An Octicons icon component (`@primer/octicons-react`). */
export type LabelIcon = ComponentType<SVGProps<SVGSVGElement> & { size?: number }>;

/** The families this component replaced, one geometry each. */
export type StateLabelShape = "badge" | "chip" | "advisory" | "readonly" | "kind" | "meta";

interface StateLabelBase extends Omit<HTMLAttributes<HTMLSpanElement>, "children"> {
  /** Octicon rendered at 12px before the text; hidden from assistive tech. */
  icon?: LabelIcon;
  children: ReactNode;
}

export type StateLabelProps = StateLabelBase &
  (
    | { shape: "badge"; tone: "neutral" | "success" | "attention" }
    | { shape: "chip"; tone: "success" | "attention" }
    | { shape: "advisory"; tone: "attention" }
    | { shape: "readonly"; tone: "success" }
    | { shape: "kind"; tone: "neutral" }
    | { shape: "meta"; tone: "neutral" | "accent" }
  );

export function StateLabel({ shape, tone, icon: Icon, className, children, ...rest }: StateLabelProps) {
  return (
    <span
      {...rest}
      className={cx(
        "post-state-label",
        `post-state-label-${shape}`,
        `post-state-label-${shape}-${tone}`,
        className,
      )}
    >
      {Icon ? <Icon size={12} aria-hidden="true" /> : null}
      {children}
    </span>
  );
}
