"use client";

import { useProjectShell } from "./shell-context";

/**
 * Overview tab (T0108): the research summary's first landing — the
 * project's purpose and its governance/state facts straight from the
 * shell's single fetch. Research questions, findings and branch state
 * replace this seed as the RSG milestones land (docs/06 §1).
 */
export default function ProjectOverviewPage() {
  const shell = useProjectShell();
  if (shell === null) {
    // The shell only mounts tab content in its ready state; null means a
    // wiring error, not a user-visible page.
    return null;
  }
  const { project, role } = shell;
  return (
    <div className="project-overview" data-project-tab-content="overview">
      {project.purpose !== "" ? (
        <p className="project-overview-purpose">{project.purpose}</p>
      ) : null}
      <dl>
        <dt>Activity status</dt>
        <dd>{project.activity_status}</dd>
        <dt>Visibility</dt>
        <dd>{project.visibility}</dd>
        <dt>Main branch</dt>
        <dd>{project.main_frozen ? "frozen" : "unfrozen"}</dd>
        <dt>Repository</dt>
        <dd>{project.provision_status}</dd>
        <dt>Your role</dt>
        <dd>{role ?? "not a member"}</dd>
        <dt>Created</dt>
        <dd>{project.created_at.slice(0, 10)}</dd>
      </dl>
    </div>
  );
}
