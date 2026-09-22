"use client";

import { useEffect, useMemo, useState } from "react";
import { AlertIcon, InfoIcon } from "@primer/octicons-react";
import { Spinner } from "@primer/react";

import {
  ApiError,
  citedSources,
  createSearchClient,
  evidenceRows,
  fallbackHeadlineKey,
  searchCodeKey,
  sourceHref,
  sourceKindLabel,
  sourceLabel,
  type EvidenceRow,
  type SearchAnswer,
  type SearchResponse,
  type SearchSource,
  type SearchStatement,
} from "../../../lib/search";
import { createAuthClient } from "../../../lib/auth";
import { useT } from "../../i18n-provider";
import "./search.css";

/**
 * The evidence-backed answer (T0907).
 *
 * Four states, and the page tells them apart rather than drawing them alike:
 *
 *   loading    the question is being searched; docs/27 requires a loading
 *              state, and it is rendered before anything else can be said.
 *   answered   a summary written from the sources, with the citations it
 *              leans on and the sources it may be checked against.
 *   fallback   NO summary, and this is NOT an error: it is the platform
 *              declining to write an answer it could not ground, and it is
 *              rendered as a result — an informational notice, never a red
 *              one — followed by the structured result, which is a complete
 *              answer to the search.
 *   error      the request did not produce an answer at all (a refusal or an
 *              unreachable service). Drawn as an alert, because a reader has
 *              nothing and has to know that.
 *
 * # What is on the page and where it comes from
 *
 * Every sentence below is either the answer document's own text or this
 * component's label for a value the document carries. There is no computed
 * claim: no count that is not the list's own length, no "verified" mark, no
 * ranking of its own, and no section rendered from a field the answer did not
 * send. An empty list renders NO section rather than a placeholder — a page
 * that filled the gap with "no conflicts recorded" would be asserting
 * something the producer never said.
 *
 * docs/42 §Search Answer and docs/05 §5 also name `comparison` and
 * `conditions`, and docs/42 an `Evidence Map`: the answer document has no key
 * for the first two, so this page does not draw them (deriving them from
 * `sources` here would make the page a second definition of what an answer
 * means). The Evidence Map is drawn, from the one thing in the document that
 * supports it: every limitation and conflict carries the `refs` it is about,
 * and those refs are aligned with the sources below in the claim↔evidence
 * section. That view is a projection of the answer's own statements, and it
 * says so on the page.
 *
 * # Sources and underlying results are one list
 *
 * docs/14 §4 asks for "source objects" and "underlying results"; the answer
 * layer renders them as ONE list, with `cited` marking the subset the summary
 * leans on (internal/search/answer/answer.go states the reason: they are the
 * same rows, and two lists could disagree about what the search found). The
 * page renders the cited subset as the answer's sources and the whole list,
 * in rank order, as the underlying results. Both come from one array.
 */
