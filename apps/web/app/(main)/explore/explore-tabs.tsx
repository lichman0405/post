"use client";

import { useRef, useState } from "react";
import Link from "next/link";
import {
  BookIcon,
  OrganizationIcon,
  PackageIcon,
  PeopleIcon,
  ProjectIcon,
  TelescopeIcon,
} from "@primer/octicons-react";

import {
  EXPLORE_TABS,
  publishedOn,
  tabId,
  tabLabel,
  tabPanelId,
  type ExploreAsset,
  type ExploreContribution,
  type ExploreIndex,
  type ExploreKnowledge,
  type ExploreOrganization,
  type ExplorePerson,
  type ExploreProject,
  type ExploreProjectRef,
  type ExploreTab,
} from "../../../lib/explore";
import "./explore.css";

/**
 * The Explore surface (T0802): the six dimensions of docs/05 §6 — Projects,
 * Assets, Knowledge, People, Organizations, Open Contributions — over the
 * public index the API builds (internal/application/explore).
 *
 * Four things this component deliberately does NOT do.
 *
 * It does not rank. Each panel renders its rows in the order the API sent
 * them; that order is freshness (docs/05 §6 forbids点赞数 as the core
 * ranking), and a client-side sort would be a second ranking rule that
 * nothing tests and no reader is told about.
 *
 * It does not filter or hide. Every row the API sent is rendered. Where the
 * API withheld a project (a nil `project`) this renders NOTHING — no "hidden
 * project" note, no dash, no chip: that sentence is a fact about a private
 * object, which is what the withholding exists to keep off the wire
 * (docs/23 §3/§5).
 *
 * It does not count. No panel shows a total and no tab shows a badge: how
 * many rows a reader can see is how many rows are rendered, and a total
 * would have to be subtracted from to mean anything (docs/23 §5).
 *
 * It does not require JavaScript to show the index. All six panels are in
 * the DOM and in the HTML the server sends; the tabs are an enhancement over
 * a page that already contains everything (progressive enhancement — and the
 * reason the page server-renders the index instead of fetching it in the
 * browser). See the noscript rule in page.tsx, which reveals every panel for
 * a reader whose script never runs.
 *
 * # Why the tabs are links
 *
 * Each tab is an <a role="tab"> pointing at its panel, not a <button>: with
 * script disabled it still does something (it jumps to the section), instead
 * of being a control that quietly does nothing. With script enabled the click
 * is intercepted and the ARIA tablist takes over. The panels keep their own
 * <h2> headings, so the page has one structure — headings — and the tablist
 * is a second way to move through it, not the only way to know where you are.
 */

/** The icon of each dimension. Decorative: the label carries the meaning. */
const TAB_ICONS: Record<ExploreTab, typeof ProjectIcon> = {
  projects: ProjectIcon,
  assets: PackageIcon,
  knowledge: BookIcon,
  people: PeopleIcon,
  organizations: OrganizationIcon,
  contributions: TelescopeIcon,
};

