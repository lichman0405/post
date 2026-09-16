import type { Metadata } from "next";

import { getWebConfig } from "../../../../../../lib/server-config";
import { webOrigin } from "../../../../../../lib/origin";
import {
  fetchPublicReleaseMeta,
  hiddenPageMetadata,
  publicEntityPath,
  publicPageMetadata,
} from "../../../../../../lib/entity-meta";
import ReleaseDetail from "./release-detail";

/**
 * One release (T0606) — the server half of the route. It renders the
 * client component that fetches and draws the snapshot for the reader,
 * and it builds the <head> a crawler sees.
 *
 * T0801: the release route is one of the public entity routes (docs/05
 * §3 "Releases：immutable snapshots"), so it gets its own indexable head —
 * the release's title and version, over the project it belongs to. The
 * head comes from an ANONYMOUS read of both, which is the same answer a
 * crawler gets: a release of a project the anonymous caller may not read
 * answers the existence-hiding 404, and the page then carries the
 * unindexable, name-free head instead (lib/entity-meta.ts).
 *
 * Why the split: a client component cannot export metadata, and the
 * release body must keep fetching with the reader's own session. The head
 * and the body therefore come from two different asks, by design — the
 * head from the public answer, the body from the reader's.
 *
 * One consequence of where this head comes from, worth knowing before
 * changing anything here: Next merges the metadata of every segment along
 * the route, so the PROJECT layout's canonical survives on this page
 * whenever this segment hides itself (a release id the project does not
 * have). Title, description and `noindex, nofollow` are this segment's own
 * and DO win; the canonical that remains names the public project the
 * crawler is already under, which discloses nothing. The private case is
 * unaffected: there the layout hides itself too, and the whole head is
 * generic. tests/e2e-anonymous pins both.
 */
export async function generateMetadata({
  params,
}: {
  params: Promise<{ id: string; releaseId: string }>;
}): Promise<Metadata> {
  const cfg = getWebConfig();
  const { id, releaseId } = await params;
  const entity = await fetchPublicReleaseMeta(cfg.apiBaseUrl, id, releaseId);
  if (entity === null) return hiddenPageMetadata();
  const origin = await webOrigin();
  return publicPageMetadata(entity, `${origin}${publicEntityPath("release", id, releaseId)}`);
}

export default function ReleaseDetailPage() {
  return <ReleaseDetail />;
}
