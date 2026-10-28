"use client";

import { useEffect, useMemo, useRef, useState, type FormEvent } from "react";
import Link from "next/link";
import { Spinner } from "@primer/react";

import { createAuthClient } from "../../../lib/auth";
import {
  ApiError,
  createFieldForError,
  createProjectsClient,
  messageForProjectCode,
  projectNameProblem,
  projectPurposeProblem,
  projectSlugProblem,
  suggestProjectSlug,
  type Project,
  type ProjectOrganization,
} from "../../../lib/projects";
import { useT } from "../../i18n-provider";
import { ProjectBadges } from "./project-badges";
import "./projects.css";

/**
 * The project directory (client component): lists the projects the signed-in
 * actor may see (GET /api/v1/projects) — newest first — with the governance
 * state per row (visibility + frozen main) and a link into the project shell.
 * The API already applies the visibility filter (T0106): public projects for
 * every reader, plus the caller's own; the directory renders what the API
 * answered and never widens it.
 *
 * WEB-04 adds the CREATE surface (docs/70 G04, D16). It is not a second
 * route: it is the panel this page opens, because that is the surface the
 * reader is already on when they have no project to start from. Two entries
 * reach it — the header control on a populated directory, and the empty
 * state's "start your first research project", which is D16's "空状态要给出
 * 第一条研究路径入口": a project is where a research question, its hypotheses
 * and the evidence live, so the first project IS the first research path.
 *
 * D16's two rules are enforced here and are NOT decoration:
 *   - Public is the PRESELECTED visibility and Private is one click away;
 *   - the choice applies to the project being created only — the panel says
 *     so, and nothing on this page can change an existing project's
 *     visibility (the API refuses that outright: VISIBILITY_CHANGE_NOT_
 *     SUPPORTED, T0109).
 *
 * A refusal is put where the reader can act on it: createFieldForError maps
 * the API's stable code onto the control that produced it (a duplicate
 * address under the Address field), and anything form-level — signed out,
 * organization policy, a store failure — renders above the submit button.
 * The API's own message is shown for a VALIDATION_FAILED refusal, because
 * that is the sentence which names the rule that was broken.
 */
