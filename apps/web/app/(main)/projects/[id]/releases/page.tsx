import { TabPlaceholder } from "../tab-placeholder";

/** Releases tab: immutable release snapshots land with the release-hub
 *  milestones; the shell route is navigable today. */
export default function ReleasesPage() {
  return (
    <TabPlaceholder title="Releases" milestone="the release & asset hub milestones (T0606)">
      Immutable research snapshots — RSG hash, Git commit, blob hashes —
      list here.
    </TabPlaceholder>
  );
}
