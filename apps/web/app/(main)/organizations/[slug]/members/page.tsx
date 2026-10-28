import type { Metadata } from "next";
import { Suspense } from "react";

import { getWebConfig } from "../../../../../lib/server-config";
import { getT } from "../../../../../lib/i18n-server";
import { OrganizationMembers } from "../../organization-members";

/**
 * The organization member page (T1304 / WEB-03, G03) at
 * /organizations/{slug}/members.
 *
 * Slug-keyed like the public profile next door, and for the same reason: the
 * slug is the organization's stable public address, so a member can go
 * straight from the profile they were reading to the governance of the
 * organization it belongs to. The numeric id the API writes with is resolved
 * from the reader's OWN organization list — a caller who is not a member
 * has no row to resolve and is told exactly that, rather than being handed
 * an id they may not use.
 *
 * A server component that hands the validated API origin and the slug to the
 * client body, exactly as the profile route does: the session and the member
 * list are browser state, so the page shell renders whether or not the API
 * is up and the body owns loading, refusal and error.
 */
export async function generateMetadata(): Promise<Metadata> {
  const { t } = await getT();
  return { title: t("nav.organizations") };
}

export default async function OrganizationMembersPage({
  params,
}: {
  params: Promise<{ slug: string }>;
}) {
  const cfg = getWebConfig();
  const { slug } = await params;
  return (
    <div className="org-members-main">
      <Suspense fallback={null}>
        <OrganizationMembers apiBaseUrl={cfg.apiBaseUrl} slug={slug} />
      </Suspense>
    </div>
  );
}
