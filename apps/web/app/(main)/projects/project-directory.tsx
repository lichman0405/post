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
import { useT } from "../../i18n-provider";
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
  const t = useT();
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
          setLoadError(t("projects.error.signIn"));
        } else {
          setLoadError(messageForProjectCode(err instanceof ApiError ? err.code : "UNKNOWN"));
        }
      });
    return () => {
      cancelled = true;
    };
    // `t` belongs in the deps: the error sentence is resolved from the
    // catalog inside the catch, so switching language must re-run the fetch
    // (and its error path) rather than leaving a stale-language message on
    // screen. `useT()` is memoized on the locale, so this is one extra fetch
    // per language change and never one per render.
  }, [client, t]);

  return (
    <div className="project-directory-main">
      <h1 className="project-directory-title">{t("projects.title")}</h1>
      <p className="project-directory-sub">
        {t("projects.subtitle")}
      </p>
      {loadError !== null ? (
        <div className="project-list-empty">{loadError}</div>
      ) : projects === null ? (
        <Spinner aria-label={t("projects.loading")} />
      ) : projects.length === 0 ? (
        <div className="project-list-empty">
          {t("projects.empty")}
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
