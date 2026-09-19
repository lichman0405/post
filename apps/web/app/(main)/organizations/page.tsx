import type { Metadata } from "next";

import { OrganizationLookup } from "./organization-lookup";

export const metadata: Metadata = {
  title: "Organizations — POST",
};

/**
 * /organizations (T0808).
 *
 * This used to be a ComingSoon stub promising that "organization profiles
 * land in T0808". They have: /organizations/{slug} renders the Organization
 * Research Profile (docs/05 §4). What is still missing is the DIRECTORY, and
 * the stub is replaced by a page that says exactly that rather than by a
 * different promise — GET /api/v1/organizations is member-only, so there is
 * no public list to render and no rule that would let this page invent one.
 */
export default function OrganizationsPage() {
  return (
    <div className="org-index">
      <h1 className="org-index-title">Organizations</h1>
      <p className="org-index-desc">
        An organization&apos;s research profile is public: its projects, the
        activity its affiliated people recorded while they were affiliated, and
        the assets its public projects published — all as recorded facts, with
        no score, rank or rating.
      </p>
      <p className="org-index-desc">
        There is no public directory of organizations yet: the list endpoint
        answers members only.
      </p>
      <OrganizationLookup />
    </div>
  );
}
