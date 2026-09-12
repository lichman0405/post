import type { Metadata } from "next";
import { ComingSoon } from "../../components/coming-soon";

export const metadata: Metadata = {
  title: "Organizations — POST",
};

/** Organization profiles land in T0808; listing is stubbed here. */
export default function OrganizationsPage() {
  return (
    <ComingSoon title="Organizations" milestone="T0808 Research Profile">
      Organization research profiles with members, projects and rights across
      the network.
    </ComingSoon>
  );
}
