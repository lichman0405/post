import type { Metadata } from "next";
import { Suspense } from "react";
import { getWebConfig } from "../../../../lib/server-config";
import { webOrigin } from "../../../../lib/origin";
import {
  fetchPublicProfileMeta,
  hiddenPageMetadata,
  publicEntityPath,
  publicPageMetadata,
} from "../../../../lib/entity-meta";
import { ProfileCard } from "./profile-card";
import { ResearchProfileCard } from "./research-profile-card";

/**
 * The public profile page: server component that resolves the validated
 * API origin and hands it — plus the id from the URL — to the client-side
 * card. The web app has no backend of its own; the card reads the public
 * profile over the wire (T0102) and shows the owner an edit form only when
 * the signed-in session is the profile's own (the API enforces the same
 * rule server-side; the form visibility is just UX).
 *
 * The URL is id-keyed by design: it never changes when the owner renames
 * the handle (docs/21 §2 — stable ids are the reference identity).
 *
 * T0801: the <head> comes from the same public read, anonymously — a
 * profile is public (docs/02 §3 "公开 Research Profile"; GET
 * /api/v1/users/{id}/profile answers without a session), so it is
 * indexable; an unknown id gets the unindexable head instead.
 */
export async function generateMetadata({
  params,
}: {
  params: Promise<{ id: string }>;
}): Promise<Metadata> {
  const cfg = getWebConfig();
  const { id } = await params;
  const entity = await fetchPublicProfileMeta(cfg.apiBaseUrl, id);
  if (entity === null) return hiddenPageMetadata();
  const origin = await webOrigin();
  return publicPageMetadata(entity, `${origin}${publicEntityPath("profile", id)}`);
}

export default async function ProfilePage({
  params,
}: {
  params: Promise<{ id: string }>;
}) {
  const cfg = getWebConfig();
  const { id } = await params;
  return (
    <div className="profile-main">
      <Suspense fallback={null}>
        <ProfileCard apiBaseUrl={cfg.apiBaseUrl} userId={id} />
        {/* T0808: the Research Profile (docs/42) under the identity card.
            One page, two reads: the card above is the editable identity
            (owner-only PATCH), this one is the public research record. */}
        <ResearchProfileCard apiBaseUrl={cfg.apiBaseUrl} userId={id} />
      </Suspense>
    </div>
  );
}
