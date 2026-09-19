"use client";

import { Link, Text } from "@primer/react";

import {
  describeRelation,
  describeVia,
  formatAffiliationWindow,
  formatDay,
  printableEntity,
  type ProfileAffiliation,
  type ProfileAsset,
  type ProfileContribution,
  type ProfileReproduction,
  type ProfileReuse,
} from "../../lib/research-profile";

/**
 * The shared rendering of the Research Profile's dimensions (T0808), used by
 * BOTH the person card and the organization card. docs/42 lists the content;
 * these are the rows.
 *
 * Three rules are visible in this file and each one is a product rule rather
 * than a style choice:
 *
 *  1. NOTHING IS TOTALLED. No component here renders, or has anywhere to
 *     render, a count of the rows it was given. A dimension that is empty
 *     says "Nothing recorded yet." and does not say "0" — docs/13 §4 forbids
 *     a single score and docs/13 §6 makes raw counts quality proxies,
 *     CLAUDE.md §9 invariant 13 forbids a Truth Score. A badge reading "42
 *     contributions" is the first thing that would become one.
 *
 *  2. A WITHHELD IDENTITY IS NOT A MISSING ONE. Where the API sends
 *     `project: null` or `organization: null` (a private origin project, a
 *     private using project, a deactivated organization) these components
 *     render the row WITHOUT the entity and say nothing about why. The
 *     alternative — "project hidden" — would tell a reader that a project
 *     exists and is being hidden, which is the disclosure the null exists to
 *     prevent. The decision is not made here four times over: every row asks
 *     `printableEntity` (lib/research-profile.ts) for the entity to print or
 *     for null, and renders nothing at all when it gets null — which is what
 *     makes the rule testable in the node:test module suite, where this file
 *     cannot be tested.
 *
 *  3. NO ORDERING CLAIM. The rows arrive in the API's order and are rendered
 *     in it; nothing here sorts, ranks or annotates a row with a judgement.
 *     A failed reproduction is listed like a successful one (docs/10 §4: "V1
 *     不自动赋数值权重").
 */

/** Section is one labelled dimension: a heading, its rows, and the honest
 *  empty line. It renders no count (rule 1 above). */
function Section({
  title,
  items,
  empty,
  render,
}: {
  title: string;
  items: unknown[];
  empty: string;
  render: (index: number) => React.ReactNode;
}) {
  return (
    <section className="rp-section">
      <h2 className="rp-section-title">{title}</h2>
      {items.length === 0 ? (
        <Text className="rp-empty">{empty}</Text>
      ) : (
        <ul className="rp-list">{items.map((_, i) => render(i))}</ul>
      )}
    </section>
  );
}

/** EntityLink renders a link when the identity may be named, and a plain
 *  span when it may not (rule 2 above). */
function EntityLink({ url, label }: { url: string | null; label: string }) {
  if (url === null || url === "") return <span className="rp-entity">{label}</span>;
  return (
    <Link href={url} className="rp-entity">
      {label}
    </Link>
  );
}

function Roles({ codes }: { codes: string[] }) {
  if (codes.length === 0) return null;
  return <span className="rp-roles">{codes.join(", ")}</span>;
}

export function AffiliationList({ items }: { items: ProfileAffiliation[] }) {
  return (
    <Section
      title="Affiliations"
      items={items}
      empty="No affiliation recorded."
      render={(i) => {
        const a = items[i];
        // A withheld employer is not a missing one (rule 2): the row keeps
        // everything that is the person's own — role, both dates, verification
        // — and prints no label at all where the name would go. It must not say
        // why, and it must not stand a placeholder in for the name: the model
        // withholds an affiliation's organization exactly when the employer is
        // deactivated, so any sentence there would be the disclosure the null
        // exists to prevent. `printableEntity` owns that decision, and the
        // module test pins it.
        const organization = printableEntity(a.organization);
        return (
          <li key={i} className="rp-row">
            <div className="rp-row-main">
              {organization !== null && (
                <EntityLink url={organization.url} label={organization.label} />
              )}
              <span className="rp-meta">
                {a.role} · {formatAffiliationWindow(a.affiliation_start, a.affiliation_end)}
                {a.verified ? " · verified" : ""}
              </span>
            </div>
          </li>
        );
      }}
    />
  );
}

export function ContributionList({
  items,
  showActor,
  empty,
}: {
  items: ProfileContribution[];
  showActor: boolean;
  empty: string;
}) {
  return (
    <Section
      title="Contributions"
      items={items}
      empty={empty}
      render={(i) => {
        const c = items[i];
        const project = printableEntity(c.project);
        return (
          <li key={i} className="rp-row">
            <div className="rp-row-main">
              <span className="rp-event">{c.event_type}</span>
              {showActor && c.actor !== undefined && (
                <>
                  {" "}
                  <EntityLink url={c.actor.url} label={c.actor.display_name || c.actor.handle} />
                </>
              )}
              <span className="rp-meta">
                {project !== null && (
                  <>
                    <EntityLink url={project.url} label={project.label} /> ·{" "}
                  </>
                )}
                {formatDay(c.occurred_at)} · via {describeVia(c.via)}
                {c.accepted_context ? " · accepted" : ""}
                {c.released_context ? " · released" : ""}
              </span>
              <Roles codes={c.role_codes} />
            </div>
          </li>
        );
      }}
    />
  );
}

export function AssetList({ items, empty }: { items: ProfileAsset[]; empty: string }) {
  return (
    <Section
      title="Assets"
      items={items}
      empty={empty}
      render={(i) => {
        const a = items[i];
        const origin = printableEntity(a.project);
        return (
          <li key={i} className="rp-row">
            <div className="rp-row-main">
              <Link href={a.url} className="rp-entity">
                {a.title}
              </Link>
              <span className="rp-meta">
                {a.asset_type} · {a.pid}@{a.version}
                {a.role !== undefined ? ` · ${a.role}` : ""} · {formatDay(a.published_at)}
                {/* The origin project, when it may be named. A private
                    project's public version travels without it (docs/12 §2),
                    and then nothing is printed in its place (rule 2). */}
                {origin !== null && (
                  <>
                    {" · "}
                    <EntityLink url={origin.url} label={origin.label} />
                  </>
                )}
              </span>
            </div>
          </li>
        );
      }}
    />
  );
}

export function ReuseList({ items }: { items: ProfileReuse[] }) {
  return (
    <Section
      title="Reuse"
      items={items}
      empty="No recorded reuse of this work yet."
      render={(i) => {
        const r = items[i];
        return (
          <li key={i} className="rp-row">
            <div className="rp-row-main">
              <Link href={r.url} className="rp-entity">
                {r.title}
              </Link>
              <span className="rp-meta">
                used by <EntityLink url={r.project.url} label={r.project.name} />{" "}
                · {r.dependency_type} · {formatDay(r.declared_at)}
              </span>
            </div>
          </li>
        );
      }}
    />
  );
}

export function ReproductionList({ items }: { items: ProfileReproduction[] }) {
  return (
    <Section
      title="Reproductions"
      items={items}
      empty="No reproduction recorded."
      render={(i) => {
        const r = items[i];
        const project = printableEntity(r.project);
        return (
          <li key={i} className="rp-row">
            <div className="rp-row-main">
              <span className="rp-event">{describeRelation(r.relation)}</span>
              <span className="rp-meta">
                {r.review_state} · {formatDay(r.created_at)}
                {project !== null && (
                  <>
                    {" · "}
                    <EntityLink url={project.url} label={project.label} />
                  </>
                )}
              </span>
            </div>
          </li>
        );
      }}
    />
  );
}
