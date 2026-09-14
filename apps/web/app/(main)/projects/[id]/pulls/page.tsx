"use client";

import { useEffect, useMemo, useState } from "react";
import Link from "next/link";
import { GitPullRequestIcon } from "@primer/octicons-react";
import { Spinner } from "@primer/react";

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
          <table className="pulls-table">
            <thead>
              <tr>
                <th>Number</th>
                <th>Title</th>
                <th>State</th>
                <th>Opened</th>
              </tr>
            </thead>
            <tbody>
              {pulls.map((pr) => (
                <tr key={pr.id} data-pull-row={pr.number}>
                  <td className="pulls-number">
                    <Link
                      className="pulls-number-link"
                      href={`/projects/${shell.project.id}/pulls/${pr.number}`}
                    >
                      #{pr.number}
                    </Link>
                  </td>
                  <td className="pulls-title">{pr.title}</td>
                  <td className="pulls-state" data-pull-state={pr.state}>
                    {pr.state}
                  </td>
                  <td className="pulls-date">{pr.created_at.slice(0, 10)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>
    </div>
  );
}
