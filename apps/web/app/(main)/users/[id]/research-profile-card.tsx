"use client";

import { useEffect, useMemo, useState } from "react";
import { Flash, Heading, Link, Spinner, Text } from "@primer/react";

import {
  ResearchProfileError,
  createResearchProfileClient,
  researchProfileCodeKey,
  type PersonResearchProfile,
} from "../../../../lib/research-profile";
import {
  AffiliationList,
  AssetList,
  ContributionList,
  ReproductionList,
  ReuseList,
} from "../../../components/research-profile-sections";
import { useT } from "../../../i18n-provider";

/**
 * The Research Profile card (docs/42), rendered under the identity card on
 * /users/{id}.
 *
 * It is a SECOND read of the same person, not an extension of the identity
 * read: the identity card edits a profile (PATCH, CSRF, owner-only) and this
 * one is a public, anonymous, read-only research record. Keeping them apart is
 * what lets this card render for a signed-out visitor without the editing
 * form's session check running at all.
 *
 * The dimensions render as facts, in the API's order, with no total anywhere
 * (see research-profile-sections.tsx, which states the rule once).
 */
export function ResearchProfileCard({
  apiBaseUrl,
  userId,
}: {
  apiBaseUrl: string;
  userId: string;
}) {
  const t = useT();
  const client = useMemo(
    () => createResearchProfileClient(apiBaseUrl),
    [apiBaseUrl],
  );
  const [profile, setProfile] = useState<PersonResearchProfile | null>(null);
  const [loading, setLoading] = useState(true);
  const [notFound, setNotFound] = useState(false);
  const [loadError, setLoadError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    client
      .person(userId)
      .then((p) => {
        if (!cancelled) setProfile(p);
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        if (err instanceof ResearchProfileError && err.status === 404) {
          setNotFound(true);
        } else {
          setLoadError(
            t(researchProfileCodeKey(
              err instanceof ResearchProfileError ? err.code : "UNKNOWN",
            )),
          );
        }
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
    // `t` in the deps: the failure line resolves through it.
  }, [client, userId, t]);

  if (loading) {
    return (
      // Same shape as profile-card.tsx: the visible sentence announces the
      // loading state, the spinner's default srText would say it twice.
      <div className="rp-card rp-loading" role="status" aria-busy="true">
        <Spinner size="small" srText={null} />
        <Text>{t("rp.loading")}</Text>
      </div>
    );
  }

  if (notFound) {
    // A person whose account is disabled answers the same 404 as an unknown
    // id (cmd/api/researchprofilehttp), so this line covers both and must not
    // guess which one it is.
    return (
      <div className="rp-card">
        <Flash variant="default">{t(researchProfileCodeKey("USER_NOT_FOUND"))}</Flash>
      </div>
    );
  }

  if (profile === null || loadError !== null) {
    return (
      <div className="rp-card">
        <Flash variant="danger">{loadError ?? t("rp.error.load")}</Flash>
      </div>
    );
  }

  return (
    <div className="rp-card">
      <Heading as="h2" className="rp-title">
        {t("rp.title")}
      </Heading>

      {profile.person.bio !== "" && <p className="rp-bio">{profile.person.bio}</p>}

      <AffiliationList items={profile.affiliations} />
      <ContributionList
        items={profile.contributions}
        showActor={false}
        emptyKey="rp.empty.contributions"
      />
      <AssetList items={profile.assets} emptyKey="rp.empty.assets" />
      <ReuseList items={profile.reuse} />
      <ReproductionList items={profile.reproductions} />

      <section className="rp-section">
        <h2 className="rp-section-title">{t("rp.projects")}</h2>
        {profile.projects.length === 0 ? (
          <Text className="rp-empty">{t("rp.empty.projects")}</Text>
        ) : (
          <ul className="rp-list">
            {profile.projects.map((p) => (
              <li key={p.id} className="rp-row">
                <div className="rp-row-main">
                  <Link href={p.url} className="rp-entity">
                    {p.name}
                  </Link>
                </div>
              </li>
            ))}
          </ul>
        )}
      </section>

      <p className="rp-footnote">
        {t("rp.footnote")}
      </p>
    </div>
  );
}
