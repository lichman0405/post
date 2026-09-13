import { TabPlaceholder } from "../tab-placeholder";

/** Activity tab: state transitions, audit and contribution events — the
 *  audit store and its query surface land with T0110 (in flight); this
 *  route is the shell's navigable anchor for them. */
export default function ActivityPage() {
  return (
    <TabPlaceholder title="Activity" milestone="T0110 基础 Audit Log">
      State transitions, governance actions and contribution events read
      here.
    </TabPlaceholder>
  );
}
