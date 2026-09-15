// Package assetshttp is the transport of the publication impact preview
// (T0704, docs/11 §3, docs/23 §4):
//
//	POST /api/v1/projects/{projectId}/assets:publish-preview
//	     Build publication impact/rights preview
//
// It answers one question, read-only: if this publish were executed, who
// would see what? The route is registered with the path and the method the
// contract fixes (specs/api/openapi.yaml) and it has exactly one of them.
//
// # Read-only: what holds here, and where it is proved
//
// The route answers with a preview and writes nothing: no asset row, no
// version row, no audit event, no domain event. The publish itself — the
// state write, the explicit human action it requires and the audit event
// that records it — is T0705's surface (docs/23 §4 lists the whole
// operation; this is its preview).
//
// The production guarantee is that guarantee, and only it: nothing on this
// path issues a write (state.go's reader, the queries under
// internal/persistence/queries/asset_preview.sql), and the pool cmd/api
// hands the reader is the service's ordinary one. "Could not write even if
// it tried" is NOT a property of production: it is what
// tests/integration deliberately builds, by running this same route against
// a real PostgreSQL over a session whose connections carry
// default_transaction_read_only=on — a write there is refused by the server
// with SQLSTATE 25006, while the same statement succeeds on the writable
// pool, which is what makes the refusal a fact about the session rather
// than about the statement. The same suite compares every row of the
// database before and after a preview.
//
// # What it resolves, and where the reading happens
//
// The pure half lives in internal/assets (assets.Preview): it runs the SAME
// publish gate the publish command runs (assets.Gate, docs/11 §3) and turns
// the candidate's documents into the five lists the question has — the
// objects, the metadata, the blobs, the refs, and the private dependencies
// that must not ride along into a public version.
//
// The half that needs the repository's current state is here, because it is
// a read of storage and internal/persistence's root package files are not
// in this task's scope: the adapter travels with the transport, exactly as
// cmd/api/provenancehttp's projection store does (T0505; L1, recorded in
// both task results). The SQL itself does NOT travel: the preview's reads
// are canonical queries under internal/persistence/queries
// (asset_preview.sql) and their generated code under
// internal/persistence/sqlc, so they are covered by the sqlc drift check
// like every other query in the tree.
//
// The candidate names things by IDENTITY — a pid, a pid@version pin, a
// kind:value origin ref, a blob id — and the reader answers, for each one,
// whether it exists and how visible it is today:
//
//   - pid → research_assets.pid (the unique public identity, migration
//     00064): the row's type and origin project are what the preview
//     checks the candidate against, and the visibility of that project
//     (projects.visibility, the same read a project:<uuid> ref makes) is
//     what decides whether the asset's project and title may be rendered at
//     all — see "whose identity may be rendered" below;
//   - pid@version → research_asset_versions.visibility, the state of the
//     pinned version. A pin that resolves to a private version is the
//     leak docs/23 §4 is about;
//   - project:<uuid> → projects.visibility (the V1 public/private preset,
//     docs/12 §2);
//   - release:<uuid> → releases.project_id → projects.visibility;
//   - state:<uuid> → project_states.project_id → projects.visibility;
//   - object_version:<uuid> → scientific_object_versions.object_id →
//     scientific_objects.project_id → projects.visibility, plus the
//     version's title, because that is what a published page renders;
//   - blob id → blobs.id and whether ANY attachment of that blob records
//     access_level = 'open' (blob_attachments.access_level). What this
//     answers is exactly that question and no more: an attachment that says
//     open is a NECESSARY but not a SUFFICIENT reading of docs/17 §5's
//     download gate ("Public metadata 不代表 blob open download"), because
//     the object's and the project's access policy do not take part in this
//     decision — a blob openly attached only inside a private project is
//     reported as open here, and docs/12 §2 keeps a private project's blobs
//     invisible regardless. The predicate is deliberately left at one axis:
//     what the full download gate is, and how its inputs rank, is product
//     semantics this task does not decide (recorded as a follow-up). What
//     the preview's blob rule actually refuses is narrower, and stated
//     where it is enforced (assets.Preview): a version whose documents
//     promise open data access while no attachment says open. Fail-closed
//     on the axis it does decide: a blob with no openly attached row —
//     including one with no attachment at all — is not open.
//
// Four different tables answer for the four origin kinds, and all four
// answers are "the visibility of the project the entity belongs to".
// That is not a shortcut: every V1 origin entity is an entity of exactly
// one project (docs/09 §1: the project is the research boundary), so
// "would publishing this ref disclose something the network cannot see
// today?" has one shape of answer.
//
// # Whose identity may be rendered (T0712, issue #238)
//
// The route's gate asks ONE question about the caller — whether it may
// read the project in the path — so a response that rendered the project
// id, the visibility and the title of every entity it resolved would
// answer, for anyone who can read the publishing project, questions about
// any other project the caller can name an identity of: does this uuid
// exist, whose is it, is that project public, what is it called. A caller
// who belongs to A alone could ask all four about a private entity of B by
// putting B's pid or uuid in its own candidate.
//
// assets.Preview therefore renders those fields for exactly two
// owners: the publishing project itself, and a PUBLIC project. Every other
// entry is still produced — the ref is listed, resolved, and a private
// dependency is still named and still blocking, which is what docs/23 §4
// sends this route to say and what a publish refusing on a hidden
// dependency needs — and the fields that would say whose private state it
// is are withheld, as is the second identity of the entity behind a version
// the caller named (the scientific_objects row id, which the caller never
// held). How a withheld field looks on the wire differs by field and is not
// uniform: an object's project_id, title, current_visibility and object_id,
// and an asset's title and origin_project_id, have no omitempty, so they are
// served as an EMPTY STRING with the key still present; a ref's project_id
// and current_visibility carry omitempty (they did before this change too),
// so they are served by OMITTING the key. The rule is membership-independent:
// it consults no membership of the caller's, so a caller who belongs to both
// projects learns no more about B here than one who belongs to neither,
// and the same request answers the same way for everyone.
//
// The reader's contract is assets.CurrentState: it must answer for EVERY
// canonical ref the candidate declares, including the ones that do not
// exist (Resolved=false). One it simply omits is an error
// (assets.ErrIncompleteState) answered as 500, because a dropped ref must
// fail loudly rather than be reported as a finding — "the ref does not
// exist" and "my reader did not look" are different statements, and only
// one of them is true advice about the publication.
//
// # A document that cannot be read is a finding, not an error
//
// The candidate's refs are the candidate's own field, so the reader
// resolves them whether or not the manifest parses (state.go). A manifest
// the platform cannot read — malformed JSON, an unknown type, a required
// metadata key missing — costs the preview the pins and the blobs that are
// declared IN that document, and costs it nothing else: the refs are
// resolved and reported, the gate's ASSET_* manifest refusal appears in
// publish_blockers, and publishable is false. That is the same answer this
// route gives every unexecutable candidate (see previewRequest): the
// candidate is refused and the impact of what IS readable is shown anyway.
// What it is NOT is a claim that the unreadable document hides no private
// dependency — the refusal says the document could not be read at all,
// which is why the pins and the blobs it would have declared are absent
// rather than reported as absent. ErrIncompleteState stays what it was: the
// reader dropped a ref it was required to answer for.
//
// # Authorization
//
// The route is a POST, so the shared v1 guard already requires a session
// and a CSRF token (cmd/api); on top of that the handler resolves the
// principal and then runs the same project read gate every other project
// read runs (projects.Service.Get, T0106) — a project the caller cannot
// see is answered with the existence-hiding 404. The preview answers for
// the identities the CALLER named in its own request body (it is not an
// enumeration oracle: nothing in the response can be reached without
// putting the identity in the request first), and WHOSE project an identity
// turned out to be — a project id, a visibility, an entity's title — is
// disclosed only for the publishing project and for public ones (see
// "whose identity may be rendered"). What remains disclosed about another
// project's private entity is its existence, and that the entry the caller
// sent is a private dependency this publication must not carry — the
// private_dependencies list says so of the caller's own ref, and reports
// which project it points into as "another private project" without naming
// it. Both of those are the finding docs/23 §4 asks this route to deliver,
// and both stay reachable only to a caller that
// already held its identity: a 128-bit random token the caller could not
// have guessed, which is the price of telling it "the entry you sent names
// something real that this publication must not expose" (docs/23 §4).
//
// What this route deliberately does NOT decide is whether the caller may
// publish: the publish authorization (membership + the matrix row for the
// publish action) belongs to the publish command (T0705), and duplicating
// it here would create a second, weaker copy of a security decision. A
// preview is a read of what a proposed publication would expose, served to
// a caller who can read the project it is proposed in.
package assetshttp
