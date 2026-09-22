"use client";

import { useEffect, useMemo, useState } from "react";
import Link from "next/link";
import { GitPullRequestIcon } from "@primer/octicons-react";
import { Spinner } from "@primer/react";
import { Table } from "@post/ui";

import {
  ApiError,
  createPullsClient,
  pullRequestCodeKey,
  type PullRequest,
} from "../../../../../lib/pulls";
import { useT } from "../../../../i18n-provider";
import { useProjectShell } from "../shell-context";

/**
 * Pull requests tab (T0403): the project's research PRs, oldest first
 * (the API's number order). Each row links to the detail page, where the
 * machine integrity report (the /checks endpoint) renders the review
 * answer: what blocks, what warns, and why.
 */
export default function PullsPage() {
  const shell = useProjectShell();
  const t = useT();

  const [pulls, setPulls] = useState<PullRequest[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  const client = useMemo(
    () => (shell === null ? null : createPullsClient(shell.apiBaseUrl)),
    [shell],
  );

  useEffect(() => {
    if (shell === null || client === null) return;
    let cancelled = false;
    client
      .list(shell.project.id)
      .then((list) => {
        if (!cancelled) setPulls(list);
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        setError(
          err instanceof ApiError
            ? t(pullRequestCodeKey(err.code))
            : t("pull.list.error.load"),
        );
      });
    return () => {
      cancelled = true;
    };
    // `t` in the deps: the fallback sentence is resolved in the catch.
  }, [shell, client, t]);

  if (shell === null) {
    // The shell only mounts tab content in its ready state; null means a
    // wiring error, not a user-visible page.
    return null;
  }

  return (
    <div className="pulls-page" data-pulls-list>
      <section className="pulls-section">
        <h2 className="pulls-section-title">{t("pull.list.title")}</h2>
        <p className="pulls-section-desc">
          {t("pull.list.intro", { project: shell.project.name })}
        </p>
        {error !== null ? (
          <div className="pulls-error" data-pulls-error>{error}</div>
        ) : pulls === null ? (
          <Spinner aria-label={t("pull.list.loading")} />
        ) : pulls.length === 0 ? (
          <div className="pulls-empty" data-pulls-empty>
            <GitPullRequestIcon size={16} aria-hidden="true" />
            {t("pull.list.empty")}
          </div>
        ) : (
          /* T1101: `.pulls-table` was one of two byte-identical table
             stylesheets in projects.css (the other is `.settings-members`);
             the shared Table owns that shape now and this list only says
             what its columns are. */
          <Table
            className="pulls-table"
            columns={[
              {
                key: "number",
                header: t("pull.list.col.number"),
                render: (pr) => (
                  <Link
                    className="pulls-number-link"
                    href={`/projects/${shell.project.id}/pulls/${pr.number}`}
                  >
                    #{pr.number}
                  </Link>
                ),
                cellAttrs: () => ({ className: "pulls-number" }),
              },
              {
                key: "title",
                header: t("pull.list.col.title"),
                render: (pr) => pr.title,
                cellAttrs: () => ({ className: "pulls-title" }),
              },
              {
                key: "state",
                header: t("pull.list.col.state"),
                render: (pr) => pr.state,
                cellAttrs: (pr) => ({ className: "pulls-state", "data-pull-state": pr.state }),
              },
              {
                key: "opened",
                header: t("pull.list.col.opened"),
                render: (pr) => pr.created_at.slice(0, 10),
                cellAttrs: () => ({ className: "pulls-date" }),
              },
            ]}
            rows={pulls}
            rowKey={(pr) => pr.id}
            rowAttrs={(pr) => ({ "data-pull-row": pr.number })}
          />
        )}
      </section>
    </div>
  );
}
