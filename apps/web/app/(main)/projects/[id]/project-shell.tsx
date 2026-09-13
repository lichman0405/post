"use client";

import { useEffect, useMemo, useState } from "react";
import Link from "next/link";
import { usePathname } from "next/navigation";
import { AlertIcon, RepoIcon } from "@primer/octicons-react";
import { Spinner } from "@primer/react";

import {
  ApiError,
  createProjectsClient,
  messageForProjectCode,
  type Project,
  type ProjectMembership,
} from "../../../../lib/projects";
import { ProjectBadges } from "../project-badges";
import { projectTabHref, tabsForRole } from "../project-tabs";
import { ProjectShellContext } from "./shell-context";
import "../projects.css";

type ShellFailure = {
  projectId: string;
  kind: "not-found" | "error";
  message: string;
};

type ShellLoaded = {
  projectId: string;
  project: Project;
  membership: ProjectMembership | null;
};

/**
 * The project shell (client component): one fetch of the project and the
 * caller's own membership, shared by every tab under /projects/{id}. The
 * header carries the governance state (visibility + frozen badges); the
 * tab bar renders only the tabs the caller's role permits (Settings is
 * owner/maintainer-only — L1; the API-side enforcement of the settings
 * actions lands with T0109's endpoints).
 *
 * Authorization truth lives in the API's answers, never here: a 404
 * (existence hiding for a private project the caller may not see) renders
 * a plain not-found state WITHOUT shell chrome, and the tab content
 * (children) never mounts.
 */
export function ProjectShell({
  apiBaseUrl,
  projectId,
  children,
}: {
  apiBaseUrl: string;
  projectId: string;
  children: React.ReactNode;
}) {
  const client = useMemo(() => createProjectsClient(apiBaseUrl), [apiBaseUrl]);
  const pathname = usePathname();
  // The phase derives from data keyed by projectId — nothing is set
  // synchronously inside the effect, and navigating from one project to
  // another falls back to "loading" (never stale chrome) until the new
  // fetch lands.
  const [loaded, setLoaded] = useState<ShellLoaded | null>(null);
  const [failure, setFailure] = useState<ShellFailure | null>(null);

  useEffect(() => {
    let cancelled = false;
    // The two reads race in parallel. The membership answer decides the
    // tab gate only: a membership failure (or 401 when signed out) falls
    // back to "no role" — fail closed, no Settings tab — and never hides
    // a shell the project read authorized.
    Promise.all([
      client.get(projectId),
      client.myMembership(projectId).catch(() => null),
    ])
      .then(([project, membership]) => {
        if (cancelled) return;
        setLoaded({ projectId, project, membership });
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        setFailure({
          projectId,
          kind: err instanceof ApiError && err.status === 404 ? "not-found" : "error",
          message: messageForProjectCode(err instanceof ApiError ? err.code : "UNKNOWN"),
        });
      });
    return () => {
      cancelled = true;
    };
  }, [client, projectId]);

  const phase: "loading" | "not-found" | "error" | "ready" =
    loaded !== null && loaded.projectId === projectId
      ? "ready"
      : failure !== null && failure.projectId === projectId
        ? failure.kind
        : "loading";

  if (phase === "loading") {
    return (
      <div className="project-state-main">
        <Spinner aria-label="Loading project" />
      </div>
    );
  }

  // Private + unauthorized: the API answered the existence-hiding 404 —
  // render the same neutral not-found every stranger sees, with no shell
  // chrome (no name, no badges, no tabs) and no tab content.
  if (phase === "not-found") {
    return (
      <div className="project-state-main">
        <div className="project-state" data-project-notfound>
          <div className="project-state-icon" aria-hidden="true">
            <AlertIcon size={24} />
          </div>
          <h1 className="project-state-title">Project not found</h1>
          <p className="project-state-desc">
            This project does not exist, or you do not have access to it.
          </p>
          <Link className="project-state-button" href="/projects">
            Back to Projects
          </Link>
        </div>
      </div>
    );
  }

  if (phase === "error") {
    return (
      <div className="project-state-main">
        <div className="project-state">
          <div className="project-state-icon" aria-hidden="true">
            <AlertIcon size={24} />
          </div>
          <h1 className="project-state-title">Project unavailable</h1>
          <p className="project-state-desc">
            {failure !== null ? failure.message : ""}
          </p>
        </div>
      </div>
    );
  }

  if (loaded === null) {
    return null;
  }
  const { project, membership } = loaded;
  const role = membership?.role ?? null;
  const tabs = tabsForRole(role);

  return (
    <div className="project-shell" data-project-shell={project.slug}>
      <header className="project-shell-header">
        <p className="project-shell-breadcrumb">
          <Link href="/projects">Projects</Link> / {project.slug}
        </p>
        <div className="project-shell-title-row">
          <h1 className="project-shell-name">
            <RepoIcon size={16} aria-hidden="true" /> {project.name}
          </h1>
          <ProjectBadges project={project} />
        </div>
        {project.purpose !== "" ? (
          <p className="project-shell-purpose">{project.purpose}</p>
        ) : null}
      </header>
      <nav className="project-tabs" aria-label="Project">
        {tabs.map((tab) => {
          const href = projectTabHref(project.id, tab.path);
          const active = pathname === href;
          const Icon = tab.icon;
          return (
            <Link
              key={tab.key}
              href={href}
              className="project-tab"
              data-project-tab={tab.key}
              aria-current={active ? "page" : undefined}
            >
              <Icon size={14} aria-hidden="true" />
              {tab.label}
            </Link>
          );
        })}
      </nav>
      <ProjectShellContext.Provider value={{ project, role }}>
        <div className="project-tab-content">{children}</div>
      </ProjectShellContext.Provider>
    </div>
  );
}
