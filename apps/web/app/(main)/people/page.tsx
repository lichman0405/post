import type { Metadata } from "next";
import { ComingSoon } from "../../components/coming-soon";

export const metadata: Metadata = {
  title: "People — POST",
};

/** Person research profiles land in T0808; listing is stubbed here. */
export default function PeoplePage() {
  return (
    <ComingSoon title="People" milestone="T0808 Research Profile">
      Person research profiles with contributions, credit ledger state and
      persistent identity.
    </ComingSoon>
  );
}
