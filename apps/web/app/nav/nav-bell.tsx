"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import { BellIcon } from "@primer/octicons-react";

import { useT } from "../i18n-provider";

/**
 * The notifications bell (client): the desktop affordance for the
 * Notifications destination (docs/05 §1 — the destination appears once in
 * the header, here, not as a second entry in the text row). Marks itself
 * aria-current="page" on /notifications, exactly like the row links do.
 */
export function NavBell() {
  const pathname = usePathname();
  const t = useT();
  return (
    <Link
      href="/notifications"
      className="global-nav-icon"
      aria-label={t("nav.notifications")}
      aria-current={pathname === "/notifications" ? "page" : undefined}
    >
      <BellIcon size={16} aria-hidden />
    </Link>
  );
}

/** The pre-hydration fallback: the same link, no active marker yet.
 *
 *  T1105: it takes the label as a PROP instead of translating it itself.
 *  This module is a client module ("use client" is above), so it cannot
 *  import the server-side locale reader — the build rejects that outright
 *  ("next/headers … only available in Server Components"). The fallback is
 *  rendered on the server by app/nav/global-nav.tsx, which is a server
 *  component and therefore already holds the localized label; passing it
 *  down keeps this file free of any locale lookup at all. */
export function NavBellFallback({ label }: { label: string }) {
  return (
    <Link
      href="/notifications"
      className="global-nav-icon"
      aria-label={label}
    >
      <BellIcon size={16} aria-hidden />
    </Link>
  );
}
