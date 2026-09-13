import { TabPlaceholder } from "../tab-placeholder";

/** Pull requests tab: research PRs (proposed RSG diffs + review) land
 *  with the research-PR milestones; the shell route is navigable today. */
export default function PullsPage() {
  return (
    <TabPlaceholder title="Pull requests" milestone="the research-PR milestones (T0205)">
      Proposed research-state diffs and their scientific reviews organize
      here.
    </TabPlaceholder>
  );
}
