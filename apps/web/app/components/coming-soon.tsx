import Link from "next/link";
import { RocketIcon } from "@primer/octicons-react";

/**
 * Honest placeholder for global-nav destinations that land in later
 * milestones (T0107 ships the navigation and layout, not the pages behind
 * every destination). Each stub names the task that replaces it, so the
 * gap is discoverable instead of a dead end. Primer-token styling lives in
 * globals.css (.coming-soon*).
 */
export function ComingSoon({
  title,
  milestone,
  children,
}: {
  title: string;
  milestone: string;
  children: React.ReactNode;
}) {
  return (
    <div className="coming-soon-main">
      <div className="coming-soon">
        <div className="coming-soon-icon" aria-hidden="true">
          <RocketIcon size={24} />
        </div>
        <h1 className="coming-soon-title">{title}</h1>
        <p className="coming-soon-desc">
          {children} This area lands with <strong>{milestone}</strong>.
        </p>
        <Link className="coming-soon-button" href="/">
          Back to Home
        </Link>
      </div>
    </div>
  );
}