export function SearchAnswerView({
  query,
  apiBaseUrl,
}: {
  query: string;
  apiBaseUrl: string;
}) {
  // This component renders none of its own copy — the three states below
  // (AnswerLoading / AnswerError / AnswerBody) do, and each resolves its own
  // `useT()`. A `t` here would be a declaration with no reader, which is the
  // failure `unused_catalog_keys` catches in the catalog and lint catches here.
  // The CSRF token comes from the shared session storage, the same way the
  // inbox writes (apps/web/app/(main)/notifications/inbox-surface.tsx:61-63):
  // the search POST is a state
  // change, and without the header the guard answers 403 CSRF_FAILED.
  const authClient = useMemo(() => createAuthClient(apiBaseUrl), [apiBaseUrl]);
  const client = useMemo(
    () => createSearchClient(apiBaseUrl, { csrfToken: () => authClient.csrfToken() }),
    [apiBaseUrl, authClient],
  );
  const [attempt, setAttempt] = useState(0);
  const [state, setState] = useState<State>({ kind: "loading", for: query, attempt: 0 });

  useEffect(() => {
    let cancelled = false;
    // setState only in the callbacks: the effect's body talks to the network
    // and to nothing else (the same shape as AssetsBrowse).
    client.search(query).then(
      (response) => {
        if (!cancelled) setState({ kind: "ready", for: query, attempt, response });
      },
      (err: unknown) => {
        if (cancelled) return;
        const failure =
          err instanceof ApiError
            ? err
            : // The message of this synthetic error is a diagnostic, not copy:
              // nothing renders it (the panel below renders the CODE's
              // sentence, searchCodeKey). It stays a constant so the
              // scanner's `property:message` rule can tell "a sentence a
              // reader sees" from "a string an engineer greps a log for".
              new ApiError(0, { code: "UNREACHABLE", message: UNREACHABLE_MESSAGE });
        setState({
          kind: "error",
          for: query,
          attempt,
          status: failure.status,
          code: failure.code,
          requestId: failure.requestId,
          retryable: failure.retryable,
        });
      },
    );
    return () => {
      cancelled = true;
    };
  }, [client, query, attempt]);

  // An answer tagged with a different question — or with an earlier attempt at
  // this one — is not this question's answer: it is stale, and showing it
  // under the new question would attribute one search's evidence to another.
  // The tag is compared rather than a reset effect racing the fetch, which is
  // also what keeps the effect's body free of setState.
  const settled = state.for === query && state.attempt === attempt ? state : null;
  if (settled === null || settled.kind === "loading") {
    return <AnswerLoading />;
  }
  if (settled.kind === "error") {
    return (
      <AnswerError
        code={settled.code}
        requestId={settled.requestId}
        retryable={settled.retryable}
        onRetry={() => setAttempt((n) => n + 1)}
      />
    );
  }
  return <AnswerBody response={settled.response} apiBaseUrl={apiBaseUrl} />;
}

/** The diagnostic carried by the synthetic error for a request that never
 *  reached the API. Not copy: no component renders it. */
const UNREACHABLE_MESSAGE = "search unreachable";

/**
 * The state of one question's answer, tagged with the question it is about and
 * with the attempt that produced it (so a retry shows the loading state again
 * without the effect having to reset anything).
 */
type State =
  | { kind: "loading"; for: string; attempt: number }
  | { kind: "ready"; for: string; attempt: number; response: SearchResponse }
  | {
      kind: "error";
      for: string;
      attempt: number;
      status: number;
      code: string;
      requestId: string;
      retryable: boolean;
    };

/** The three dots github uses while it is fetching. Text, not a spinner. */
function AnswerLoading() {
  const t = useT();
  return (
    <div className="search-panel search-panel-loading" data-search-state="loading">
      <p className="search-loading" role="status" data-search-loading>
        {/* srText={null}: the visible sentence beside it is the announcement
            (Primer's own instruction for this case), so a screen reader hears
            the loading state once instead of twice. */}
        <Spinner size="small" srText={null} />
        <span>{t("search.loading")}</span>
      </p>
    </div>
  );
}

/**
 * A failure: no answer exists for this question, and the page says which
 * failure it was rather than drawing an empty result.
 *
 * The code is rendered with the sentence because the codes are the contract's
 * (docs/45) and they are what a reader reports when they ask for help; the
 * request id comes from the API's envelope (cmd/api/authhttp) for the same
 * reason.
 */
function AnswerError({
  code,
  requestId,
  retryable,
  onRetry,
}: {
  code: string;
  requestId: string;
  retryable: boolean;
  onRetry: () => void;
}) {
  const t = useT();
  return (
    <div className="search-panel search-panel-error" data-search-state="error">
      <p className="search-error" role="alert" data-search-error={code}>
        <AlertIcon size={16} aria-hidden />
        <span>{t(searchCodeKey(code))}</span>
      </p>
      <p className="search-error-meta">
        <span className="search-error-code">{code}</span>
        {requestId ? <span className="search-error-request">{t("search.error.request", { requestId })}</span> : null}
      </p>
      {retryable ? (
        <button type="button" className="search-retry" onClick={onRetry}>
          {t("search.retry")}
        </button>
      ) : null}
    </div>
  );
}

