import type { Icon } from "@primer/octicons-react";
import {
  ArchiveIcon,
  BeakerIcon,
  BookIcon,
  FileDirectoryIcon,
  GearIcon,
  GitPullRequestIcon,
  IssueOpenedIcon,
  MilestoneIcon,
  PulseIcon,
  TagIcon,
} from "@primer/octicons-react";

import type { ProjectRole } from "../../../lib/projects";

/**
 * The project tab bar (docs/05 §3, specs/ui/routes.yaml project_tabs):
 * one list shared by the shell's rendering and the tests, so the routes
 * can never drift from the navigation. The Settings tab carries the
 * permission gate (owner/maintainer only, L1 — the matrix has no
 * settings action yet; T0109 adds the management endpoints that enforce
 * the same rule server-side).
 */
export interface ProjectTab {
  key: string;
  label: string;
  /** Route segment; "" is the overview (the project page itself). */
  path: string;
  icon: Icon;
  /** Hide the tab for roles below this one (Settings only). */
  minRole?: ProjectRole;
}

export const PROJECT_TABS: ProjectTab[] = [
  { key: "overview", label: "Overview", path: "", icon: BookIcon },
  { key: "research", label: "Research", path: "research", icon: BeakerIcon },
  { key: "issues", label: "Issues", path: "issues", icon: IssueOpenedIcon },
  { key: "pulls", label: "Pull requests", path: "pulls", icon: GitPullRequestIcon },
  { key: "releases", label: "Releases", path: "releases", icon: TagIcon },
  { key: "milestones", label: "Milestones", path: "milestones", icon: MilestoneIcon },
  { key: "assets", label: "Assets", path: "assets", icon: ArchiveIcon },
  { key: "files", label: "Files", path: "files", icon: FileDirectoryIcon },
  { key: "activity", label: "Activity", path: "activity", icon: PulseIcon },
  {
    key: "settings",
    label: "Settings",
    path: "settings",
    icon: GearIcon,
    minRole: "maintainer",
  },
];

/** The href of one tab for one project. */
export function projectTabHref(projectId: string, path: string): string {
  return path === "" ? `/projects/${projectId}` : `/projects/${projectId}/${path}`;
}

/** Role ranks, mirroring internal/domain ProjectRole.Rank. */
const ROLE_RANK: Record<ProjectRole, number> = {
  viewer: 0,
  contributor: 1,
  maintainer: 2,
  owner: 3,
};

/** The tabs a role may see (Settings is hidden below its minRole). */
export function tabsForRole(role: ProjectRole | null): ProjectTab[] {
  return PROJECT_TABS.filter((tab) => {
    if (tab.minRole === undefined) return true;
    return role !== null && ROLE_RANK[role] >= ROLE_RANK[tab.minRole];
  });
}
