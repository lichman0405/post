import { TabPlaceholder } from "../tab-placeholder";

/** Assets tab: published/dependent/derived research assets land with the
 *  release & asset hub milestones; the shell route is navigable today. */
export default function AssetsPage() {
  return (
    <TabPlaceholder title="Assets" milestone="the release & asset hub milestones">
      Research assets this project publishes, depends on or derives from
      list here.
    </TabPlaceholder>
  );
}
