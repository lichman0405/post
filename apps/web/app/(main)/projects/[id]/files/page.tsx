"use client";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import Link from "next/link";
import {
  BeakerIcon,
  DownloadIcon,
  EyeClosedIcon,
  FileDirectoryFillIcon,
  FileIcon,
  GitBranchIcon,
  HistoryIcon,
  LinkIcon,
} from "@primer/octicons-react";
import { Spinner } from "@primer/react";

import {
  ApiError,
  createFilesClient,
  messageForFileCode,
  type FilesCommitEntry,
  type FilesFileView,
  type FilesTreeListing,
} from "../../../../../lib/files";
import { Sidebar, StateLabel } from "@post/ui";
import { useProjectShell } from "../shell-context";

/**
 * Files tab (T0308): the read-only Git/Blob view over the T0307 Files API
 * (docs/06 §7, docs/17 §1 — the Files Web page is strictly read-only).
 *
 * Layout: the repository tree on the left (one directory at main), the
 * file preview in the middle (the API's safe preview: text, binary,
 * too_large, symlink, gitlink, blob-pointer), and the context sidebar on
 * the right — repository ref/SHA, the selected file's git facts, the
 * download link, the per-file commit history with one raw diff link per
 * commit, and the jump into the project's Scientific Context (the
 * Research tab).
 *
 * Read-only is structural here, not a note: the page renders no form, no
 * input, no upload/drop target and no mutating call — every interactive
 * element is a navigation (buttons that change what is shown, links that
 * open the raw/raw-diff streams or another page). The files e2e pins the
 * absence of mutation controls in both the DOM and the network traffic.
 * Authorization truth stays in the API's answers: the shell already
 * answered the existence-hiding 404, so this page mounts only where the
 * project read succeeded; the files reads themselves answer per their own
 * policy and their errors render inline.
 */

const REF = "main";

/** The two-pane entry row: name + an optional right-side detail. */
function TreeRow({
  entry,
  onClick,
}: {
  entry: { name: string; path: string; type: string; size: number };
  onClick: (path: string) => void;
}) {
  const isTree = entry.type === "tree";
  return (
    <li>
      <button
        type="button"
        className="files-entry"
        data-files-entry={entry.name}
        data-entry-type={isTree ? "tree" : "blob"}
        onClick={() => onClick(entry.path)}
        title={entry.path}
      >
        <span className="files-entry-icon" aria-hidden="true">
          {isTree ? <FileDirectoryFillIcon size={16} /> : <FileIcon size={16} />}
        </span>
        <span className="files-entry-name">{entry.name}</span>
        {!isTree ? (
          <span className="files-entry-size">{formatBytes(entry.size)}</span>
        ) : null}
      </button>
    </li>
  );
}

/** The commit history list with one raw diff link per commit. */
function HistoryList({
  entries,
  diffUrlFor,
}: {
  entries: FilesCommitEntry[];
  diffUrlFor: (sha: string) => string;
}) {
  if (entries.length === 0) {
    return <p className="files-empty">No commits touch this path yet.</p>;
  }
  return (
    <ul className="files-history" data-files-history>
      {entries.map((entry) => (
        <li className="files-history-entry" data-files-history-entry key={entry.sha}>
          <p className="files-history-message">{entry.message.split("\n")[0]}</p>
          <p className="files-history-meta">
            <span className="files-history-author">{entry.author}</span>
            {" · "}
            <time dateTime={entry.date}>{formatDate(entry.date)}</time>
            {" · "}
            <span className="files-history-sha" title={entry.sha}>
              {entry.sha.slice(0, 7)}
            </span>
          </p>
          <a
            className="files-diff-link"
            data-files-diff
            href={diffUrlFor(entry.sha)}
            target="_blank"
            rel="noreferrer"
            aria-label={`Raw diff for ${entry.sha}`}
          >
            raw diff
          </a>
        </li>
      ))}
    </ul>
  );
}

