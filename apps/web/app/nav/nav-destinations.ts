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
export interface NavDestination {
  href: string;
  label: string;
  icon: Icon;
}

export const NAV_DESTINATIONS: NavDestination[] = [
  { href: "/", label: "Home", icon: HomeIcon },
  { href: "/explore", label: "Explore", icon: TelescopeIcon },
  { href: "/search", label: "Search", icon: SearchIcon },
  { href: "/projects", label: "Projects", icon: ProjectIcon },
  { href: "/assets", label: "Assets", icon: PackageIcon },
  { href: "/people", label: "People", icon: PeopleIcon },
  { href: "/organizations", label: "Organizations", icon: OrganizationIcon },
  { href: "/notifications", label: "Notifications", icon: BellIcon },
];

/** The destinations that render as a text row on desktop (Search is the
    header form and Notifications is the bell shortcut there — docs/05 §1
    lists each destination once; the drawer keeps both as links). */
export const NAV_DESKTOP_DESTINATIONS = NAV_DESTINATIONS.filter(
  (d) => d.label !== "Search" && d.label !== "Notifications",
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
  ...NAV_DESTINATIONS.filter((d) => d.label !== "Search"),
];