/** The answer, in the shape its own status decides. */
function AnswerBody({
  response,
  apiBaseUrl,
}: {
  response: SearchResponse;
  apiBaseUrl: string;
}) {
  const t = useT();
  const { answer, search_id: searchId } = response;
  const cited = citedSources(answer);
  const rows = evidenceRows(answer);

  return (
    <div
      className="search-panel"
      data-search-state="ready"
      data-search-answer
      data-search-id={searchId}
      data-status={answer.status}
    >
      {answer.status === "fallback" ? (
        <FallbackNotice answer={answer} />
      ) : (
        <SummaryBlock answer={answer} />
      )}

      {cited.length > 0 ? <CitedSources sources={cited} apiBaseUrl={apiBaseUrl} /> : null}

      <Statements section="limitations" statements={answer.limitations} />
      <Statements section="conflicts" statements={answer.conflicts} />
      <EvidenceMap rows={rows} apiBaseUrl={apiBaseUrl} />
      <UnderlyingResults sources={answer.sources} apiBaseUrl={apiBaseUrl} />

      <p className="search-provenance">
        {t("search.provenance.label")} <span className="search-provenance-id">{searchId}</span>
        {t("search.provenance.suffix")}
      </p>
    </div>
  );
}

/**
 * The summary, and the View label docs/14 §4 requires beside it.
 *
 * The label is rendered from `answer_view`, the answer document's own
 * boolean, and only when it is true: a summary the document does not call a
 * View must not be labelled one on this page.
 */
function SummaryBlock({ answer }: { answer: SearchAnswer }) {
  const t = useT();
  if (answer.summary === "") return null;
  return (
    <section className="search-section" data-search-section="answer">
      <div className="search-section-head">
        <h2 className="search-section-title">{t("search.answer.title")}</h2>
        {answer.answer_view ? (
          <span className="search-badge" data-search-view-badge title={t("search.view.tooltip")}>
            {t("search.view.badge")}
          </span>
        ) : null}
      </div>
      <p className="search-summary" data-search-summary>
        {answer.summary}
      </p>
      {answer.citations.length > 0 ? (
        <div className="search-citations" data-search-citations>
          <span className="search-citations-label">{t("search.cites")}</span>
          {answer.citations.map((ref) => (
            <span className="search-citation" key={ref} data-search-citation={ref}>
              {ref}
            </span>
          ))}
        </div>
      ) : null}
    </section>
  );
}

/**
 * A fallback: the structured result, presented as a result.
 *
 * The headline is this page's sentence for the reason code; the platform's
 * own account of the same event is the answer's first limitation, rendered
 * verbatim by Statements below. Nothing here is styled as a failure — no
 * alert role, no danger colour — because the search succeeded and the
 * platform chose not to write an answer without evidence to ground it.
 */
function FallbackNotice({ answer }: { answer: SearchAnswer }) {
  const t = useT();
  const reason = answer.reason ?? "";
  return (
    <section className="search-section search-fallback" data-search-fallback={reason}>
      <div className="search-section-head">
        <h2 className="search-section-title">{t("search.fallback.title")}</h2>
        <span className="search-badge search-badge-neutral" data-search-fallback-reason>
          {reason === "" ? t("search.fallback.noReason") : reason}
        </span>
      </div>
      <p className="search-fallback-headline" data-search-fallback-headline>
        <InfoIcon size={16} aria-hidden />
        <span>{t(fallbackHeadlineKey(reason))}</span>
      </p>
      <p className="search-fallback-note">
        {t("search.fallback.note")}
      </p>
    </section>
  );
}

/**
 * The sources the answer leans on — the cited subset of the one source list.
 *
 * Its heading carries the distinction the answer layer draws: these are the
 * rows the summary's citations name, and they are also rows of the underlying
 * results below, never a second set of entities.
 */