export default function FilesPage() {
  const shell = useProjectShell();
  const client = useMemo(
    () => (shell === null ? null : createFilesClient(shell.apiBaseUrl)),
    [shell],
  );
  const projectId = shell?.project.id ?? null;

  const [listing, setListing] = useState<FilesTreeListing | null>(null);
  const [currentPath, setCurrentPath] = useState("");
  const [treeError, setTreeError] = useState<string | null>(null);
  const [file, setFile] = useState<FilesFileView | null>(null);
  const [fileError, setFileError] = useState<string | null>(null);
  const [history, setHistory] = useState<FilesCommitEntry[] | null>(null);
  const [busyPath, setBusyPath] = useState<string | null>(null);

  // The tree load and the preview load race against each other and
  // against navigation; the latest request wins, stale answers drop
  // (the same cancelled-fetch discipline as the shell).
  const treeSeq = useRef(0);
  const fileSeq = useRef(0);

  const loadTree = useCallback(
    (path: string) => {
      if (client === null || projectId === null) return;
      const seq = ++treeSeq.current;
      client
        .tree(projectId, { ref: REF, path })
        .then((next) => {
          if (seq === treeSeq.current) setListing(next);
        })
        .catch((err: unknown) => {
          if (seq !== treeSeq.current) return;
          setTreeError(
            err instanceof ApiError ? messageForFileCode(err.code) : "Could not list this directory.",
          );
        });
    },
    [client, projectId],
  );

  const openPath = useCallback(
    (path: string) => {
      if (client === null || projectId === null) return;
      setCurrentPath(path);
      setFileError(null);
      setBusyPath(path);
      const seq = ++fileSeq.current;
      setFile(null);
      setHistory(null);
      Promise.all([
        client.content(projectId, { ref: REF, path }),
        client.history(projectId, { ref: REF, path }),
      ])
        .then(([view, commits]) => {
          if (seq !== fileSeq.current) return;
          setFile(view);
          setHistory(commits);
        })
        .catch((err: unknown) => {
          if (seq !== fileSeq.current) return;
          setFileError(
            err instanceof ApiError ? messageForFileCode(err.code) : "Could not preview this file.",
          );
        })
        .finally(() => {
          if (seq === fileSeq.current) setBusyPath(null);
        });
    },
    [client, projectId],
  );

  const descend = useCallback(
    (path: string) => {
      setCurrentPath(path);
      setListing(null);
      setTreeError(null);
      loadTree(path);
    },
    [loadTree],
  );

  // All files state belongs to one project; when the shell hands us a
  // different one (tab navigation between projects), reset it during
  // render — the sanctioned alternative to setState-in-effect — so the
  // previous project's tree can never flash into the new one.
  const [loadedFor, setLoadedFor] = useState<string | null>(null);
  if (projectId !== null && loadedFor !== projectId) {
    setLoadedFor(projectId);
    setListing(null);
    setTreeError(null);
    setFile(null);
    setFileError(null);
    setHistory(null);
    setBusyPath(null);
    setCurrentPath("");
  }

  // One tree load per project mount (the tab starts at the root of main).
  // The sequence bumps invalidate every in-flight read of the previous
  // project before the new load starts, so a stale answer can never
  // overwrite the new project's state.
  useEffect(() => {
    if (client === null || projectId === null) return;
    treeSeq.current++;
    fileSeq.current++;
    loadTree("");
  }, [client, projectId, loadTree]);

  if (shell === null || client === null || projectId === null) {
    return null;
  }

  const segments = currentPath === "" ? [] : currentPath.split("/");
  const downloadHref = (path: string) => client.rawUrl(projectId, { ref: REF, path });
  const diffHref = (sha: string) => client.diffUrl(projectId, sha);
  const scientificContextHref = `/projects/${projectId}/research`;

  return (
    <div className="files-page" data-files-page>
      <header className="files-header">
        <h2 className="files-title">
          <GitBranchIcon size={16} aria-hidden="true" /> Files
        </h2>
        <span className="files-ref">
          <GitBranchIcon size={14} aria-hidden="true" /> {REF}
        </span>
        <StateLabel shape="readonly" tone="success" icon={EyeClosedIcon} data-files-readonly>
          Read-only
        </StateLabel>
      </header>

      <div className="files-layout">
        <nav className="files-tree" data-files-tree aria-label="Repository tree">
          {/* T1101: the trail is the shared Sidebar. The path controls stay
              this page's own — they are buttons with their own `onClick`,
              and `data-files-crumb` is what the Files e2e selects — while
              the row, the separators and the crumb hit target come from
              `post-sidebar-action` instead of a fourth `.files-crumb`
              spelling of the same chip. */}
          <div className="files-breadcrumb" data-files-breadcrumb>
            <Sidebar
              label="Repository path"
              tone="accent"
              trail="row"
              crumbs={[
                {
                  key: "root",
                  content: (
                    <button
                      type="button"
                      className="post-sidebar-action"
                      data-files-crumb="root"
                      onClick={() => descend("")}
                      aria-current={segments.length === 0 ? "page" : undefined}
                    >
                      <GitBranchIcon size={14} aria-hidden="true" /> {REF}
                    </button>
                  ),
                },
                ...segments.map((seg, i) => {
                  const path = segments.slice(0, i + 1).join("/");
                  return {
                    key: path,
                    content: (
                      <button
                        type="button"
                        className="post-sidebar-action"
                        data-files-crumb={path}
                        onClick={() => descend(path)}
                        aria-current={i === segments.length - 1 ? "page" : undefined}
                      >
                        {seg}
                      </button>
                    ),
                  };
                }),
              ]}
            />
          </div>
          {listing === null && treeError === null ? (
            <div className="files-state">
              <Spinner aria-label="Loading repository tree" />
            </div>
          ) : null}
          {treeError !== null ? (
            <div className="files-error" data-files-tree-error>
              <p>{treeError}</p>
              <button type="button" className="files-retry" onClick={() => loadTree(currentPath)}>
                Try again
              </button>
            </div>
          ) : null}
          {listing !== null ? (
            listing.entries.length === 0 ? (
              <p className="files-empty">This directory is empty.</p>
            ) : (
              <ul className="files-entry-list">
                {[...listing.entries]
                  .sort((a, b) =>
                    a.type === b.type ? a.name.localeCompare(b.name) : a.type === "tree" ? -1 : 1,
                  )
                  .map((entry) => (
                    <TreeRow
                      key={entry.path}
                      entry={entry}
                      onClick={entry.type === "tree" ? descend : openPath}
                    />
                  ))}
              </ul>
            )
          ) : null}
        </nav>

        <section className="files-preview" data-files-preview aria-label="File preview">
          {file !== null ? (
            <>
              <div className="files-preview-header">
                <span className="files-preview-name" data-files-preview-name={file.name}>
                  {file.name}
                </span>
                {file.kind !== "text" ? (
                  <StateLabel shape="kind" tone="neutral" data-files-kind={file.kind}>
                    {file.kind}
                  </StateLabel>
                ) : null}
                <span className="files-preview-size">{formatBytes(file.size)}</span>
              </div>
              <FileBody file={file} />
            </>
          ) : busyPath !== null ? (
            <div className="files-state">
              <Spinner aria-label="Loading file preview" />
            </div>
          ) : fileError !== null ? (
            <div className="files-error" data-files-preview-error>
              <p>{fileError}</p>
            </div>
          ) : (
            <p className="files-hint">Select a file to preview it.</p>
          )}
        </section>

        <aside className="files-context" data-files-context aria-label="File context">
          <section className="files-context-section">
            <h3 className="files-context-title">Repository</h3>
            <dl className="files-facts">
              <dt>Branch</dt>
              <dd>{REF}</dd>
              <dt>Tree SHA</dt>
              <dd className="files-sha" title={listing?.sha ?? ""}>
                {listing === null ? "…" : (listing.sha || "unknown").slice(0, 12)}
              </dd>
            </dl>
          </section>

          {file !== null ? (
            <section className="files-context-section" data-files-facts>
              <h3 className="files-context-title">File</h3>
              <dl className="files-facts">
                <dt>Path</dt>
                <dd className="files-fact-path" title={file.path}>
                  {file.path}
                </dd>
                <dt>Type</dt>
                <dd>{file.type}</dd>
                <dt>Mode</dt>
                <dd>{file.mode}</dd>
                <dt>Size</dt>
                <dd>{formatBytes(file.size)}</dd>
                <dt>Blob SHA</dt>
                <dd className="files-sha" title={file.sha}>
                  {file.sha.slice(0, 12)}
                </dd>
              </dl>
            </section>
          ) : null}

          {file !== null ? (
            <section className="files-context-section">
              <h3 className="files-context-title">Actions</h3>
              <ul className="files-actions">
                <li>
                  <a
                    className="files-action"
                    data-files-download
                    href={downloadHref(file.path)}
                    aria-label={`Download ${file.name}`}
                  >
                    <DownloadIcon size={14} aria-hidden="true" /> Download
                  </a>
                </li>
                <li>
                  <Link
                    className="files-action"
                    data-files-scientific-context
                    href={scientificContextHref}
                  >
                    <BeakerIcon size={14} aria-hidden="true" /> View scientific context
                  </Link>
                </li>
              </ul>
              <p className="files-context-note">
                Files mutate only through the scientific workflow — never here.
              </p>
            </section>
          ) : null}

          {file !== null ? (
            <section className="files-context-section">
              <h3 className="files-context-title">
                <HistoryIcon size={14} aria-hidden="true" /> History
              </h3>
              {history === null ? (
                <div className="files-state">
                  <Spinner aria-label="Loading commit history" />
                </div>
              ) : (
                <HistoryList entries={history} diffUrlFor={diffHref} />
              )}
            </section>
          ) : null}
        </aside>
      </div>
    </div>
  );
}

