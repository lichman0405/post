import Link from "next/link";

import { getT } from "../../lib/i18n-server";
import { NAV_DESKTOP_DESTINATIONS } from "./nav-destinations";

/**
 * Static fallback for the desktop link row: same destinations, no
 * aria-current (that needs the client router). Served in the server-rendered
 * HTML and swapped for <NavLinks> at hydration, so the navigation exists
 * even before client JS runs.
 *
 * Async since T1105: it is a server component (it is rendered as the
 * Suspense fallback on the server), so it reads the locale the same way the
 * other server components do rather than through the client context.
 */
export async function NavLinksStatic() {
  const { t } = await getT();
  return (
    <ul className="global-nav-list">
      {NAV_DESKTOP_DESTINATIONS.map(({ href, labelKey, icon: Icon }) => (
        <li key={href}>
          <Link href={href} className="global-nav-link">
            <Icon size={16} aria-hidden />
            <span>{t(labelKey)}</span>
          </Link>
        </li>
      ))}
    </ul>
  );
}