function CitedSources({
  sources,
  apiBaseUrl,
}: {
  sources: SearchSource[];
  apiBaseUrl: string;
}) {
  const t = useT();
  return (
    <section className="search-section" data-search-section="sources">
      <div className="search-section-head">
        <h2 className="search-section-title">{t("search.citedSources.title")}</h2>
        <span className="search-count">{sources.length}</span>
      </div>
      <ul className="search-list">
        {sources.map((source) => (
          <li className="search-row" key={source.ref} data-search-cited-ref={source.ref}>
            <SourceTitle source={source} apiBaseUrl={apiBaseUrl} />
            <p className="search-row-meta">
              <span className="search-ref">{source.ref}</span>
              <span className="search-kind">{sourceKindLabel(source)}</span>
            </p>
          </li>
        ))}
      </ul>
    </section>
  );
}

/**
 * Limitations and conflicts, as the answer states them.
 *
 * `text` is the platform's sentence and is rendered unchanged; `refs` is
 * rendered as the identity of the sources it is about (the evidence map below
 * resolves them to rows). `origin` is not rendered as a badge: every statement
 * the platform produces has the one origin, and a page that labelled all of
 * them would be labelling nothing.
 */
function Statements({
  section,
  statements,
}: {
  section: "limitations" | "conflicts";
  statements: SearchStatement[];
}) {
  const t = useT();
  if (statements.length === 0) return null;
  const title = section === "limitations" ? t("search.limitations") : t("search.conflicts");
  return (
    <section className="search-section" data-search-section={section}>
      <div className="search-section-head">
        <h2 className="search-section-title">{title}</h2>
        <span className="search-count">{statements.length}</span>
      </div>
      <ul className="search-list">
        {statements.map((statement, i) => (
          <li className="search-row search-statement" key={`${section}-${i}`}>
            <p className="search-statement-text">{statement.text}</p>
            {statement.refs && statement.refs.length > 0 ? (
              <p className="search-row-meta">
                {statement.refs.map((ref) => (
                  <span className="search-ref" key={ref}>
                    {ref}
                  </span>
                ))}
              </p>
            ) : null}
          </li>
        ))}
      </ul>
    </section>
  );
}

/**
 * The claim↔evidence view (docs/42's "Evidence Map").
 *
 * It is a PROJECTION of the answer's own data and the page says so: each
 * statement the platform derived (a limitation or a conflict) beside the
 * sources its `refs` name. Statements that named no source are about the
 * search rather than about any one source and have no row here; they are
 * rendered, unchanged, in the lists above.
 *
 * A ref that names no source of this answer is rendered as itself, with no
 * row: the answer layer's grounding guard makes that impossible today, and if
 * it ever happened, printing the ref is what lets a reader see it rather than
 * this page quietly inventing a source to attach it to.
 */
function EvidenceMap({ rows, apiBaseUrl }: { rows: EvidenceRow[]; apiBaseUrl: string }) {
  const t = useT();
  const withRefs = rows.filter((row) => row.refs.length > 0);
  if (withRefs.length === 0) return null;
  return (
    <section className="search-section" data-search-section="evidence-map">
      <div className="search-section-head">
        <h2 className="search-section-title">{t("search.evidenceMap.title")}</h2>
        <span className="search-count">{withRefs.length}</span>
      </div>
      <p className="search-section-note">
        {t("search.evidenceMap.note")}
      </p>
      <ul className="search-list">
        {withRefs.map((row, i) => {
          const resolved = new Set(row.sources.map((source) => source.ref));
          return (
            <li className="search-row" key={`evidence-${i}`}>
              <p className="search-statement-text">
                <span className="search-statement-kind">{row.section}</span>
                {row.text}
              </p>
              <ul className="search-evidence-refs">
                {row.sources.map((source) => (
                  <li key={source.ref}>
                    <SourceTitle source={source} apiBaseUrl={apiBaseUrl} />
                    <span className="search-ref">{source.ref}</span>
                  </li>
                ))}
                {row.refs
                  .filter((ref) => !resolved.has(ref))
                  .map((ref) => (
                    <li key={ref}>
                      <span className="search-ref" data-search-unresolved-ref={ref}>
                        {ref}
                      </span>
                    </li>
                  ))}
              </ul>
            </li>
          );
        })}
      </ul>
    </section>
  );
}

