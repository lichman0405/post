import { LawIcon } from "@primer/octicons-react";

import {
  RIGHTS_ACCESS_NOTICE,
  RIGHTS_LEGAL_NOTICE,
  agreementLine,
  dataAccessLabel,
  licenseLine,
  metadataLabel,
  parseRightsDocument,
  unstatedNotice,
  usageRows,
} from "../../lib/rights";

/**
 * The rights panel (T0703).
 *
 * Renders a published research asset version's or a knowledge publication's
 * declaration — the stored rights document, read by lib/rights.ts. It is
 * deliberately dumb in two ways.
 *
 * It holds no state and fetches nothing: the caller passes whatever the API
 * returned and the panel shows it, so a page cannot render a stale or
 * defaulted declaration.
 *
 * It asserts nothing. The heading says "Rights" and every line is attributed
 * to the publisher — the panel reports a record, and the two notices at the
 * foot say what this platform does and does not do with it. Nothing here
 * may be reworded into an assurance about the law; see the module comment in
 * lib/rights.ts and the copy scan in lib/rights-copy.test.mjs.
 */
export function RightsPanel({ rights }: { rights: unknown }) {
  const read = parseRightsDocument(rights);
  const unstated = read.kind === "declaration" ? unstatedNotice(read.unstated) : null;

  return (
    <section className="rights-panel" aria-labelledby="rights-panel-heading">
      <header className="rights-panel-header">
        <span className="rights-panel-icon">
          <LawIcon size={16} aria-hidden="true" />
        </span>
        <h2 className="rights-panel-title" id="rights-panel-heading">
          Rights
        </h2>
      </header>

      {read.kind === "none" ? (
        <p className="rights-panel-empty">{read.reason}</p>
      ) : (
        <>
          <p className="rights-panel-byline">Declared by the publisher.</p>

          <dl className="rights-references">
            <dt>Standard license</dt>
            <dd className="rights-reference-value">{licenseLine(read.document)}</dd>
            <dt>Separate agreement</dt>
            <dd className="rights-reference-value">{agreementLine(read.document)}</dd>
          </dl>

          <h3 className="rights-section-title">Declared usage</h3>
          <ul className="rights-usage">
            {usageRows(read.document).map((row) => (
              <li className="rights-usage-row" key={row.axis}>
                <span className="rights-usage-axis">{row.heading}</span>
                {/* data-declared carries the declared value and nothing
                    else: when the document states none, row.declared is
                    null and React omits the attribute, so the node that
                    reads "Not stated" has no attribute claiming a value.
                    An attribute holding a made-up token would contradict
                    the text beside it. */}
                <span className="rights-usage-value" data-declared={row.declared}>
                  {row.rendered}
                </span>
              </li>
            ))}
          </ul>

          <h3 className="rights-section-title">Access</h3>
          <dl className="rights-access">
            <dt>Metadata visibility</dt>
            <dd>{metadataLabel(read.document.visibility.metadata)}</dd>
            <dt>Data access</dt>
            <dd>{dataAccessLabel(read.document.visibility.data_access)}</dd>
          </dl>

          {read.document.notes === null ? null : (
            <div className="rights-notes">
              <h3 className="rights-section-title">The publisher&rsquo;s note</h3>
              <blockquote className="rights-notes-text">{read.document.notes}</blockquote>
            </div>
          )}

          {unstated === null ? null : <p className="rights-panel-unstated">{unstated}</p>}
        </>
      )}

      <p className="rights-notice">{RIGHTS_ACCESS_NOTICE}</p>
      <p className="rights-notice">{RIGHTS_LEGAL_NOTICE}</p>
    </section>
  );
}
