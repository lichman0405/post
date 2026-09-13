"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";

import { NAV_DESKTOP_DESTINATIONS } from "./nav-destinations";

/**
 * The desktop link row. A client component only because the current route
 * is needed to set aria-current="page" on the active destination (screen
 * readers announce the current page; the stylesheet weights it). Links
 * server-render inside the Suspense boundary in the layout.
 */
export function NavLinks() {
  const pathname = usePathname();
  return (
    <ul className="global-nav-list">
      {NAV_DESKTOP_DESTINATIONS.map(({ href, label, icon: Icon }) => {
        const current = pathname === href;
        return (
          <li key={href}>
            <Link
              href={href}
              className="global-nav-link"
              aria-current={current ? "page" : undefined}
            >
              <Icon size={16} aria-hidden />
              <span>{label}</span>
            </Link>
          </li>
        );
      })}
    </ul>
  );
}
