import type { Metadata } from "next";
import { ComingSoon } from "../../components/coming-soon";

export const metadata: Metadata = {
  title: "Explore — POST",
};

/** Explore 聚合 lands in T0802; the nav destination is stubbed for now. */
export default function ExplorePage() {
  return (
    <ComingSoon title="Explore" milestone="T0802 Explore 聚合">
      Aggregated discovery across projects, assets, knowledge objects, people
      and organizations, ranked by freshness, reuse and relevance.
    </ComingSoon>
  );
}
