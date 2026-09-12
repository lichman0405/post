import type { Metadata } from "next";
import { Suspense } from "react";
import { getWebConfig } from "../../../lib/server-config";
import { ProfileCard } from "./profile-card";

export const metadata: Metadata = {
  title: "Profile — POST",
};

/**
 * The public profile page: server component that resolves the validated
 * API origin and hands it — plus the id from the URL — to the client-side
 * card. The web app has no backend of its own; the card reads the public
 * profile over the wire (T0102) and shows the owner an edit form only when
 * the signed-in session is the profile's own (the API enforces the same
 * rule server-side; the form visibility is just UX).
 *
 * The URL is id-keyed by design: it never changes when the owner renames
 * the handle (docs/21 §2 — stable ids are the reference identity).
 */
export default async function ProfilePage({
  params,
}: {
  params: Promise<{ id: string }>;
}) {
  const cfg = getWebConfig();
  const { id } = await params;
  return (
    <main className="profile-main">
      <Suspense fallback={null}>
        <ProfileCard apiBaseUrl={cfg.apiBaseUrl} userId={id} />
      </Suspense>
    </main>
  );
}
