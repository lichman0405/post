import type { Icon } from "@primer/octicons-react";
import {
  BellIcon,
  HomeIcon,
  OrganizationIcon,
  PackageIcon,
  PeopleIcon,
  ProjectIcon,
  SearchIcon,
  TelescopeIcon,
} from "@primer/octicons-react";

/**
 * The global navigation destinations (docs/05 §1): the one list shared by
 * the desktop link row and the mobile drawer, so the two can never drift.
 * Search is the header form on desktop and a drawer entry on narrow screens.
 */
/**
 * T1105: `label` became `labelKey`.
 *
 * The eight labels are user-visible copy and docs/28 §3 puts copy in the
 * catalog, not in a data table. Carrying a catalog KEY rather than a
 * translated string is what keeps this module a plain `.ts` table — it
 * resolves no locale, imports no React and renders nothing, so both the
 * desktop row and the drawer translate it at their own render site.
 *
 * Every one of the eight is on every core page, which is why this table is
 * in scope: a scanner that only looked at JSX text could not see a one-word
 * label living in a `.ts` file, and the header would have stayed English
 * while the pages below it switched.
 */
export interface NavDestination {
  href: string;
  labelKey: string;
  icon: Icon;
}

export const NAV_DESTINATIONS: NavDestination[] = [
  { href: "/", labelKey: "nav.home", icon: HomeIcon },
  { href: "/explore", labelKey: "nav.explore", icon: TelescopeIcon },
  { href: "/search", labelKey: "nav.search", icon: SearchIcon },
  { href: "/projects", labelKey: "nav.projects", icon: ProjectIcon },
  { href: "/assets", labelKey: "nav.assets", icon: PackageIcon },
  { href: "/people", labelKey: "nav.people", icon: PeopleIcon },
  { href: "/organizations", labelKey: "nav.organizations", icon: OrganizationIcon },
  { href: "/notifications", labelKey: "nav.notifications", icon: BellIcon },
];

/** The destinations that render as a text row on desktop (Search is the
    header form and Notifications is the bell shortcut there — docs/05 §1
    lists each destination once; the drawer keeps both as links).
    Selected by href, not by label: the label is a catalog key now, and
    filtering a rendered string is what made the old version depend on the
    copy staying in English. */
export const NAV_DESKTOP_DESTINATIONS = NAV_DESTINATIONS.filter(
  (d) => d.href !== "/search" && d.href !== "/notifications",
);

/** The drawer order: Search first (it has no header form on narrow screens),
    then the remaining destinations in their canonical order — all eight,
    including Notifications, so every destination stays reachable on narrow
    screens. */
const SEARCH_DESTINATION = NAV_DESTINATIONS.find((d) => d.href === "/search");
if (SEARCH_DESTINATION === undefined) {
  // The drawer layout depends on Search being a known destination; fail the
  // module load instead of spreading undefined into the link list.
  throw new Error("NAV_DESTINATIONS must contain the /search destination");
}
export const NAV_DRAWER_DESTINATIONS: NavDestination[] = [
  SEARCH_DESTINATION,
  ...NAV_DESTINATIONS.filter((d) => d.href !== "/search"),
];
