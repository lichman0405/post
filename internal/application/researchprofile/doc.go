// Package researchprofile is the Research Profile and Organization Profile
// read model (T0808): the two public surfaces docs/05 §3/§4 name — a person's
// contribution identity and an institution's research identity — built from
// facts the platform already records.
//
// # What the two surfaces are, and where their shape comes from
//
// docs/42_PAGE_SPECS.md's Research Profile block is the page's content list,
// verbatim: "Affiliations、public contribution dimensions、accepted/released/
// reused/reproduced evidence、assets/projects、confidential verified
// contribution summaries". The Organization Profile block is docs/42's
// "institutional research identity", and docs/05 §3's route table gives both
// the same visibility ("optional if public").
//
// Every dimension this package renders is derived from rows that already
// exist, and each one names its own source:
//
//   - Affiliations — organization_memberships (00002/00018): role, start and
//     end dates, verification. docs/04 §6 is the rule that makes them a
//     dimension rather than a current-state field: "Person identity 跨
//     Organization 长期存在。Affiliation 具有 start/end 时间和 verification
//     status。组织不能删除个人历史贡献；离职只终止 affiliation/role。"
//   - Contributions — contribution_events, the append-only ledger the T0807
//     projection writes from the domain event log (docs/13 §1). The row
//     carries what the event said: its type, the docs/04 §4 roles, when it
//     happened, and the two context facts docs/13 §4 makes dimensions of
//     their own (accepted_context, released_context).
//   - Assets — research_asset_versions joined to the credits
//     asset_version_parties records (00082): docs/11 §6's "Creator/history
//     永久保留" is why a credit is a fact about a version and never revised.
//   - Reuse — asset_dependencies (00010): the projects that declared they use
//     one exact published version. docs/42's Asset Page names the direction
//     ("used/derived public links") and internal/assets/usage.go records what
//     the row means.
//   - Reproductions — evidence_assertions (00007/00058/00091) of the two
//     relations docs/10 §4 lists for it (reproduces, fails_to_reproduce).
//   - Projects — the public projects the rendered contributions name, derived
//     from the rows above rather than from a second read: project_memberships
//     is a governance relation (who may write), and a profile publishes a
//     person's WORK, not their project ACL.
//
// # There is no score, and this package has nowhere to put one
//
// docs/13 §4: "禁止单一分数。Profile 展示多维 evidence". docs/13 §6 refuses
// raw commit/experiment counts as quality proxies, and CLAUDE.md §9 invariant
// 13 forbids a Truth Score / Research Score. So this package renders LISTS OF
// FACTS and no aggregate of any kind: no total, no per-dimension count, no
// weight, no rank, no ordering key that could stand in for one. The three
// dimensions docs/04 §5 additionally names — Review quality, Cross-project
// impact, Industrial/Public contribution — are deliberately NOT here: the
// first two would require a judgement of quality or impact that no
// specification defines and that docs/02 §2 keeps out of the platform's
// automatic judgements, and the third has no evidence source in the schema.
// A field invented to fill those would be the score this build is not allowed
// to have.
//
// # Fail closed, and never a count of what was withheld
//
// Two independent layers, and both are needed. Each read states the render
// predicate of the dimension it feeds in SQL, with its LIMIT after that
// predicate, so a bounded window is counted in rows this surface may actually
// RENDER rather than in everything written — otherwise an author's newer
// invisible rows would spend the window and push visible ones out of it
// (tests/integration TestResearchProfileWindowIsCountedInRenderableRows; the
// same arrangement T1004 gave the feed reads). The rules themselves are still
// enforced HERE, on every row, without trusting what a query returned — the
// query is a read strategy and this package is the disclosure rule, so a
// reader that filtered too little renders a SHORTER profile, never a leak,
// and a reader that filtered too much costs entries rather than correctness.
// The two layers fail in opposite directions on purpose, which is why neither
// is redundant with the other.
//
// An invisible row is DROPPED without being counted either way. docs/23 §5 is
// the rule — "私有对象计数也不能通过 public API 泄漏" — and it offers V1 the
// option this package takes: "V1 可直接不显示 private count". A withheld
// contribution leaves no trace in the payload: not a count, not a placeholder
// row, not a difference between two numbers.
//
// # What is deliberately not here
//
// Confidential verified contribution summaries (docs/13 §5, docs/04 §7, and
// the last item of docs/42's Research Profile list) are NOT rendered, and
// neither is any aggregate stand-in for them. An organization-issued
// attestation needs three things the specifications do not define: who may
// issue one, the "披露级别" vocabulary docs/04 §7 requires each attestation to
// state, and the rule for when a count stops being a leak. Those are L3
// decisions (the Supervisor recorded the same gap in tasks/decisions.md
// before this task was dispatched); until they are answered, a contribution
// to a private project is withheld entirely rather than summarized.
package researchprofile
