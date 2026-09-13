import { getWebConfig } from "../../../../lib/server-config";
import { ProjectShell } from "./project-shell";

/**
 * The project shell layout (T0108): every /projects/{id} route renders
 * inside the shell — project header (name, purpose, visibility + frozen
 * badges) and the tab bar (docs/05 §3). The shell fetches the project and
 * the caller's own membership over the wire; when the API answers the
 * existence-hiding 404 (a private project the caller is not authorized
 * for), the shell renders NO chrome — just the not-found state — and the
 * tab content below never mounts.
 */
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
