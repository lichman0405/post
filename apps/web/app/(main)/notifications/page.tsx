import type { Metadata } from "next";
import { ComingSoon } from "../../components/coming-soon";

export const metadata: Metadata = {
  title: "Notifications — POST",
};

/** The research inbox lands in T1003; the destination is stubbed for now. */
export default function NotificationsPage() {
  return (
    <ComingSoon title="Notifications" milestone="T1003 Web Research Inbox">
      Research events that need attention: reviews, state transitions,
      contributions and dependency warnings.
    </ComingSoon>
  );
}