export function ProjectDirectory({ apiBaseUrl }: { apiBaseUrl: string }) {
  const t = useT();
  const auth = useMemo(() => createAuthClient(apiBaseUrl), [apiBaseUrl]);
  const client = useMemo(
    () => createProjectsClient(apiBaseUrl, { csrfToken: () => auth.csrfToken() }),
    [apiBaseUrl, auth],
  );

  const [projects, setProjects] = useState<Project[] | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  // Bumped after a create so the list is re-read from the API rather than
  // patched locally: the row the reader sees is the row the API holds.
  const [reloadToken, setReloadToken] = useState(0);
  const [formOpen, setFormOpen] = useState(false);
  const [createdSlug, setCreatedSlug] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    client
      .list()
      .then((list) => {
        if (!cancelled) {
          setProjects(list);
          setLoadError(null);
        }
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
  }, [client, t, reloadToken]);

  const isEmpty = projects !== null && projects.length === 0 && loadError === null;

  function openForm() {
    setCreatedSlug(null);
    setFormOpen(true);
  }

  return (
    <div className="project-directory-main">
      <div className="project-directory-header">
        <div>
          <h1 className="project-directory-title">{t("projects.title")}</h1>
          <p className="project-directory-sub">{t("projects.subtitle")}</p>
        </div>
        {!formOpen ? (
          <button
            type="button"
            className="project-create-open"
            data-project-create="open"
            onClick={openForm}
          >
            {t("projects.create.title")}
          </button>
        ) : null}
      </div>

      {createdSlug !== null ? (
        <p className="project-create-flash" role="status" data-project-create="created">
          {t("projects.create.created", { slug: createdSlug })}
        </p>
      ) : null}

      {formOpen ? (
        <CreateProjectForm
          client={client}
          onCancel={() => setFormOpen(false)}
          onCreated={(slug) => {
            setCreatedSlug(slug);
            setFormOpen(false);
            setReloadToken((n) => n + 1);
          }}
        />
      ) : null}

      {loadError !== null ? (
        <div className="project-list-empty">{loadError}</div>
      ) : projects === null ? (
        <Spinner aria-label={t("projects.loading")} />
      ) : isEmpty ? (
        <div className="project-list-empty" data-project-empty>
          <p className="project-empty-lede">{t("projects.empty")}</p>
          <p className="project-empty-path">{t("projects.create.emptyPath")}</p>
          <button
            type="button"
            className="project-create-open"
            data-project-create="empty-entry"
            onClick={openForm}
          >
            {t("projects.create.emptyEntry")}
          </button>
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

/** The create panel (D16). Fields are native controls with explicit ids and
 *  labels: the form is on a core page, so its copy comes from the catalog and
 *  its structure has to survive an axe scan (a control with no accessible
 *  name fails one). */
function CreateProjectForm({
  client,
  onCancel,
  onCreated,
}: {
  client: ReturnType<typeof createProjectsClient>;
  onCancel: () => void;
  onCreated: (slug: string) => void;
}) {
  const t = useT();
  const [name, setName] = useState("");
  const [slug, setSlug] = useState("");
  // The address follows the name until the reader edits it themselves. A
  // suggestion that kept overwriting a typed address would be the form
  // fighting its user.
  const slugTouched = useRef(false);
  const [purpose, setPurpose] = useState("");
  const [visibility, setVisibility] = useState<"public" | "private">("public");
  const [organizationId, setOrganizationId] = useState("");
  // null means "not known" — the organization read is lazy (the panel opens
  // signed in or not) and a failed read renders NO organization choice at
  // all rather than a sentence claiming the reader belongs to none.
  const [organizations, setOrganizations] = useState<ProjectOrganization[] | null>(null);
  const [busy, setBusy] = useState(false);
  const [fieldErrors, setFieldErrors] = useState<{
    name?: string;
    slug?: string;
    purpose?: string;
  }>({});
  const [formError, setFormError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    client
      .myOrganizations()
      .then((orgs) => {
        if (!cancelled) setOrganizations(orgs);
      })
      .catch(() => {
        // No claim either way: the picker simply does not render.
      });
    return () => {
      cancelled = true;
    };
  }, [client]);

  /** A field's own rule, resolved to copy. */
  function problemMessage(problem: "required" | "invalid"): string {
    return problem === "required" ? t("projects.create.required") : t("projects.create.invalid");
  }

  function submit(event: FormEvent) {
    event.preventDefault();
    if (busy) return;
    const problems = {
      name: projectNameProblem(name),
      slug: projectSlugProblem(slug),
      purpose: projectPurposeProblem(purpose),
    };
    const nextErrors: { name?: string; slug?: string; purpose?: string } = {};
    if (problems.name !== null) nextErrors.name = problemMessage(problems.name);
    if (problems.slug !== null) nextErrors.slug = problemMessage(problems.slug);
    if (problems.purpose !== null) nextErrors.purpose = problemMessage(problems.purpose);
    setFieldErrors(nextErrors);
    setFormError(null);
    if (Object.keys(nextErrors).length > 0) return;

    setBusy(true);
    client
      .create({
        slug,
        name,
        purpose,
        visibility,
        organization_id: organizationId === "" ? null : organizationId,
      })
      .then(({ project }) => {
        // The parent closes the panel, flashes the new address and re-reads
        // the list; nothing is patched into the local list here.
        onCreated(project.slug);
      })
      .catch((err: unknown) => {
        const field = createFieldForError(err);
        if (field !== null && err instanceof ApiError) {
          // The API names the field through a stable code; a validation
          // refusal carries the rule it broke, which is more useful to the
          // reader than a generic sentence, so that message is shown as-is.
          setFieldErrors({
            [field]:
              err.code === "VALIDATION_FAILED" ? err.message : messageForProjectCode(err.code),
          });
          return;
        }
        if (err instanceof ApiError && err.status === 401) {
          setFormError(t("projects.error.signIn"));
          return;
        }
        setFormError(
          messageForProjectCode(
            err instanceof ApiError ? err.code : "UNKNOWN",
            t("projects.create.failed"),
          ),
        );
      })
      .finally(() => {
        setBusy(false);
      });
  }

  return (
    <form className="project-create" data-project-create="form" onSubmit={submit} noValidate>
      <h2 className="project-create-title">{t("projects.create.title")}</h2>
      <p className="project-create-help">{t("projects.create.help")}</p>

      <div className="project-create-field">
        <label className="project-create-label" htmlFor="project-create-name">
          {t("projects.create.name")}
        </label>
        <input
          id="project-create-name"
          className="project-create-input"
          data-project-field="name"
          type="text"
          maxLength={200}
          value={name}
          aria-invalid={fieldErrors.name !== undefined}
          aria-describedby={fieldErrors.name !== undefined ? "project-create-name-error" : undefined}
          onChange={(event) => {
            const value = event.target.value;
            setName(value);
            if (!slugTouched.current) setSlug(suggestProjectSlug(value));
          }}
        />
        {fieldErrors.name !== undefined ? (
          <p
            className="project-create-error"
            id="project-create-name-error"
            role="alert"
            data-project-field-error="name"
          >
            {fieldErrors.name}
          </p>
        ) : null}
      </div>

      <div className="project-create-field">
        <label className="project-create-label" htmlFor="project-create-slug">
          {t("projects.create.address")}
        </label>
        <input
          id="project-create-slug"
          className="project-create-input project-create-input-mono"
          data-project-field="slug"
          type="text"
          maxLength={64}
          value={slug}
          aria-invalid={fieldErrors.slug !== undefined}
          aria-describedby={
            fieldErrors.slug !== undefined ? "project-create-slug-error" : "project-create-slug-hint"
          }
          onChange={(event) => {
            slugTouched.current = true;
            setSlug(event.target.value);
          }}
        />
        {fieldErrors.slug !== undefined ? (
          <p
            className="project-create-error"
            id="project-create-slug-error"
            role="alert"
            data-project-field-error="slug"
          >
            {fieldErrors.slug}
          </p>
        ) : (
          <p className="project-create-hint" id="project-create-slug-hint">
            {t("projects.create.addressHint")}
          </p>
        )}
      </div>

      <div className="project-create-field">
        <label className="project-create-label" htmlFor="project-create-purpose">
          {t("projects.create.purpose")}
        </label>
        <textarea
          id="project-create-purpose"
          className="project-create-input project-create-purpose"
          data-project-field="purpose"
          rows={3}
          maxLength={4000}
          value={purpose}
          aria-invalid={fieldErrors.purpose !== undefined}
          aria-describedby={
            fieldErrors.purpose !== undefined
              ? "project-create-purpose-error"
              : "project-create-purpose-hint"
          }
          onChange={(event) => setPurpose(event.target.value)}
        />
        {fieldErrors.purpose !== undefined ? (
          <p
            className="project-create-error"
            id="project-create-purpose-error"
            role="alert"
            data-project-field-error="purpose"
          >
            {fieldErrors.purpose}
          </p>
        ) : (
          <p className="project-create-hint" id="project-create-purpose-hint">
            {t("projects.create.purposeHint")}
          </p>
        )}
      </div>

      {/* D16: Public is preselected; Private is one click away, and the note
          says what each choice means for who can read the project. */}
      <fieldset className="project-create-visibility">
        <legend className="project-create-label">{t("projects.create.visibility")}</legend>
        <label className="project-create-option" data-project-visibility-option="public">
          <input
            type="radio"
            name="project-visibility"
            value="public"
            checked={visibility === "public"}
            data-project-visibility="public"
            onChange={() => setVisibility("public")}
          />
          <span className="project-create-option-body">
            <span className="project-create-option-title">{t("projects.badge.public")}</span>
            <span className="project-create-option-note">
              {t("projects.create.visibilityPublicNote")}
            </span>
          </span>
        </label>
        <label className="project-create-option" data-project-visibility-option="private">
          <input
            type="radio"
            name="project-visibility"
            value="private"
            checked={visibility === "private"}
            data-project-visibility="private"
            onChange={() => setVisibility("private")}
          />
          <span className="project-create-option-body">
            <span className="project-create-option-title">{t("projects.badge.private")}</span>
            <span className="project-create-option-note">
              {t("projects.create.visibilityPrivateNote")}
            </span>
          </span>
        </label>
      </fieldset>

      {organizations !== null ? (
        organizations.length > 0 ? (
          <div className="project-create-field">
            <label className="project-create-label" htmlFor="project-create-organization">
              {t("projects.create.organization")}
            </label>
            <select
              id="project-create-organization"
              className="project-create-input"
              data-project-field="organization"
              value={organizationId}
              onChange={(event) => setOrganizationId(event.target.value)}
            >
              <option value="">{t("projects.create.personal")}</option>
              {organizations.map((org) => (
                <option key={org.id} value={org.id}>
                  {org.name} ({org.slug})
                </option>
              ))}
            </select>
            <p className="project-create-hint">{t("projects.create.organizationHint")}</p>
          </div>
        ) : (
          <p className="project-create-hint" data-project-organizations="none">
            {t("projects.create.noOrganizations")}
          </p>
        )
      ) : null}

      {formError !== null ? (
        <p className="project-create-error" role="alert" data-project-create="error">
          {formError}
        </p>
      ) : null}

      <div className="project-create-actions">
        <button
          type="submit"
          className="project-create-submit"
          data-project-create="submit"
          disabled={busy}
        >
          {busy ? t("projects.create.submitting") : t("projects.create.submit")}
        </button>
        <button
          type="button"
          className="project-create-cancel"
          data-project-create="cancel"
          disabled={busy}
          onClick={onCancel}
        >
          {t("projects.create.cancel")}
        </button>
      </div>
    </form>
  );
}
