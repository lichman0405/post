import type { Metadata } from "next";
import { Suspense } from "react";

import { getWebConfig } from "../../../../lib/server-config";
import { OrganizationProfileCard } from "./organization-profile-card";

/**
 * The public Organization Research Profile page (docs/05 §4) at
 * /organizations/{slug}.
 *
 * Slug-keyed, and the slug is normalized case-insensitively by the API
 * (domain.NormalizeOrgSlug), so one spelling is enough here: both "Institute"
 * and "institute" reach the same document.
 *
 * The page is a server component that resolves the validated API origin and
 * hands it, plus the slug from the URL, to the client card — the same shape
 * /users/{id} uses. There is no server-side fetch, so the page renders its
 * shell whether or not the API is up, and the card owns the loading, the
 * not-found and the unavailable states.
 *
 * Indexing is deliberately NOT claimed here: a profile is public and
 * indexable by the same rule the person profile follows, but the head is
 * built from a server-side read (see /users/{id}, which uses
 * lib/entity-meta) and this task does not add one. The title is therefore
 * generic rather than derived — an honest gap, not a stub page.
 */
export const metadata: Metadata = {
  title: "Organization profile — POST",
};

export default async function OrganizationProfilePage({
  params,
}: {
  params: Promise<{ slug: string }>;
}) {
  const cfg = getWebConfig();
  const { slug } = await params;
  return (
    <div className="profile-main">
      <Suspense fallback={null}>
        <OrganizationProfileCard apiBaseUrl={cfg.apiBaseUrl} slug={slug} />
      </Suspense>
    </div>
  );
}
