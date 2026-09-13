import { TabPlaceholder } from "../tab-placeholder";

/** Issues tab: workflow items land with the workflow milestones; the
 *  shell route is navigable today. */
export default function IssuesPage() {
  return (
    <TabPlaceholder title="Issues" milestone="the workflow milestones">
      Research workflow items — reviews, integrity checks, open
      contributions — organize here.
    </TabPlaceholder>
  );
}
