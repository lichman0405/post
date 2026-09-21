import type { Metadata } from "next";
import { notFound } from "next/navigation";

import { getWebConfig } from "../../../../lib/server-config";
import { webOrigin } from "../../../../lib/origin";
import { hiddenPageMetadata } from "../../../../lib/entity-meta";
import {
  attestationFacts,
  attestationHref,
  messageForAttestationCode,
  readAttestation,
  validationResultTone,
  validationTypeLabel,
  WITHHELD_FACTS,
  type Attestation,
  type AttestationReadOutcome,
} from "../../../../lib/attestations";

/**
 * One attestation (T0812): the public page a private project's statement
 * creates.
 *
 * Server component, and that is the shape rather than a preference. Every
 * other entity page here is client-side because what it renders depends on
 * the READER's session — a member sees their own project's versions, a
 * visitor does not. This page has no such axis: an attestation is written
 * already public, and the command refuses to write one whose target is not
 * already readable by the network. So there is nothing to defer until the
 * session is known, and the document can be rendered on the server where a
 * crawler can index it and a reader with JavaScript disabled can still read
 * it. The read carries `cache: "no-store"` because an attestation is
 * append-only: the page never changes, but the *set* of them does, and a
 * cached 404 would outlive the record that made it wrong.
 *
 * # What this page can leak, and why it cannot
 *
 * The requirement is that a reader cannot reverse-engineer the private
 * project or the data behind the statement. Nothing in this file enforces
 * that, and that is on purpose: the projection happened in the API's read
 * (internal/application/attestations.Present), over a query that selects no
 * private column. There is no field here for the attesting project, the
 * basis state or the internal review, so there is no rendering rule to get
 * wrong. What this page adds is the opposite of a filter — it NAMES the
 * facts that are absent (WITHHELD_FACTS), so a reader learns that the
 * absence is the design rather than a gap, and it states in the document
 * itself that an attestation is not evidence.
 *
 * Nothing on this page is scored, weighted or ranked: the result is one of
 * three words (CLAUDE.md §9.13 — no Truth Score, no Research Score).
 */
export async function generateMetadata({
  params,
}: {
  params: Promise<{ pid: string }>;
}): Promise<Metadata> {
  const cfg = getWebConfig();
  const { pid } = await params;
  const result = await read(cfg.apiBaseUrl, pid);
  // The hidden head covers BOTH failures, and for the same reason: an
  // outage must not be indexed as a title either (a crawler that met a
  // 503 and a crawler that met a 404 must both learn nothing here).
  if (result.kind !== "found") return hiddenPageMetadata();
  const attestation = result.attestation;
  const origin = await webOrigin();
  const title = `${validationTypeLabel(attestation.validation_type)} — attestation — POST`;
  const description = `An attestation about “${attestation.target.title}”: ${validationTypeLabel(
    attestation.validation_type,
  )}, ${attestation.validation_result}. It is not evidence.`;
  return {
    title,
    description,
    alternates: { canonical: `${origin}${attestationHref(attestation.pid)}` },
    robots: { index: true, follow: true },
    openGraph: { type: "website", title, description, siteName: "POST" },
  };
}

export default async function AttestationPageRoute({
  params,
}: {
  params: Promise<{ pid: string }>;
}) {
  const cfg = getWebConfig();
  const { pid } = await params;
  const result = await read(cfg.apiBaseUrl, pid);
  if (result.kind === "missing") notFound();
  if (result.kind === "unavailable") return <AttestationUnavailable pid={pid} code={result.code} />;
  return <AttestationView attestation={result.attestation} />;
}

/**
 * read is the page's one call into the API: the read itself
 * (readAttestation, which owns the three-outcome rule and is unit-tested)
 * over a fetch that never caches.
 *
 * `cache: "no-store"` and not the framework default: an attestation is
 * append-only, so the page never changes, but the SET of them does, and a
 * cached 404 would outlive the record that made it wrong.
 */
function read(apiBaseUrl: string, pid: string): Promise<AttestationReadOutcome> {
  return readAttestation(apiBaseUrl, pid, (input, init) => fetch(input, { ...init, cache: "no-store" }));
}

/**
 * AttestationUnavailable renders the outage page.
 *
 * It is a page and not an error thrown into the framework's boundary, for
 * the same reason the record page is a server component: what it says has to
 * reach a reader without JavaScript and without a crawler being told the
 * record is gone. It states what it does NOT know — whether the attestation
 * exists — because that is the honest half of an outage, and it gives the
 * reader the one action available to them.
 */
