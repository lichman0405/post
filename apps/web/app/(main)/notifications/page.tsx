import type { Metadata } from "next";
import { getWebConfig } from "../../../lib/server-config";
import { InboxSurface } from "./inbox-surface";

export const metadata: Metadata = {
  title: "Notifications — POST",
  description:
    "The research inbox: what happened on the projects, assets and people you follow.",
};

/**
 * The research inbox (T1003).
 *
 * A server component resolves the validated API origin and hands it to the
 * client surface, because the read is session-scoped: the session cookie
 * lives in the browser, so the browser is what calls GET /api/v1/inbox
 * (credentials: include) — the same shape the profile card and the project
 * tabs use. Nothing here is public, so there is no server-rendered copy
 * that could leak between users.
 */
export default function NotificationsPage() {
  const cfg = getWebConfig();
  return <InboxSurface apiBaseUrl={cfg.apiBaseUrl} />;
}
