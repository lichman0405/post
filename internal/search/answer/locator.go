package answer

import (
	"net/url"
	"strings"

	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/search"
	"github.com/lichman0405/post/internal/search/ranking"
	"github.com/lichman0405/post/internal/search/retrieval"
)

// The acceptance criterion of T0906's second half — "source click 可定位" — is
// older than this task: a citation a reader cannot open is a citation they
// have to take on faith, which is the whole difference between an
// evidence-backed answer (ADR-010) and a plausible one. So every source
// carries where a click on it lands.
//
// # One address family, stated
//
// The href is an API path in the SAME namespace as the answer document
// itself. The answer comes out of /api/v1/search, and its sources point back
// into /api/v1/... — the asset page data, the published knowledge read, the
// release read. A client that can fetch the answer can fetch every source it
// names, without knowing the web app's route table; the web app maps an
// entity to its own page with its own builders (internal/assets/url.go for
// the asset pages). Mixing the two families would mean an href whose meaning
// depends on which surface happened to receive it, which is exactly the kind
// of ambiguity a citation cannot afford.
//
// # The closure, and why it is short
//
// Every kind and entity type the projection can produce is either addressed
// here or refused, and the refusals are the interesting half:
//
//	kind            entity type   address
//	document        asset         /api/v1/assets/{identity}[?version=label]
//	document        knowledge     /api/v1/knowledge/{identity}
//	document        release       /api/v1/projects/{projectID}/releases/{identity}
//	document        state         none
//	object_version  (any)         none
//
//   - A state has no address because the platform has no route that reads
//     one by id. Every object read that exists is nested under a BRANCH
//     (/api/v1/projects/{projectId}/branches/{branchId}/objects/{objectId},
//     cmd/api/rsghttp/wiring.go), and a candidate does not carry the branch —
//     a project state is reached through the projection, not through the
//     branch it was committed on. A link that guessed a branch would be a
//     fabricated locator, which is the same defect as a fabricated citation,
//     so the honest answer is no link.
//   - An object_version is the same case: it is a version of an object inside
//     a project, its branch is not carried, and the routes that take
//     (projectId, objectId) without a branch read the object's EVIDENCE and
//     LINEAGE, not the object. Pointing a click at a different thing than the
//     source would be worse than pointing it at nothing.
//
// Both still carry Source.ProjectID, so a client that has its own route into
// a project (the web app does: /projects/{id}/research) can place the source
// without this package inventing an address for it.
//
// # What makes a source locatable at all
//
// The identity has to be usable as a path segment. A pid, a uuid and a
// release id are; an identity carrying '/', '?' or '#' is refused rather than
// escaped, because escaping it would produce a path the platform does not
// serve — the route patterns above match a single segment, and a client
// following a link that 404s has been told something false about what the
// platform holds. This branch is unreachable for the projection's own
// identities (search/sources.go writes pids and uuids); it is the floor for a
// candidate from anywhere else, the same role the identity fallback in
// retrieval.documentCandidate plays.

// locate returns the address a click on a source lands on, or "" when the
// platform has none for it (see the closure above — "" is a fact about the
// platform's routes, never about the source's quality).
func locate(r ranking.Ranked) string {
	projectID := r.Candidate.ProjectID
	if r.Kind == retrieval.KindObjectVersion {
		// A version reached by traversal: the object it belongs to is real
		// and the platform can show its evidence, but no route serves the
		// version itself (see the closure).
		return ""
	}
	switch r.EntityType {
	case search.EntityAsset:
		// The builder is internal/assets' rather than a literal here: it is
		// documented as the canonical source of the asset URL scheme, and a
		// second copy of that scheme is a second thing to keep in step.
		href := assets.AssetAPIPath(assets.PID(r.Candidate.Identity))
		if label := r.Version; label != "" {
			// The version is a QUERY parameter on the API read
			// (cmd/api/assetshttp/page.go: `?version=`), and a label is
			// 1..64 characters of letters, digits, '.', '_' or '-'
			// (assets.ValidVersionLabel), so the escape is a formality that
			// keeps a hand-built row from producing a broken query.
			href += "?version=" + url.QueryEscape(label)
		}
		return hrefIfAddressable(href, r.Candidate.Identity)
	case search.EntityKnowledge:
		return hrefIfAddressable("/api/v1/knowledge/"+r.Candidate.Identity, r.Candidate.Identity)
	case search.EntityRelease:
		// The release read is nested under its project because a release id
		// is unique within a project's timeline rather than globally
		// (cmd/api/releasehttp/release_wiring.go). The project travels on the
		// candidate because the projection wrote it (search/sources.go).
		if projectID == "" {
			return ""
		}
		return hrefIfAddressable(
			"/api/v1/projects/"+projectID+"/releases/"+r.Candidate.Identity,
			r.Candidate.Identity, projectID)
	default:
		// A state — and any entity type a later projection adds, which must
		// come through here and decide rather than inherit a wrong link by
		// default.
		return ""
	}
}

// hrefIfAddressable refuses a link whose segments cannot be one path segment,
// so a source is either addressed correctly or not at all.
func hrefIfAddressable(href string, segments ...string) string {
	for _, s := range segments {
		if s == "" || strings.ContainsAny(s, "/?#") {
			return ""
		}
	}
	return href
}