/** The preview body for one file kind (the API's own vocabulary). */
function FileBody({ file }: { file: FilesFileView }) {
  switch (file.kind) {
    case "text":
      return (
        <div className="files-text-body">
          <pre className="files-content" data-files-content>
            {file.content}
          </pre>
          {file.truncated ? (
            <p className="files-truncated-note" data-files-truncated>
              Preview truncated at 256 KiB — download the file to see it whole.
            </p>
          ) : null}
          {file.blob_pointer !== undefined ? (
            <dl className="files-pointer" data-files-blob-pointer>
              <dt>Blob pointer</dt>
              <dd>
                This manifest points at a blob in the platform store (docs/17 §2); Git holds
                only the pointer. Content hash:{" "}
                <code>{file.blob_pointer.content_hash}</code>, size:{" "}
                {formatBytes(file.blob_pointer.size_bytes)}
                {file.blob_pointer.blob_id !== undefined && file.blob_pointer.blob_id !== ""
                  ? `, blob id: ${file.blob_pointer.blob_id}`
                  : ""}
                .
              </dd>
            </dl>
          ) : null}
        </div>
      );
    case "binary":
      return (
        <p className="files-kind-note" data-files-kind-note>
          Binary file — {formatBytes(file.size)}. Download it to inspect the bytes.
        </p>
      );
    case "too_large":
      return (
        <p className="files-kind-note" data-files-kind-note>
          Too large to preview ({formatBytes(file.size)}) — download it instead.
        </p>
      );
    case "symlink":
      return (
        <p className="files-kind-note" data-files-kind-note>
          <LinkIcon size={14} aria-hidden="true" /> Symbolic link →{" "}
          <code>{file.target ?? ""}</code>
        </p>
      );
    case "gitlink":
      return (
        <p className="files-kind-note" data-files-kind-note>
          Submodule → <code>{file.target ?? ""}</code>
        </p>
      );
    default:
      return null;
  }
}

/** Human-readable byte counts ("3.2 KiB"), never a promise of precision. */
function formatBytes(size: number): string {
  if (size < 1024) return `${size} B`;
  const units = ["KiB", "MiB", "GiB"];
  let value = size;
  let unit = -1;
  do {
    value /= 1024;
    unit += 1;
  } while (value >= 1024 && unit < units.length - 1);
  return `${value.toFixed(value >= 100 ? 0 : 1)} ${units[unit]}`;
}

/** A short commit date ("2026-09-14"), from the API's RFC3339 value. */
function formatDate(value: string): string {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  return date.toISOString().slice(0, 10);
}
