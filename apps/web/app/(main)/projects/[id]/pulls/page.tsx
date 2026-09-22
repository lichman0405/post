"use client";

import { useEffect, useMemo, useState } from "react";
import Link from "next/link";
import { GitPullRequestIcon } from "@primer/octicons-react";
import { Spinner } from "@primer/react";
import { Table } from "@post/ui";

import {
  ApiError,
  createPullsClient,
  messageForPullRequestCode,
  type PullRequest,
} from "../../../../../lib/pulls";
import { useProjectShell } from "../shell-context";

/**
 * Pull requests tab (T0403): the project's research PRs, oldest first
 * (the API's number order). Each row links to the detail page, where the
 * machine integrity report (the /checks endpoint) renders the review
 * answer: what blocks, what warns, and why.
 */
export default function PullsPage() {
  const shell = useProjectShell();

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
            ? messageForPullRequestCode(err.code)
            : "Could not load pull requests.",
        );
      });
    return () => {
      cancelled = true;
    };
  }, [shell, client]);

  if (shell === null) {
    // The shell only mounts tab content in its ready state; null means a
    // wiring error, not a user-visible page.
    return null;
  }

  return (
    <div className="pulls-page" data-pulls-list>
      <section className="pulls-section">
        <h2 className="pulls-section-title">Pull requests</h2>
        <p className="pulls-section-desc">
          Proposed research-state diffs against {shell.project.name}&apos;s
          main branch, oldest first. Open a pull request to see its machine
          integrity report.
        </p>
        {error !== null ? (
          <div className="pulls-error" data-pulls-error>{error}</div>
        ) : pulls === null ? (
          <Spinner aria-label="Loading pull requests" />
        ) : pulls.length === 0 ? (
          <div className="pulls-empty" data-pulls-empty>
            <GitPullRequestIcon size={16} aria-hidden="true" />
            No pull requests yet. Research PRs appear here once proposed.
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
                header: "Number",
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
                header: "Title",
                render: (pr) => pr.title,
                cellAttrs: () => ({ className: "pulls-title" }),
              },
              {
                key: "state",
                header: "State",
                render: (pr) => pr.state,
                cellAttrs: (pr) => ({ className: "pulls-state", "data-pull-state": pr.state }),
              },
              {
                key: "opened",
                header: "Opened",
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
