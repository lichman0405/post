import { GlobeIcon, LockIcon, ShieldLockIcon } from "@primer/octicons-react";
import { StateLabel } from "@post/ui";

import type { Project } from "../../../lib/projects";
import { useT } from "../../i18n-provider";

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
 *
 * T1105: the four visible labels come from the catalog; `data-badge` does
 * NOT. docs/28 §3 ("领域对象 ID/enum 使用英文稳定 code，显示 label 本地化")
 * splits the two on purpose: the code is what a test and the API agree on,
 * the label is what a reader sees, and switching language may only move the
 * second one. The same sentence is why `StateLabel`'s `tone` is untouched —
 * tone is semantic, not copy.
 */
export function ProjectBadges({ project }: { project: Project }) {
  const t = useT();
  return (
    <span className="project-badges">
      {project.visibility === "public" ? (
        <StateLabel shape="badge" tone="success" icon={GlobeIcon} data-badge="public">
          {t("projects.badge.public")}
        </StateLabel>
      ) : (
        <StateLabel shape="badge" tone="attention" icon={LockIcon} data-badge="private">
          {t("projects.badge.private")}
        </StateLabel>
      )}
      {project.main_frozen ? (
        <StateLabel shape="badge" tone="attention" icon={ShieldLockIcon} data-badge="frozen">
          {t("projects.badge.frozenMain")}
        </StateLabel>
      ) : null}
      {project.provision_status === "pending" ? (
        <StateLabel shape="badge" tone="neutral" data-badge="pending">
          {t("projects.badge.repositoryPending")}
        </StateLabel>
      ) : null}
    </span>
  );
}
