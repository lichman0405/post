"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import { BellIcon } from "@primer/octicons-react";

/**
 * The notifications bell (client): the desktop affordance for the
 * Notifications destination (docs/05 §1 — the destination appears once in
 * the header, here, not as a second entry in the text row). Marks itself
 * aria-current="page" on /notifications, exactly like the row links do.
 */
export function NavBell() {
  const pathname = usePathname();
  return (
    <Link
      href="/notifications"
      className="global-nav-icon"
      aria-label="Notifications"
      aria-current={pathname === "/notifications" ? "page" : undefined}
    >
      <BellIcon size={16} aria-hidden />
    </Link>
  );
}

/** The pre-hydration fallback: the same link, no active marker yet. */
export function NavBellFallback() {
  return (
    <Link
      href="/notifications"
      className="global-nav-icon"
      aria-label="Notifications"
    >
      <BellIcon size={16} aria-hidden />
    </Link>
  );
}
