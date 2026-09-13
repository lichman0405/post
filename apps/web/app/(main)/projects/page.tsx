import type { Metadata } from "next";

import { getWebConfig } from "../../../lib/server-config";
import { ProjectDirectory } from "./project-directory";

export const metadata: Metadata = {
  title: "Projects — POST",
};

/**
 * The project directory (T0108): server component that resolves the
 * validated API origin and hands it to the client-side listing. The list
 * itself reads over the wire — the web app has no backend of its own.
 */
export default function ProjectsPage() {
  const cfg = getWebConfig();
  return <ProjectDirectory apiBaseUrl={cfg.apiBaseUrl} />;
}
