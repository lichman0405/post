"use client";

import { useEffect, useMemo, useState } from "react";
import Link from "next/link";
import { usePathname } from "next/navigation";
import { Flash, Heading, Label, Spinner, Text } from "@primer/react";
import { BeakerIcon, BellIcon, ProjectIcon } from "@primer/octicons-react";

import {
  loginHref,
  sessionEndedNotice,
  signupHref,
  type AuthUser,
} from "../../lib/auth";
import {
  ApiError as ProjectsApiError,
  createProjectsClient,
  messageForProjectCode,
  type Project,
} from "../../lib/projects";
import { useT } from "../i18n-provider";
import { useNavSession } from "./nav-session";

/**
 * The home workbench (T1303 / WEB-02, G02).
 *
 * docs/05 §2: a signed-in Home is a workspace — My Projects, what needs
 * attention and the entry into starting or continuing research — and the
 * signed-out Home is product content with a sign-in entry, not a marketing
 * page. This component is that page's body:
 *
 *   - signed out: the sign-in entry, the create-an-account entry and
 *     Explore (docs/05 §2's public entry);
 *   - signed in: the project list (GET /api/v1/projects — the API's
 *     visibility-filtered list, which is the union of the actor's projects
 *     and public projects; there is no mine-only read yet), the entry into
 *     the project workspaces where a project is started or continued, and
 *     a shortcut to notifications. There is no research-inbox (待办) read on
 *     this page: that surface is not implemented yet, so nothing here
 *     claims to show it;
 *   - a session this tab had but the API no longer accepts is reported as
 *     exactly that, with a sign-in link back to the current page, instead of
 *     a silent sign-out. On Home the destination is "/" — the default — so
 *     the link resolves to plain /login; loginHref only adds a `next=`
 *     parameter for a non-default page.
 *
 * WHY THIS FILE LIVES IN app/nav/: the session is resolved once by
 * NavSessionProvider and shared through context; the workbench is a
 * consumer of that shared state, so it sits with it. Nothing here renders
 * chrome — the header is global-nav.tsx's.
 *
 * Copy: the ordinary sentences come from the catalog (lib/i18n.ts). The two
 * exceptions are refusal-path messages — the session-ended notice from
 * lib/auth.ts and the project-list load error from lib/projects.ts (via
 * messageForProjectCode) — both of which live with the other refusal
 * messages. NOTHING HERE IS COVERED BY THE i18n HARD-CODED SCANNER
 * (tests/i18n/scan-hardcoded.mjs): this file is not in its `core` group, so
 * the "0 hard-coded literals" claim does not include it. The refusal-path
 * literals are listed in that scanner's CORE_LIB_OUT as the known gap.
 *
 * Service diagnostics do NOT belong here any more: they moved to /status
 * (the administration surface), and the home only links to it.
 */
export function HomeWorkbench({ apiBaseUrl }: { apiBaseUrl: string }) {
  const t = useT();
  const { user, loading, ended } = useNavSession();

  return (
    <div className="home-workbench">
      <header className="home-header">
        <BeakerIcon size={24} aria-hidden />
        <Heading as="h1" className="home-title">
          {t("nav.home")}
        </Heading>
      </header>

      {loading ? (
        <div className="home-lede" aria-live="polite" aria-busy="true">
          {/* Primer's Spinner carries its own srText; the visible sentence is
              the announcement, exactly as the header's session strip does. */}
          <Spinner size="small" srText={null} />
          <Text>{t("nav.checkingSession")}</Text>
        </div>
      ) : ended ? (
        <SessionEnded />
      ) : user === null ? (
        <SignedOut />
      ) : (
        // Keyed by account: signing in as somebody else mounts a fresh
        // list rather than reusing the previous account's projects.
        <SignedIn key={user.id} user={user} apiBaseUrl={apiBaseUrl} />
      )}

      {/* The administration surface: service diagnostics live on /status.
          A muted link here is all the home keeps of them. */}
      <footer className="home-admin">
        <Link href="/status" className="home-admin-link">
          {t("home.developmentStatus")}
        </Link>
      </footer>
    </div>
  );
}

