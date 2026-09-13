import { EyeClosedIcon } from "@primer/octicons-react";

import { TabPlaceholder } from "../tab-placeholder";

/** Files tab: the read-only Git/Blob view (docs/10 — the Files Web page
 *  is strictly read-only) lands with GitProvider integration. */
export default function FilesPage() {
  return (
    <>
      <TabPlaceholder title="Files" milestone="GitProvider integration (T0301)">
        The underlying Git and blob view of the repository files opens
        here.
      </TabPlaceholder>
      <p className="tab-placeholder-note">
        <span className="tab-placeholder-readonly">
          <EyeClosedIcon size={12} aria-hidden="true" /> Strictly read-only
        </span>{" "}
        — files mutate only through the scientific workflow, never through
        the web.
      </p>
    </>
  );
}