function AttestationUnavailable({ pid, code }: { pid: string; code: string }) {
  return (
    <div className="att-main">
      <div className="att-header">
        <div className="att-header-body">
          <h1 className="att-title">Attestation</h1>
          <p className="att-subtitle">This page could not be loaded.</p>
        </div>
      </div>

      <section className="att-card">
        <h2 className="att-card-title">The record could not be read</h2>
        <p className="att-note">{messageForAttestationCode(code)}</p>
        <p className="att-note">
          This page does not know whether an attestation with this identifier exists: the service
          that answers for it could not be read, and no answer is not the answer &ldquo;there is no
          such attestation&rdquo;.{" "}
          <a className="att-link" href={attestationHref(pid)}>
            Reload this page
          </a>{" "}
          — a temporary outage answers normally once it has passed.
        </p>
      </section>
    </div>
  );
}

/**
 * AttestationView renders the record as it stands.
 *
 * The wrapper is a <div>, not a <main>: the (main) layout already renders
 * the document's one main landmark (`<main id="main" tabIndex={-1}>`, which
 * the skip link targets), and a second one inside it would leave the page
 * with two — the rule the layout's own header states, and the reason the
 * sibling panels are divs too.
 */
function AttestationView({ attestation }: { attestation: Attestation }) {
  const tone = validationResultTone(attestation.validation_result);
  const facts = attestationFacts(attestation);
  const organization = attestation.attributed_by.organization ?? null;
  const named = attestation.attributed_by.mode === "named" && organization !== null;

  return (
    <div className="att-main">
      <div className="att-header">
        <div className="att-header-body">
          <h1 className="att-title">Attestation</h1>
          <p className="att-subtitle">
            A statement that an organization validated a published version. It is not evidence.
          </p>
        </div>
        {tone === "" ? null : (
          <span className={`att-badge att-badge-${tone}`} data-att-result={attestation.validation_result}>
            {attestation.validation_result}
          </span>
        )}
      </div>

      <div className="att-pid-row">
        <span className="att-pid-label">Persistent identifier</span>
        <code className="att-pid">{attestation.pid}</code>
      </div>

      <section className="att-card">
        <h2 className="att-card-title">What it says</h2>
        <dl className="att-facts">
          {facts.map((fact) => (
            <div className="att-fact" key={fact.label}>
              <dt className="att-fact-label">{fact.label}</dt>
              <dd className="att-fact-value">{fact.value}</dd>
            </div>
          ))}
        </dl>
        <p className="att-attributed">
          {named ? (
            <>
              Attested by{" "}
              <a className="att-link" href={`/organizations/${encodeURIComponent(organization.slug)}`}>
                {organization.name}
              </a>
              .
            </>
          ) : (
            <>
              The organization that issued this statement is <strong>not named</strong>. That is the
              record&rsquo;s own mode, not a rendering choice — and this page does not report whether
              there is nobody to name or somebody who may not be.
            </>
          )}
        </p>
      </section>

      <section className="att-card">
        <h2 className="att-card-title">What it is about</h2>
        <p className="att-target-title">{attestation.target.title}</p>
        <dl className="att-facts">
          <div className="att-fact">
            <dt className="att-fact-label">Kind</dt>
            <dd className="att-fact-value">{attestation.target.kind}</dd>
          </div>
          <div className="att-fact">
            <dt className="att-fact-label">Version</dt>
            <dd className="att-fact-value">
              <code className="att-id">{attestation.target.version_id}</code>
            </dd>
          </div>
          <div className="att-fact">
            <dt className="att-fact-label">Object</dt>
            <dd className="att-fact-value">
              <code className="att-id">{attestation.target.object_id}</code>
            </dd>
          </div>
        </dl>
        <p className="att-note">
          This target is public, so naming it discloses nothing new — the attestation is a statement
          about a version the network could already read. The version is a <em>pin</em>: the
          statement keeps its meaning when the object moves on.
        </p>
      </section>

      <section className="att-card att-disclosure">
        <h2 className="att-card-title">This is not evidence</h2>
        <p className="att-statement">{attestation.disclosure.statement}</p>
        <ul className="att-claims">
          <li>
            <code className="att-id">is_evidence</code>{" "}
            <strong>{String(attestation.disclosure.is_evidence)}</strong>
          </li>
          <li>
            <code className="att-id">names_evidence</code>{" "}
            <strong>{String(attestation.disclosure.names_evidence)}</strong>
          </li>
        </ul>
        <p className="att-note">
          An evidence assertion names what it cites and says why. This record has neither field to
          carry, and the two are different things.
        </p>
      </section>

      <section className="att-card">
        <h2 className="att-card-title">What this page does not show</h2>
        <ul className="att-withheld">
          {WITHHELD_FACTS.map((item) => (
            <li key={item}>{item}</li>
          ))}
        </ul>
        <p className="att-note">
          These are not missing from this page — they are not on the record. The statement rests on
          work inside the issuing project, and that work stays there.
        </p>
      </section>
    </div>
  );
}
