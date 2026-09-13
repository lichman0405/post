"use client";

import { mayManageSettings } from "../../../../../lib/projects";
import { useProjectShell } from "../shell-context";
import { TabPlaceholder } from "../tab-placeholder";

/**
 * Settings tab (T0108): permission-aware at the content level, not just
 * the tab level. The tab bar hides Settings below maintainer, but the
 * route stays directly addressable — so this page re-checks the role the
 * shell fetched from the API and renders a not-authorized answer instead
 * of settings chrome for anyone else. The settings actions themselves
 * (member management, visibility preview, purpose/activity status) land
 * with T0109, whose API endpoints enforce the same rule server-side —
 * UI gating here is UX, never the security boundary.
 */
export default function SettingsPage() {
  const shell = useProjectShell();
  if (shell === null) {
    // The shell only mounts tab content in its ready state; null means a
    // wiring error, not a user-visible page.
    return null;
  }
  if (!mayManageSettings(shell.role)) {
    return (
      <div className="settings-unauthorized" data-settings-unauthorized>
        You do not have permission to manage this project&apos;s settings.
      </div>
    );
  }
  return (
    <TabPlaceholder title="Settings" milestone="T0109 Project Settings 与成员管理 UI">
      Members and roles, visibility preview, and the project purpose and
      activity status edit here.
    </TabPlaceholder>
  );
}
