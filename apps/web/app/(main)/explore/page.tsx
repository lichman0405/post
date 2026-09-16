import type { Metadata } from "next";

import { getWebConfig } from "../../../lib/server-config";
import {
  ApiError,
  createExploreClient,
  messageForExploreCode,
  type ExploreIndex,
  type FetchLike,
} from "../../../lib/explore";
import { ExploreTabs } from "./explore-tabs";
import "./explore.css";

export const metadata: Metadata = {
  title: "Explore — POST",
  description:
    "The open network of POST: public projects, published assets and knowledge, research profiles, organizations and open contributions — newest first.",
};

/**
 * The Explore surface (T0802, docs/05 §6): the aggregated public index.
 *
 * The page is a server component that reads the index over the wire and
 * renders it into the HTML. That is a deliberate choice over the app's
 * usual client-side card: this page IS the platform's directory, and a
 * directory whose contents only exist after a browser fetch is a directory
 * no crawler and no reader without JavaScript can use. The API call is
 * anonymous (cmd/api/explorehttp reads no principal), so there is nothing
 * here that depends on a session.
 *
 * `cache: "no-store"` is not a performance decision: the index is a live
 * view of the newest public entities, and a cached copy served after a
 * publication would show an index that is missing the newest thing in it.
 * A network directory is the one page where a stale cache reads as "this
 * does not exist yet".
 *
 * # Failure is not emptiness (docs/51)
 *
 * A failed read renders an ERROR panel, never an empty one — the API answers
 * 503 rather than a partial index for exactly this reason (a section
 * silently missing reads as "there is nothing there"), and the page must not
 * undo that by drawing the failure as six empty tabs.
 */
export default async function ExplorePage() {
  const cfg = getWebConfig();
  const index = await readIndex(cfg.apiBaseUrl);

  return (
    <>
      {/* Without script the tablist cannot switch panels, so every panel is
          revealed and the page is one long, complete document — with the
          section headings as its structure. The `!important` is over the
          `hidden` attribute, which the panels carry for the tab pattern;
          with script, the tabs take over and this rule is inert (a noscript
          element's contents are not applied by a browser that runs script). */}
      <noscript>
        <style
          dangerouslySetInnerHTML={{
            __html: ".explore-panel[hidden]{display:block !important}",
          }}
        />
      </noscript>
      {index.ok ? (
        <ExploreTabs index={index.index} />
      ) : (
        <div className="explore">
          <h1 className="explore-title">Explore</h1>
          <p className="explore-error" role="alert" data-explore-error={index.code}>
            {index.message}
          </p>
        </div>
      )}
    </>
  );
}

type IndexRead =
  | { ok: true; index: ExploreIndex }
  | { ok: false; code: string; message: string };

/**
 * readIndex reads the index once, in the shape the page renders.
 *
 * The read goes through createExploreClient — the SAME reader a client
 * component would use, and the one lib/explore.test.mjs tests — with one
 * injected difference: `cache: "no-store"`. The index is a live view of the
 * newest public entities, and a cached copy served after a publication would
 * show an index missing the newest thing in it.
 *
 * A failed read is a failure, never an empty network: an unreachable API, a
 * wire code the API named, and a body that is not an index all render the
 * error panel. The shape guard inside the client (lib/explore.isIndexShape)
 * is what makes a truncated answer a failure rather than a tab that claims a
 * dimension is empty — it is the reason the client, and not this page, owns
 * the decision of what counts as an index.
 */
async function readIndex(apiBaseUrl: string): Promise<IndexRead> {
  const noStore: FetchLike = (input, init) =>
    fetch(input, { ...init, cache: "no-store" } as RequestInit);
  try {
    const index = await createExploreClient(apiBaseUrl, { fetch: noStore }).index();
    return { ok: true, index };
  } catch (err) {
    const code = err instanceof ApiError ? err.code : "UNREACHABLE";
    return { ok: false, code, message: messageForExploreCode(code) };
  }
}
