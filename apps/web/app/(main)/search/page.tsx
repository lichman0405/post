import type { Metadata } from "next";
import { ComingSoon } from "../../components/coming-soon";

export const metadata: Metadata = {
  title: "Search — POST",
};

/**
 * Search receives the global header form (GET /search?q=). The
 * evidence-backed answer UI lands in T0907; until then the page confirms
 * the query it received so the header wiring is visible end to end.
 */
export default async function SearchPage({
  searchParams,
}: {
  searchParams: Promise<{ q?: string }>;
}) {
  const { q } = await searchParams;
  return (
    <ComingSoon title="Search" milestone="T0907 Search Answer Web UI">
      {q !== undefined && q !== "" ? (
        <>
          Received query <q>{q}</q>.{" "}
        </>
      ) : null}
      Full-text and vector search over research objects, with evidence-backed
      answers above the result list.
    </ComingSoon>
  );
}
