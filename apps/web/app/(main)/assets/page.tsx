import type { Metadata } from "next";
import { ComingSoon } from "../../components/coming-soon";

export const metadata: Metadata = {
  title: "Assets — POST",
};

/** The asset hub lands in T0709; the destination is stubbed for now. */
export default function AssetsPage() {
  return (
    <ComingSoon title="Assets" milestone="T0709 Asset Hub Pages/Explore">
      Immutable research assets with persistent identity, rights, lineage and
      current network state.
    </ComingSoon>
  );
}
