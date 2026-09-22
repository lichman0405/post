import { GlobeIcon, LockIcon, ShieldLockIcon } from "@primer/octicons-react";
import { StateLabel } from "@post/ui";

import type { Project } from "../../../lib/projects";

/**
 * The project governance badges (T0108): visibility (Public/Private) and
 * the frozen-main state. These render the API's truth — the same two
 * fields the permissions engine and every downstream product decision
 * read; the badges never hold state of their own.
 *
 * T1101: the four labels are `StateLabel` now. They used to be
 * `.badge` + one of `.badge-public/private/frozen/muted` in projects.css,
 * which is where the same shape was spelled for the conflicts page, the
 * Files page and the search answer as well. The page keeps only the thing
 * that is its own — `.project-badges`, the flex row they sit in — and the
 * tone says which governance state this is: visibility and frozen main are
 * `attention` (they constrain what the platform will do), a public project
 * is `success`, and a repository that is still being provisioned is
 * `neutral` because it is a fact about progress, not a state of health.
 *
 * Each one carries `data-badge` so the shell e2e can name the governance
 * state it is asserting on rather than the CSS class that happened to draw
 * it — the classes below are the shared component's now, and the marker
 * outlives them.
 */
export function ProjectBadges({ project }: { project: Project }) {
  return (
    <span className="project-badges">
      {project.visibility === "public" ? (
        <StateLabel shape="badge" tone="success" icon={GlobeIcon} data-badge="public">
          Public
        </StateLabel>
      ) : (
        <StateLabel shape="badge" tone="attention" icon={LockIcon} data-badge="private">
          Private
        </StateLabel>
      )}
      {project.main_frozen ? (
        <StateLabel shape="badge" tone="attention" icon={ShieldLockIcon} data-badge="frozen">
          Frozen main
        </StateLabel>
      ) : null}
      {project.provision_status === "pending" ? (
        <StateLabel shape="badge" tone="neutral" data-badge="pending">
          repository pending
        </StateLabel>
      ) : null}
    </span>
  );
}
