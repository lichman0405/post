import type { Metadata } from "next";

import { getWebConfig } from "../../../lib/server-config";
import { OrganizationLookup } from "./organization-lookup";
import { OrganizationWorkspace } from "./organization-workspace";
import "./organizations.css";

export const metadata: Metadata = {
  title: "Organizations — POST",
};

/**
 * /organizations (T0808, extended by T1304 / WEB-03, G03).
 *
 * Two things live on this page and they are deliberately separate:
 *
 *   - the PUBLIC lookup (T0808): there is no public directory, so a reader
 *     who has an organization's slug — from a paper, a project page, an
 *     affiliation — can open its public research profile;
 *   - the SIGNED-IN workspace (G03): the organizations the reader belongs
 *     to, plus the entry that creates one. Creating, inviting, changing
 *     roles and leaving are governed actions on the member page; this page
 *     only lists and creates.
 *
 * The public listing rule did not change with G03: GET /api/v1/organizations
 * still answers the signed-in actor's own organizations and says nothing to
 * an anonymous visitor. The workspace renders that answer; the lookup below
 * it is the public read path.
 */
export default function OrganizationsPage() {
  const cfg = getWebConfig();
  return (
    <div className="org-page">
      <div className="org-index org-index-head">
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
      </div>

      <OrganizationWorkspace apiBaseUrl={cfg.apiBaseUrl} />

      <div className="org-index org-index-lookup">
        <OrganizationLookup />
      </div>
    </div>
  );
}
