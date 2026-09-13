"use client";

import { useEffect, useRef, useState } from "react";
import Link from "next/link";
import { usePathname } from "next/navigation";
import { ThreeBarsIcon, SignInIcon } from "@primer/octicons-react";

import { NAV_DRAWER_DESTINATIONS } from "./nav-destinations";
import { useNavSession } from "./nav-session";

const DRAWER_ID = "global-nav-drawer";

/**
 * The narrow-screen navigation (docs/06 §3: mobile keeps every destination
 * reachable). A real <button> discloses the drawer: Enter/Space open it and
 * move focus to the first item, Tab walks the links, Escape closes and
 * returns focus to the toggle, clicking outside closes. aria-expanded
 * announces the state; aria-controls is only set while the panel exists.
 */
export function MobileNavMenu() {
  const pathname = usePathname();
  const { user, loading, busy, signOut } = useNavSession();
  const [open, setOpen] = useState(false);
  const toggleRef = useRef<HTMLButtonElement>(null);
  const panelRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!open) return;
    // Move focus into the drawer so keyboard users land on the first item
    // instead of tabbing through the whole header again.
    const first = panelRef.current?.querySelector<HTMLElement>("a, button");
    first?.focus();
    function onPointerDown(event: PointerEvent) {
      const target = event.target as Node;
      if (
        toggleRef.current?.contains(target) === false &&
        panelRef.current?.contains(target) === false
      ) {
        setOpen(false);
      }
    }
    document.addEventListener("pointerdown", onPointerDown);
    return () => document.removeEventListener("pointerdown", onPointerDown);
  }, [open]);

  function onToggleKeyDown(event: React.KeyboardEvent) {
    if (event.key === "Escape" && open) {
      setOpen(false);
      toggleRef.current?.focus();
    }
  }

  function onPanelKeyDown(event: React.KeyboardEvent) {
    if (event.key === "Escape") {
      setOpen(false);
      toggleRef.current?.focus();
    }
  }

  return (
    <div className="global-nav-mobile">
      <button
        ref={toggleRef}
        type="button"
        className="global-nav-icon global-nav-mobile-toggle"
        aria-label="Global navigation menu"
        aria-expanded={open}
        aria-controls={open ? DRAWER_ID : undefined}
        onClick={() => setOpen((value) => !value)}
        onKeyDown={onToggleKeyDown}
      >
        <ThreeBarsIcon size={16} aria-hidden />
      </button>
      {open && (
        <div
          ref={panelRef}
          id={DRAWER_ID}
          className="global-nav-drawer"
          onKeyDown={onPanelKeyDown}
        >
          <ul className="global-nav-drawer-list">
            {NAV_DRAWER_DESTINATIONS.map(({ href, label, icon: Icon }) => {
              const current = pathname === href;
              return (
                <li key={href}>
                  <Link
                    href={href}
                    className="global-nav-drawer-link"
                    aria-current={current ? "page" : undefined}
                  >
                    <Icon size={16} aria-hidden />
                    <span>{label}</span>
                  </Link>
                </li>
              );
            })}
          </ul>
          <div className="global-nav-drawer-divider" role="separator" />
          {loading ? (
            <p className="global-nav-drawer-session">Checking session…</p>
          ) : user === null ? (
            <Link href="/login" className="global-nav-drawer-link">
              <SignInIcon size={16} aria-hidden />
              <span>Sign in</span>
            </Link>
          ) : (
            <>
              <p className="global-nav-drawer-session">
                Signed in as <strong>{user.display_name || user.handle}</strong>
              </p>
              <div className="global-nav-drawer-actions">
                <Link href={`/users/${user.id}`} className="global-nav-drawer-link">
                  Your profile
                </Link>
                <button
                  type="button"
                  className="global-nav-drawer-link"
                  disabled={busy}
                  onClick={() => signOut()}
                >
                  {busy ? "Signing out…" : "Sign out"}
                </button>
              </div>
            </>
          )}
        </div>
      )}
    </div>
  );
}
