import Link from "next/link";

import { NAV_DESKTOP_DESTINATIONS } from "./nav-destinations";

/**
 * Static fallback for the desktop link row: same destinations, no
 * aria-current (that needs the client router). Served in the prerendered
 * HTML of static pages and swapped for <NavLinks> at hydration, so the
 * navigation exists even before client JS runs.
 */
export function NavLinksStatic() {
  return (
    <ul className="global-nav-list">
      {NAV_DESKTOP_DESTINATIONS.map(({ href, label, icon: Icon }) => (
        <li key={href}>
          <Link href={href} className="global-nav-link">
            <Icon size={16} aria-hidden />
            <span>{label}</span>
          </Link>
        </li>
      ))}
    </ul>
  );
}