export function ExploreTabs({ index }: { index: ExploreIndex }) {
  // The first tab is selected from the first render: a page that opened with
  // no dimension selected would open on five hidden panels and one shown, and
  // the shown one has to be a decision, not an accident.
  const [active, setActive] = useState<ExploreTab>(EXPLORE_TABS[0]);
  const tabRefs = useRef<Partial<Record<ExploreTab, HTMLAnchorElement | null>>>({});

  // Arrow/Home/End move between tabs (the WAI-ARIA tabs pattern). Focus
  // follows the selection, so the two are never out of step.
  function move(from: ExploreTab, delta: number | "first" | "last") {
    const i = EXPLORE_TABS.indexOf(from);
    let next: ExploreTab;
    if (delta === "first") next = EXPLORE_TABS[0];
    else if (delta === "last") next = EXPLORE_TABS[EXPLORE_TABS.length - 1];
    else next = EXPLORE_TABS[(i + delta + EXPLORE_TABS.length) % EXPLORE_TABS.length];
    setActive(next);
    tabRefs.current[next]?.focus();
  }

  function onKeyDown(event: React.KeyboardEvent<HTMLAnchorElement>, tab: ExploreTab) {
    switch (event.key) {
      case "ArrowRight":
        event.preventDefault();
        move(tab, 1);
        break;
      case "ArrowLeft":
        event.preventDefault();
        move(tab, -1);
        break;
      case "Home":
        event.preventDefault();
        move(tab, "first");
        break;
      case "End":
        event.preventDefault();
        move(tab, "last");
        break;
      default:
        break;
    }
  }

  return (
    <div className="explore">
      <h1 className="explore-title">Explore</h1>
      <p className="explore-sub">
        The open network: public projects, published assets and knowledge,
        research profiles, organizations and the contributions anyone can
        take on. Newest first, and no popularity anywhere in it.
      </p>

      <div className="explore-tablist" role="tablist" aria-label="Explore dimensions">
        {EXPLORE_TABS.map((tab) => {
          const Icon = TAB_ICONS[tab];
          const selected = tab === active;
          return (
            <a
              key={tab}
              role="tab"
              id={tabId(tab)}
              href={`#${tabPanelId(tab)}`}
              className="explore-tab"
              aria-selected={selected}
              aria-controls={tabPanelId(tab)}
              tabIndex={selected ? 0 : -1}
              data-explore-tab={tab}
              ref={(el) => {
                tabRefs.current[tab] = el;
              }}
              onClick={(event) => {
                // With script running the tablist owns the behaviour; without
                // script this never fires and the anchor does its normal job.
                event.preventDefault();
                setActive(tab);
              }}
              onKeyDown={(event) => onKeyDown(event, tab)}
            >
              <span className="explore-tab-icon" aria-hidden="true">
                <Icon size={16} />
              </span>
              {tabLabel(tab)}
            </a>
          );
        })}
      </div>

      {EXPLORE_TABS.map((tab) => (
        <section
          key={tab}
          role="tabpanel"
          id={tabPanelId(tab)}
          aria-labelledby={tabId(tab)}
          className="explore-panel"
          data-explore-panel={tab}
          // hidden, not unmounted: the rows stay in the document (and in the
          // served HTML) for a reader without script to reach.
          hidden={tab !== active}
        >
          <h2 className="explore-panel-heading">{tabLabel(tab)}</h2>
          {renderSection(tab, index)}
        </section>
      ))}
    </div>
  );
}

/** One section's body: the rows, or the honest empty line for it. */
function renderSection(tab: ExploreTab, index: ExploreIndex): React.ReactNode {
  switch (tab) {
    case "projects":
      return listOf(index.projects.items, "No public projects yet.", (p) => (
        <ProjectRow key={p.id} project={p} />
      ));
    case "assets":
      return listOf(index.assets.items, "No published assets yet.", (a) => (
        <AssetRow key={a.pid} asset={a} />
      ));
    case "knowledge":
      return listOf(index.knowledge.items, "No published knowledge yet.", (k) => (
        <KnowledgeRow key={k.id} knowledge={k} />
      ));
    case "people":
      return listOf(index.people.items, "No public research profiles yet.", (p) => (
        <PersonRow key={p.id} person={p} />
      ));
    case "organizations":
      return listOf(index.organizations.items, "No organizations yet.", (o) => (
        <OrganizationRow key={o.id} organization={o} />
      ));
    case "contributions":
      return listOf(
        index.contributions.items,
        "No open contributions right now.",
        (c) => <ContributionRow key={c.id} contribution={c} />,
      );
    default:
      return null;
  }
}

function listOf<T>(
  items: T[],
  emptyLine: string,
  row: (item: T) => React.ReactNode,
): React.ReactNode {
  if (items.length === 0) {
    // An empty section is a fact ("nothing public in this dimension"), and
    // it is rendered as one — never as an error and never as a spinner.
    return <p className="explore-empty">{emptyLine}</p>;
  }
  return <ul className="explore-list">{items.map(row)}</ul>;
}

/**
 * projectNote renders the project a row may name, or nothing at all.
 *
 * "Nothing at all" is the important half: the API sends null when the
 * project is private, and this renders no placeholder and no "private" chip.
 * A reader learns nothing about a withheld project — including that there is
 * one.
 */
function projectNote(project: ExploreProjectRef | null): React.ReactNode {
  if (project === null) return null;
  return (
    <Link href={project.url} className="explore-row-project" data-explore-project={project.slug}>
      {project.name}
    </Link>
  );
}

