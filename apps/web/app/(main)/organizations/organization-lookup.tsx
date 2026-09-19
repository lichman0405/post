"use client";

import { useState, type FormEvent } from "react";
import { useRouter } from "next/navigation";
import { Button, FormControl, TextInput } from "@primer/react";

/**
 * The organization lookup (T0808): one slug field, one navigation.
 *
 * There is no public organization DIRECTORY to list here, and this component
 * says so instead of pretending otherwise: GET /api/v1/organizations is a
 * member-only read (cmd/api/orgshttp — it answers 401 to an anonymous
 * caller), and building a public one would be a visibility decision no
 * specification has made. What IS public is one organization's research
 * profile at /organizations/{slug}, so that is what this page offers: the
 * slug is the institution's public identity, and a visitor who has it — from
 * a paper, a project page, an affiliation on someone's profile — can reach
 * the page.
 *
 * The slug is passed through unchanged; normalization (trim + lowercase) is
 * the API's (domain.NormalizeOrgSlug), and a second copy of that rule in the
 * browser is a second answer waiting to disagree.
 */
export function OrganizationLookup() {
  const router = useRouter();
  const [slug, setSlug] = useState("");

  const trimmed = slug.trim();

  function submit(event: FormEvent) {
    event.preventDefault();
    if (trimmed === "") return;
    router.push(`/organizations/${encodeURIComponent(trimmed)}`);
  }

  return (
    <form className="org-lookup" onSubmit={submit}>
      <FormControl>
        <FormControl.Label>Organization slug</FormControl.Label>
        <TextInput
          value={slug}
          onChange={(event) => setSlug(event.target.value)}
          placeholder="institute-of-catalysis"
          maxLength={64}
          block
        />
        <FormControl.Caption>
          The slug is the organization&apos;s public address; it appears in its
          profile URL and on the affiliations of its members.
        </FormControl.Caption>
      </FormControl>
      <div className="org-lookup-actions">
        <Button type="submit" variant="primary" disabled={trimmed === ""}>
          Open profile
        </Button>
      </div>
    </form>
  );
}
