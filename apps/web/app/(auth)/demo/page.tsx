import type { Metadata } from "next";
import Link from "next/link";
import { DEMO_ACCOUNTS } from "../../../lib/demo-accounts";
import "./demo.css";

export const metadata: Metadata = { title: "POST demo accounts" };

export default function DemoAccountsPage() {
  return (
    <div className="demo-accounts-page">
      <header className="demo-accounts-header">
        <Link href="/login" className="demo-back-link">← Sign in</Link>
        <p className="demo-accounts-kicker">Synthetic research workspace</p>
        <h1>Explore the POST demo</h1>
        <p>
          Eight fictional accounts let you inspect the same research network from
          different roles. Each account uses POST&apos;s normal sign-in form.
        </p>
        <div className="demo-accounts-notice" role="note">
          <strong>Demo credentials only.</strong> The projects, people, institutions,
          measurements, and passwords below are synthetic. The accounts appear after
          the demo seed has been run.
        </div>
      </header>

      <section className="demo-account-table-wrap" aria-labelledby="demo-accounts-title">
        <h2 id="demo-accounts-title">Choose a role</h2>
        <div className="demo-account-list">
          {DEMO_ACCOUNTS.map((account) => (
            <article className="demo-account" key={account.email}>
              <div className="demo-account-person">
                <h3>{account.name}</h3>
                <span>{account.role}</span>
              </div>
              <div className="demo-account-org">
                <strong>{account.organization}</strong>
                <span>{account.project}</span>
              </div>
              <div className="demo-account-credentials">
                <code>{account.email}</code>
                <code>{account.password}</code>
              </div>
              <Link
                className="demo-account-login"
                href={{ pathname: "/login", query: { email: account.email } }}
              >
                Sign in as this role
              </Link>
            </article>
          ))}
        </div>
      </section>

      <section className="demo-journey" aria-labelledby="demo-journey-title">
        <h2 id="demo-journey-title">What to explore</h2>
        <ol>
          <li><strong>Research history:</strong> branch work, reviewed merges, releases, evidence, and a scientific conflict left unresolved.</li>
          <li><strong>Digital assets:</strong> datasets, protocol, material collection, benchmark, pinned dependencies, rights, creators, and lineage.</li>
          <li><strong>Independent contributions:</strong> a separate university replication and a computational MOF-Y transferability project.</li>
          <li><strong>Scientific figures:</strong> adsorption curves generated from the same synthetic CSV fixtures used by the seed.</li>
        </ol>
        <p>Large-file upload and download are outside this demo&apos;s V1 scope; the pages show metadata, lineage, and figures.</p>
      </section>
    </div>
  );
}
