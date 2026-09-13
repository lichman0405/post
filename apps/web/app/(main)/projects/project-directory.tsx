"use client";

import { useEffect, useMemo, useState } from "react";
import Link from "next/link";
import { Spinner } from "@primer/react";

import {
  ApiError,
  createProjectsClient,
  messageForProjectCode,
  type Project,
} from "../../../lib/projects";
import { ProjectBadges } from "./project-badges";
import "./projects.css";

/**
 * The project directory (client component): lists the signed-in actor's
 * projects from GET /api/v1/projects — newest first — with the governance
 * state per row (visibility + frozen main) and a link into the project
 * shell. The API already filters to the actor's projects (list filtering
 * by visibility is T0106); the directory renders what the API answered.
 */
export function ProjectDirectory({ apiBaseUrl }: { apiBaseUrl: string }) {
  const client = useMemo(() => createProjectsClient(apiBaseUrl), [apiBaseUrl]);

  const [projects, setProjects] = useState<Project[] | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    client
      .list()
      .then((list) => {
        if (!cancelled) setProjects(list);
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        if (err instanceof ApiError && err.status === 401) {
          // Signed out: the API answers 401 for the member-only listing.
          setLoadError("Sign in to see your projects.");
        } else {
          setLoadError(messageForProjectCode(err instanceof ApiError ? err.code : "UNKNOWN"));
        }
      });
    return () => {
      cancelled = true;
    };
  }, [client]);

  return (
    <div className="project-directory-main">
      <h1 className="project-directory-title">Projects</h1>
      <p className="project-directory-sub">
        Your research projects — visibility state, frozen-main status and
        research questions per project.
      </p>
      {loadError !== null ? (
        <div className="project-list-empty">{loadError}</div>
      ) : projects === null ? (
        <Spinner aria-label="Loading projects" />
      ) : projects.length === 0 ? (
        <div className="project-list-empty">
          No projects yet. Projects you join — or create — appear here.
        </div>
      ) : (
        <div className="project-list">
          {projects.map((project) => (
            <Link
              key={project.id}
              href={`/projects/${project.id}`}
              className="project-row"
              data-project-row={project.slug}
            >
              <span className="project-row-title">
                <span className="project-row-name">{project.name}</span>
                <span className="project-row-slug">{project.slug}</span>
              </span>
              <ProjectBadges project={project} />
              {project.purpose !== "" ? (
                <p className="project-row-purpose">{project.purpose}</p>
              ) : null}
            </Link>
          ))}
        </div>
      )}
    </div>
  );
}
