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
  /** T1105: a catalog key, not a rendered label — the tab bar is on every
      project page, so its nine labels are copy and belong in the catalog
      (docs/28 §3). Same shape as nav-destinations.ts: the table resolves no
      locale, the renderer does. */
  labelKey: string;
  /** Route segment; "" is the overview (the project page itself). */
  path: string;
  icon: Icon;
  /** Hide the tab for roles below this one (Settings only). */
  minRole?: ProjectRole;
}

export const PROJECT_TABS: ProjectTab[] = [
  { key: "overview", labelKey: "project.tab.overview", path: "", icon: BookIcon },
  { key: "research", labelKey: "project.tab.research", path: "research", icon: BeakerIcon },
  { key: "issues", labelKey: "project.tab.issues", path: "issues", icon: IssueOpenedIcon },
  { key: "pulls", labelKey: "project.tab.pulls", path: "pulls", icon: GitPullRequestIcon },
  { key: "releases", labelKey: "project.tab.releases", path: "releases", icon: TagIcon },
  { key: "milestones", labelKey: "project.tab.milestones", path: "milestones", icon: MilestoneIcon },
  { key: "assets", labelKey: "project.tab.assets", path: "assets", icon: ArchiveIcon },
  { key: "files", labelKey: "project.tab.files", path: "files", icon: FileDirectoryIcon },
  { key: "activity", labelKey: "project.tab.activity", path: "activity", icon: PulseIcon },
  {
    key: "settings",
    labelKey: "project.tab.settings",
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
