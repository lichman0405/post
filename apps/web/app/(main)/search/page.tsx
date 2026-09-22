import type { Metadata } from "next";

import { getWebConfig } from "../../../lib/server-config";
import { getT } from "../../../lib/i18n-server";
import { SearchAnswerView } from "./search-answer";
import "./search.css";

/** T1105: the tab title and the page description are copy, so they follow
 *  the language preference like everything else on the page. */
export async function generateMetadata(): Promise<Metadata> {
  const { t } = await getT();
  return {
    title: t("search.metaTitle"),
    description: t("search.metaDescription"),
  };
}

/**
 * The Search Answer surface (T0907, docs/05 §5, docs/42 §Search Answer).
 *
 * The question lives in the URL — the global header's search form is a GET to
 * /search (app/nav/global-nav.tsx) — and this page reads it from
 * `searchParams`. That is what makes a result addressable: a refresh, a
 * copied link and a new tab all render the answer for the same question,
 * because nothing about the question is held in component state, storage or
 * a session.
 *
 * # Why the query is rendered here and the answer is not
 *
 * The question is rendered by THIS server component, from the URL, before any
 * script runs: it is the one part of the page that is true without the API,
 * and it is what a reader sees while the answer is on its way (a blank page
 * that only fills in after a fetch reads as a page that lost the query).
 *
 * The ANSWER is read by the client component below, and deliberately: POST
 * /search requires the session cookie and a JSON body, and the answer is the
 * caller's own scoped result (the API answers it `Cache-Control: no-store`).
 * Rendering a private, per-caller answer into the server's HTML would put it
 * into a document built for whoever asked for the page, cached by whatever
 * sits in front of it.
 */
export default async function SearchPage({
  searchParams,
}: {
  searchParams: Promise<{ q?: string }>;
}) {
  const params = await searchParams;
  const raw = params.q;
  const query = typeof raw === "string" ? raw.trim() : "";
  const cfg = getWebConfig();
  const { t } = await getT();

  return (
    <div className="search">
      <h1 className="search-title">{t("search.title")}</h1>
      {query === "" ? (
        <p className="search-intro">
          {/* Split around the example rather than interpolated: the example
              is an element (it is styled), and a catalog value carries text
              only. The example keeps its numerals and its units verbatim in
              both locales — "3 mmol/g" and "298 K" are a quantity and a
              temperature, and docs/28 §4 is explicit that a unit is not a
              translation problem. */}
          {t("search.introPrefix")}{" "}
          <span className="search-intro-example">
            {t("search.introExample")}
          </span>{" "}
          {t("search.introSuffix")}
        </p>
      ) : (
        <>
          <p className="search-query">
            {t("search.answerFor")} <span data-search-query>{query}</span>
          </p>
          <SearchAnswerView query={query} apiBaseUrl={cfg.apiBaseUrl} />
        </>
      )}
    </div>
  );
}
