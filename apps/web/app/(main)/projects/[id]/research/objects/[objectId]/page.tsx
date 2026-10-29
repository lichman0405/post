import { firstParam, versionParamOf } from "../../research-data";
import { ObjectDetailView } from "../../object-detail-view";

/**
 * One object of the research outline (WEB-05 / G06).
 *
 * The URL carries no branch: `?version=` selects a version of THIS object,
 * and `from` / `fromVersion` record where the reader came from so the
 * return link restores that object at the version it was showing. The
 * branch coordinate is resolved by the view from the outline, so a link to
 * an old version never pins a branch the object has left.
 *
 * A `?version=` that is not a positive integer is passed through as the
 * "invalid" sentinel rather than dropped: the view refuses it visibly
 * instead of quietly answering with the latest version.
 */
export default async function ResearchObjectPage({
  params,
  searchParams,
}: {
  params: Promise<Record<string, string | string[] | undefined>>;
  searchParams: Promise<Record<string, string | string[] | undefined>>;
}) {
  const path = await params;
  const query = await searchParams;
  const objectId = firstParam(path.objectId);
  return (
    <ObjectDetailView
      objectId={objectId}
      version={versionParamOf(query.version)}
      from={firstParam(query.from)}
      fromVersion={versionParamOf(query.fromVersion)}
      branch={firstParam(query.branch)}
    />
  );
}
