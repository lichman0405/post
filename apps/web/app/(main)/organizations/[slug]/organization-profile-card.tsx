"use client";

import { useEffect, useMemo, useState } from "react";
import { Flash, Heading, Link, Spinner, Text } from "@primer/react";

import {
  ResearchProfileError,
  createResearchProfileClient,
  messageForResearchProfileCode,
  type OrganizationResearchProfile,
} from "../../../../lib/research-profile";
import {
  AssetList,
  ContributionList,
} from "../../../components/research-profile-sections";

/**
 * The Organization Research Profile (docs/05 §4: an institution's public
 * research activity) at /organizations/{slug}.
 *
 * The slug IS the organization's public identity — it is the stable, human
 * addressable name, which is why this route is slug-keyed while the person
 * route is id-keyed (a person's URL must survive a handle rename; an
 * organization's name is not renamable, cmd/api/orgshttp).
 *
 * A deactivated organization answers the same 404 as an unknown slug, so the
 * not-found state is one line that does not guess which it was — "this
 * institution closed" is itself information no rule lets this page state.
 */
export function OrganizationProfileCard({
  apiBaseUrl,
  slug,
}: {
  apiBaseUrl: string;
  slug: string;
}) {
  const client = useMemo(
    () => createResearchProfileClient(apiBaseUrl),
    [apiBaseUrl],
  );
  const [profile, setProfile] = useState<OrganizationResearchProfile | null>(null);
  const [loading, setLoading] = useState(true);
  const [notFound, setNotFound] = useState(false);
  const [loadError, setLoadError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    client
      .organization(slug)
      .then((p) => {
        if (!cancelled) setProfile(p);
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        if (err instanceof ResearchProfileError && err.status === 404) {
          setNotFound(true);
        } else {
          setLoadError(
            messageForResearchProfileCode(
              err instanceof ResearchProfileError ? err.code : "UNKNOWN",
            ),
          );
        }
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [client, slug]);

  if (loading) {
    return (
      // Same shape as profile-card.tsx: the visible sentence announces the
      // loading state, the spinner's default srText would say it twice.
      <div className="rp-card rp-loading" role="status" aria-busy="true">
        <Spinner size="small" srText={null} />
        <Text>Loading organization profile…</Text>
      </div>
    );
  }

  if (notFound) {
    return (
      <div className="rp-card">
        <Flash variant="default">{messageForResearchProfileCode("ORG_NOT_FOUND")}</Flash>
      </div>
    );
  }

  if (profile === null || loadError !== null) {
    return (
      <div className="rp-card">
        <Flash variant="danger">{loadError ?? "The organization profile could not be loaded."}</Flash>
      </div>
    );
  }

  return (
    <div className="rp-card">
      <Heading as="h1" className="rp-title">
        {profile.organization.name}
      </Heading>
      <Text className="rp-handle">@{profile.organization.slug}</Text>
      {profile.organization.description !== "" && (
        <p className="rp-bio">{profile.organization.description}</p>
      )}

      <section className="rp-section">
        <h2 className="rp-section-title">Projects</h2>
        {profile.projects.length === 0 ? (
          <Text className="rp-empty">No public project yet.</Text>
        ) : (
          <ul className="rp-list">
            {profile.projects.map((p) => (
              <li key={p.id} className="rp-row">
                <div className="rp-row-main">
                  <Link href={p.url} className="rp-entity">
                    {p.name}
                  </Link>
                  <span className="rp-meta">
                    {p.activity_status} · {p.purpose}
                  </span>
                </div>
              </li>
            ))}
          </ul>
        )}
      </section>

      {/* What the institution's affiliated people did while they were
          affiliated. A row keeps naming the organization after the
          affiliation that produced it has ended — 离职后个人历史保留 — so this
          list is the institution's record and does not shrink when someone
          leaves. */}
      <ContributionList
        items={profile.activity}
        showActor
        empty="No public activity recorded yet."
      />
      <AssetList
        items={profile.assets}
        empty="No published assets from this organization's public projects yet."
      />

      <p className="rp-footnote">
        Activity, projects and assets are recorded facts. POST does not
        compute a score, rank or rating for an organization.
      </p>
    </div>
  );
}
