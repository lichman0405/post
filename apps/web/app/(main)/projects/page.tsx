import type { Metadata } from "next";
import { ComingSoon } from "../../components/coming-soon";

export const metadata: Metadata = {
  title: "Projects — POST",
};

/** Project pages land in T0108 (shell + tabs); listing is stubbed here. */
export default function ProjectsPage() {
  return (
    <ComingSoon title="Projects" milestone="T0108 Project Shell 与 tabs">
      The research project directory with visibility state, frozen-main
      status and current research questions per project.
    </ComingSoon>
  );
}