/** The signed-out Home (docs/05 §2): product entries, not a marketing page. */
function SignedOut() {
  const t = useT();
  return (
    <div className="home-entries">
      <p className="home-lede">
        <Link href={loginHref("/")} className="home-entry home-entry-primary" data-home-entry="sign-in">
          {t("nav.signIn")}
        </Link>{" "}
        {t("home.signInPrompt")}
      </p>
      <p className="home-lede">
        <Link
          href={signupHref("/")}
          className="home-entry"
          data-home-entry="create-account"
        >
          {t("login.createAnAccount")}
        </Link>
      </p>
      <p className="home-lede">
        <Link href="/explore" className="home-entry" data-home-entry="explore">
          {t("nav.explore")}
        </Link>
      </p>
    </div>
  );
}

/**
 * A session this tab had, which the API no longer accepts. The reader is
 * told what happened and given the way back to this page — the requirement
 * is a recoverable prompt, not an unexplained signed-out state.
 *
 * The link is built from the CURRENT path, so the prompt returns the reader
 * to the page it is on if the workbench is ever rendered somewhere other
 * than Home. On Home that path is "/", which is safeNextPath's default
 * destination, so the href is plain /login and no `next=` parameter is
 * produced; the journey asserts exactly that href rather than claiming a
 * parameter that is not there.
 */
function SessionEnded() {
  const t = useT();
  const pathname = usePathname();
  return (
    <Flash variant="danger" className="home-flash" role="alert">
      <Text className="home-flash-text">{sessionEndedNotice()}</Text>{" "}
      <Link
        href={loginHref(pathname)}
        className="home-entry home-entry-primary"
        data-home-entry="recover-session"
      >
        {t("nav.signIn")}
      </Link>
    </Flash>
  );
}

/** The signed-in Home: My Projects, the project-workspace entry, notifications. */
function SignedIn({ user, apiBaseUrl }: { user: AuthUser; apiBaseUrl: string }) {
  const t = useT();
  const client = useMemo(() => createProjectsClient(apiBaseUrl), [apiBaseUrl]);
  // Null means "not answered yet"; an empty array is the honest empty state.
  // The fetch resolves the project list from the API — the page renders no
  // project data the API did not answer.
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
        setLoadError(
          err instanceof ProjectsApiError
            ? messageForProjectCode(err.code)
            : messageForProjectCode("UNKNOWN"),
        );
      });
    return () => {
      cancelled = true;
    };
  }, [client]);

  return (
    <div>
      <p className="home-lede">
        {t("nav.signedInAs", { name: user.display_name || user.handle })}
      </p>

      <section className="home-section" aria-labelledby="home-projects-heading">
        <Heading as="h2" id="home-projects-heading" className="home-section-title">
          {t("projects.title")}
        </Heading>
        {loadError !== null ? (
          <div className="home-empty">{loadError}</div>
        ) : projects === null ? (
          <div className="home-lede" aria-live="polite" aria-busy="true">
            <Spinner size="small" srText={null} />
            <Text>{t("projects.loading")}</Text>
          </div>
        ) : projects.length === 0 ? (
          <div className="home-empty" data-home-projects="empty">
            {t("projects.empty")}
          </div>
        ) : (
          <ul className="home-project-list" data-home-projects="list">
            {projects.map((project) => (
              <li key={project.id} className="home-project-row">
                <Link
                  href={`/projects/${project.id}`}
                  className="home-project-link"
                  data-project-row={project.slug}
                >
                  <ProjectIcon size={16} aria-hidden />
                  <span className="home-project-name">{project.name}</span>
                </Link>
                <span className="home-project-slug">{project.slug}</span>
                <Label
                  variant={project.visibility === "private" ? "attention" : "secondary"}
                >
                  {project.visibility === "private"
                    ? t("projects.badge.private")
                    : t("projects.badge.public")}
                </Label>
              </li>
            ))}
          </ul>
        )}
        {/* The entry into starting or continuing research lives with the
            projects themselves: /projects is the workspace where a project
            is created and opened (WEB-04 adds the creation form there). */}
        <Link
          href="/projects"
          className="home-entry home-entry-primary"
          data-home-entry="project-workspaces"
        >
          <ProjectIcon size={16} aria-hidden />
          <span className="home-entry-label">{t("project.overview.workspaces")}</span>
        </Link>
      </section>

      <ul className="home-shortcuts">
        <li>
          <Link href="/notifications" className="home-entry" data-home-entry="notifications">
            <BellIcon size={16} aria-hidden />
            <span className="home-entry-label">{t("nav.notifications")}</span>
          </Link>
        </li>
      </ul>
    </div>
  );
}