/**
 * The underlying results: every source the search returned, in rank order.
 *
 * This is docs/05 §5's "bottom of the page" list and it is the same array the
 * cited sources came from. The order is the ranking's and this page does not
 * re-sort it; `rank` is a position, and the factors behind each position are
 * carried through so an order is never shown that cannot be explained.
 */
function UnderlyingResults({
  sources,
  apiBaseUrl,
}: {
  sources: SearchSource[];
  apiBaseUrl: string;
}) {
  const t = useT();
  if (sources.length === 0) return null;
  return (
    <section className="search-section" data-search-section="results">
      <div className="search-section-head">
        <h2 className="search-section-title">{t("search.results.title")}</h2>
        <span className="search-count">{sources.length}</span>
      </div>
      <ul className="search-list">
        {sources.map((source) => (
          <li
            className="search-row"
            key={source.ref}
            data-search-source-ref={source.ref}
            data-cited={source.cited ? "true" : "false"}
            data-rank={source.rank}
          >
            <p className="search-row-head">
              <span className="search-rank">#{source.rank}</span>
              <SourceTitle source={source} apiBaseUrl={apiBaseUrl} />
              {source.cited ? (
                <span className="search-badge search-badge-neutral" data-search-cited-mark>
                  {t("search.cited")}
                </span>
              ) : null}
            </p>
            <p className="search-row-meta">
              <span className="search-ref">{source.ref}</span>
              <span className="search-kind">{sourceKindLabel(source)}</span>
              {source.version ? (
                <span className="search-version">{t("search.sourceVersion", { version: source.version })}</span>
              ) : null}
              {/* The project travels with a source whether or not it has an
                  address, and for an unaddressed source it is the only handle
                  on where the entity lives. */}
              {source.href ? null : source.project_id ? (
                <span className="search-project">{t("search.sourceProject", { projectId: source.project_id })}</span>
              ) : null}
            </p>
            {source.labels.length > 0 ? (
              <p className="search-labels">
                {source.labels.map((label) => (
                  <span className="search-label" key={label}>
                    {label}
                  </span>
                ))}
              </p>
            ) : null}
            {source.factors.length > 0 ? (
              <details className="search-factors">
                <summary className="search-factors-summary">
                  {t("search.factors.summary", { count: source.factors.length })}
                </summary>
                <ul className="search-factors-list">
                  {source.factors.map((factor) => (
                    <li className="search-factor" key={factor.factor}>
                      <span className="search-factor-name">{factor.factor}</span>
                      <span className="search-factor-level">{factor.level}</span>
                      <span className="search-factor-reason">{factor.reason}</span>
                    </li>
                  ))}
                </ul>
              </details>
            ) : null}
          </li>
        ))}
      </ul>
    </section>
  );
}

/**
 * A source's title, linked to the address the answer gives for it.
 *
 * The href comes from the answer (internal/search/answer/locator.go) and is
 * an API path in the answer's own namespace; it is resolved against that
 * origin and never composed from the entity's id or type here — a path this
 * page invented would be a link to something the search did not return. A
 * source with no address is rendered as text, which is what "the platform has
 * no address for this kind of entity" looks like.
 */
function SourceTitle({ source, apiBaseUrl }: { source: SearchSource; apiBaseUrl: string }) {
  const href = sourceHref(apiBaseUrl, source.href);
  if (href === "") {
    return <span className="search-row-title">{sourceLabel(source)}</span>;
  }
  return (
    <a
      className="search-row-title search-row-link"
      href={href}
      data-search-source-link={source.ref}
    >
      {sourceLabel(source)}
    </a>
  );
}
