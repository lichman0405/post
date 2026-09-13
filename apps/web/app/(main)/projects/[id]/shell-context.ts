import { createContext, useContext } from "react";

import type { Project, ProjectRole } from "../../../../lib/projects";

/**
 * The shell's single fetch, shared with the tab content: the shell
 * layout reads project + membership once and provides both to the pages
 * it renders, so the Overview and the Settings gate never re-fetch.
 * A consumer rendering outside a ready shell (the shell's not-found /
 * error states never mount children) reads null and renders nothing —
 * that is a programming error, not a user-visible state.
 */
export interface ShellData {
  project: Project;
  role: ProjectRole | null;
  /** The signed-in actor's user id (null when not a member). */
  userId: string | null;
  /** The API origin the shell fetched from; tab pages reuse it for their writes. */
  apiBaseUrl: string;
  /** Replace the shell's project copy after a settings write, so the header reflects it. */
  applyProject: (project: Project) => void;
}

export const ProjectShellContext = createContext<ShellData | null>(null);

/** The shell data of the enclosing project shell, or null. */
export function useProjectShell(): ShellData | null {
  return useContext(ProjectShellContext);
}