function ProjectRow({ project }: { project: ExploreProject }) {
  return (
    <li className="explore-row" data-explore-project-row={project.slug}>
      <Link href={project.url} className="explore-row-title">
        {project.name}
      </Link>
      <span className="explore-row-facts">
        <code className="explore-row-id">{project.slug}</code>
        <span className="explore-chip" data-explore-status={project.activity_status}>
          {project.activity_status}
        </span>
        <span className="explore-row-date">{publishedOn(project.created_at)}</span>
      </span>
      {project.purpose === "" ? null : <p className="explore-row-note">{project.purpose}</p>}
    </li>
  );
}

function AssetRow({ asset }: { asset: ExploreAsset }) {
  return (
    <li className="explore-row" data-explore-asset-row={asset.pid}>
      <Link href={asset.url} className="explore-row-title">
        {asset.title}
      </Link>
      <span className="explore-row-facts">
        <span className="explore-chip" data-explore-asset-type={asset.type}>
          {asset.type}
        </span>
        <code className="explore-row-id">{asset.pid}</code>
        {projectNote(
          asset.origin_project === null
            ? null
            : { ...asset.origin_project, url: `/projects/${asset.origin_project.id}` },
        )}
        <Link href={asset.latest_url} className="explore-row-link">
          latest {asset.latest_version}
        </Link>
        <span className="explore-row-date">{publishedOn(asset.latest_published_at)}</span>
      </span>
    </li>
  );
}

function KnowledgeRow({ knowledge }: { knowledge: ExploreKnowledge }) {
  return (
    <li className="explore-row" data-explore-knowledge-row={knowledge.object_id}>
      {/* No link: the knowledge object page is not built yet (T0805 owns
          it), and a link to a route that does not exist is a lie in the
          shape of an href. The object id and the public version are the
          citation until then. */}
      <span className="explore-row-title">{knowledge.title}</span>
      <span className="explore-row-facts">
        {/* The published VERSION's own state (docs/43): a withdrawn or
            superseded version is rendered as such, so a reader never takes
            stale research for current. */}
        <span className="explore-chip" data-explore-lifecycle={knowledge.lifecycle_state}>
          {knowledge.lifecycle_state}
        </span>
        <span className="explore-chip" data-explore-object-type={knowledge.object_type}>
          {knowledge.object_type}
        </span>
        <code className="explore-row-id">{knowledge.object_id}</code>
        <span className="explore-row-version">{knowledge.public_version}</span>
        {projectNote(knowledge.project)}
        <span className="explore-row-date">{publishedOn(knowledge.published_at)}</span>
      </span>
    </li>
  );
}

function PersonRow({ person }: { person: ExplorePerson }) {
  return (
    <li className="explore-row" data-explore-person-row={person.handle}>
      <Link href={person.url} className="explore-row-title">
        {person.display_name}
      </Link>
      <span className="explore-row-facts">
        <code className="explore-row-id">@{person.handle}</code>
      </span>
      {person.bio === "" ? null : <p className="explore-row-note">{person.bio}</p>}
    </li>
  );
}

function OrganizationRow({ organization }: { organization: ExploreOrganization }) {
  return (
    <li className="explore-row" data-explore-organization-row={organization.slug}>
      {/* No link: the organization page is T0808's. The slug is the identity
          until then. */}
      <span className="explore-row-title">{organization.name}</span>
      <span className="explore-row-facts">
        <code className="explore-row-id">{organization.slug}</code>
      </span>
      {organization.description === "" ? null : (
        <p className="explore-row-note">{organization.description}</p>
      )}
    </li>
  );
}

function ContributionRow({ contribution }: { contribution: ExploreContribution }) {
  return (
    <li className="explore-row" data-explore-contribution-row={contribution.id}>
      {/* No link: taking an opportunity on is T0803's surface (the project's
          opportunities page), and it is member-facing — a public row here
          says what the work is, not where to claim it. */}
      <span className="explore-row-title">{contribution.title}</span>
      <span className="explore-row-facts">
        <span className="explore-chip" data-explore-difficulty={contribution.difficulty}>
          {contribution.difficulty}
        </span>
        <span className="explore-chip" data-explore-target-type={contribution.target_type}>
          {contribution.target_type}
        </span>
        {contribution.required_capabilities.map((capability) => (
          <code className="explore-row-capability" key={capability}>
            {capability}
          </code>
        ))}
        {projectNote(contribution.project)}
        <span className="explore-row-date">{publishedOn(contribution.publicized_at)}</span>
      </span>
      {contribution.description === "" ? null : (
        <p className="explore-row-note">{contribution.description}</p>
      )}
    </li>
  );
}
