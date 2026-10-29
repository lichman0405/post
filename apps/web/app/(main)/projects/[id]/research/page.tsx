import { firstParam, researchViewOf } from "./research-data";
import { ResearchWorkspace } from "./research-workspace";

/**
 * The Research tab's entry (WEB-05 / G06).
 *
 * This file stays a SERVER component so the address bar is the page's
 * state: `?view=` chooses the outline axis and `?focus=` keeps the object a
 * reader came back from marked in the outline. Both are plain strings read
 * from the request — no client-side search-param suspension, and the page
 * renders the same picture on a hard reload as after a click.
 *
 * Every string the workspace renders lives in the catalog (the file is in
 * the hard-coded-string gate's core group); this wrapper renders none.
 */
export default async function ResearchPage({
  searchParams,
}: {
  searchParams: Promise<Record<string, string | string[] | undefined>>;
}) {
  const params = await searchParams;
  return (
    <ResearchWorkspace
      view={researchViewOf(params.view)}
      focus={firstParam(params.focus)}
      branch={firstParam(params.branch)}
    />
  );
}
