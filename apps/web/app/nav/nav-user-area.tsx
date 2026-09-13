"use client";

import Link from "next/link";
import { ActionList, ActionMenu } from "@primer/react";
import { PersonIcon, TriangleDownIcon } from "@primer/octicons-react";

import { useNavSession } from "./nav-session";

/**
 * The header's account area (client): a "Sign in" link when there is no
 * session, and a handle + caret menu with profile / sign-out when there is.
 * The session comes from the shared NavSessionProvider — no fetch here.
 */
export function NavUserArea() {
  const { user, loading, busy, signOut } = useNavSession();

  if (loading) {
    // Reserve the trigger's footprint so the header does not shift when
    // the session resolves.
    return <span className="global-nav-icon" aria-hidden="true" />;
  }

  if (user === null) {
    return (
      <Link href="/login" className="global-nav-signin">
        Sign in
      </Link>
    );
  }

  return (
    <ActionMenu>
      <ActionMenu.Anchor aria-label={`Account menu for ${user.handle}`}>
        <span className="global-nav-user">
          <PersonIcon size={16} aria-hidden />
          <span className="global-nav-user-handle">{user.handle}</span>
          <TriangleDownIcon size={12} aria-hidden />
        </span>
      </ActionMenu.Anchor>
      <ActionMenu.Overlay align="end" width="medium">
        <ActionList>
          <ActionList.Item disabled>
            Signed in as {user.display_name || user.handle}
          </ActionList.Item>
          <ActionList.LinkItem href={`/users/${user.id}`}>
            Your profile
          </ActionList.LinkItem>
          <ActionList.Divider />
          <ActionList.Item variant="danger" onSelect={() => signOut()}>
            {busy ? "Signing out…" : "Sign out"}
          </ActionList.Item>
        </ActionList>
      </ActionMenu.Overlay>
    </ActionMenu>
  );
}
