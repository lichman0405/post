import type { Metadata } from "next";

import { getWebConfig } from "../../../lib/server-config";
import { getT } from "../../../lib/i18n-server";
import { ProjectDirectory } from "./project-directory";

/** T1105: the tab title is copy — it follows the language preference like the
 *  directory below it. */
export async function generateMetadata(): Promise<Metadata> {
  const { t } = await getT();
  return { title: t("projects.metaTitle") };
}

/**
 * The project directory (T0108): server component that resolves the
 * validated API origin and hands it to the client-side listing. The list
 * itself reads over the wire — the web app has no backend of its own.
 */
export default function ProjectsPage() {
  const cfg = getWebConfig();
  return <ProjectDirectory apiBaseUrl={cfg.apiBaseUrl} />;
}
