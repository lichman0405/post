import { GlobeIcon, LockIcon, ShieldLockIcon } from "@primer/octicons-react";

import type { Project } from "../../../lib/projects";

/**
 * The project governance badges (T0108): visibility (Public/Private) and
 * the frozen-main state. These render the API's truth — the same two
 * fields the permissions engine and every downstream product decision
 * read; the badges never hold state of their own.
 */
export function ProjectBadges({ project }: { project: Project }) {
  return (
    <span className="project-badges">
      {project.visibility === "public" ? (
        <span className="badge badge-public">
          <GlobeIcon size={12} aria-hidden="true" /> Public
        </span>
      ) : (
        <span className="badge badge-private">
          <LockIcon size={12} aria-hidden="true" /> Private
        </span>
      )}
      {project.main_frozen ? (
        <span className="badge badge-frozen">
          <ShieldLockIcon size={12} aria-hidden="true" /> Frozen main
        </span>
      ) : null}
      {project.provision_status === "pending" ? (
        <span className="badge badge-muted">repository pending</span>
      ) : null}
    </span>
  );
}
