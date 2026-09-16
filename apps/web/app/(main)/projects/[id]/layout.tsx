import type { Metadata } from "next";

import { getWebConfig } from "../../../../lib/server-config";
import { webOrigin } from "../../../../lib/origin";
import {
  fetchPublicProjectMeta,
  hiddenPageMetadata,
  publicEntityPath,
  publicPageMetadata,
} from "../../../../lib/entity-meta";
import { ProjectShell } from "./project-shell";

/**
 * The project shell layout (T0108): every /projects/{id} route renders
 * inside the shell — project header (name, purpose, visibility + frozen
 * badges) and the tab bar (docs/05 §3). The shell fetches the project and
 * the caller's own membership over the wire; when the API answers the
 * existence-hiding 404 (a private project the caller is not authorized
 * for), the shell renders NO chrome — just the not-found state — and the
 * tab content below never mounts.
 *
 * T0801 adds the <head> the crawler reads: the project's name and purpose
 * for a PUBLIC project (docs/51 §"Public pages 服务端输出可索引内容"), and
 * the unindexable, name-free head for everything else. The metadata read
 * is anonymous, so it answers for the entity and not for the reader; see
 * lib/entity-meta.ts for why the pages themselves stay 200 either way.
 */
export async function generateMetadata({
  params,
}: {
  params: Promise<{ id: string }>;
}): Promise<Metadata> {
  const cfg = getWebConfig();
  const { id } = await params;
  const entity = await fetchPublicProjectMeta(cfg.apiBaseUrl, id);
  if (entity === null) return hiddenPageMetadata();
  const origin = await webOrigin();
  return publicPageMetadata(entity, `${origin}${publicEntityPath("project", id)}`);
}

export default async function ProjectShellLayout({
  params,
  children,
}: {
  params: Promise<{ id: string }>;
  children: React.ReactNode;
}) {
  const cfg = getWebConfig();
  const { id } = await params;
  return (
    <ProjectShell apiBaseUrl={cfg.apiBaseUrl} projectId={id}>
      {children}
    </ProjectShell>
  );
}
