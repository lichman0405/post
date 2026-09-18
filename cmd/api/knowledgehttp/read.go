package knowledgehttp

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/knowledgepublish"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/observability"
)

// The public read (T0805): GET /api/v1/knowledge/{knowledgeId}.
//
// # Where the route comes from
//
// It is the CONTRACT's path (specs/api/openapi.yaml: /knowledge/{knowledgeId},
// `security: []`, "Published knowledge object plus origin/network evidence
// state"), registered verbatim under the v1 server prefix, so anonymous
// callers reach it: the v1 guard lets an unauthenticated read through and the
// handler decides what that caller may see, which is what `security: []`
// means everywhere else in this tree.
//
// # The order of the steps is the order of what may be disclosed
//
// The pid is checked, the publication is resolved, the audience rule decides,
// and only then are the membership facts read and the body built. The 404 is
// answered for every refusal — an unknown pid, a string that is not a pid, a
// publication whose audience is the project's members and a caller who is not
// one of them — and answering one of those differently would tell an anonymous
// caller which publications exist.
//
// # The audience rule is the command's, not a second one
//
// knowledgepublish.AudienceFor is the ONE definition of who may read a
// published knowledge object. It reads the version's OWN visibility axis
// (scientific_object_versions.visibility_policy_id), then the owning project's
// preset, then the published rights document's metadata token — which is the
// shape owner ruling L3-20260916-1 #1 requires: publishing records a state,
// visibility is that other axis, and 发布不等于公开. Reading the project's
// preset ALONE here would be the bug the ruling was written against: a
// visibility-restricted version in a public project would become readable by
// the network the moment it was published, which is exactly what publishing
// must not do.
//
// # What the response may contain
//
// The publication's own facts and the version it published — never the blob,
// never a rights narrowing, never search. The version's origin/network
// evidence state, which the contract's summary also names, is NOT rendered
// here: no read for it exists yet on this surface, and a section invented by
// an implementation task would be a contract decision made in the wrong
// place (the same restraint assetshttp records for the browse route). See the
// task result.

// Wire codes (docs/45: stable codes, no dependency detail).
const (
	// CodeKnowledgeNotFound: the pid names no publication this caller may
	// see. It is one code for the cases that answer it — no such pid, a
	// segment that is not a pid, and a publication whose audience this
	// caller is not in — because they must be indistinguishable.
	CodeKnowledgeNotFound = "KNOWLEDGE_NOT_FOUND"
	// CodeKnowledgeUnavailable: the read failed, so no honest answer can be
	// built. A publication page is a statement about the repository as it
	// was read, and one built over a failed read would report facts about a
	// repository nobody finished looking at.
	CodeKnowledgeUnavailable = "KNOWLEDGE_UNAVAILABLE"
)

// publishedKnowledgePayload is the 200 body: the publication, the version it
// published, and the origin facts a reader can check.
//
// The publication's own ROW uuid is NOT on the wire: the pid is the identity
// a client addresses and cites, exactly as the asset page names an asset by
// pid. The object and version uuids that do appear are the ones a caller
// already holds — object_version:<uuid> is how a publish request spells the
// version it names, so a citation of it is a citation the client made
// itself.
type publishedKnowledgePayload struct {
	PID           string          `json:"pid"`
	PublicVersion string          `json:"public_version"`
	PublishedBy   string          `json:"published_by"`
	PublishedAt   string          `json:"published_at"`
	Rights        json.RawMessage `json:"rights"`
	// Audience is who may read this: "network" or "members". It is stated
	// rather than left to the client to derive, because it is the answer to
	// "is this public?" and the platform's answer to that question is a
	// rule, not a field a client should re-implement.
	Audience knowledgepublish.Audience `json:"audience"`
	// Object names the knowledge object this version belongs to.
	Object objectPayload `json:"object"`
	// Version names the published version and its integrity facts.
	Version versionPayload `json:"version"`
	// Project is the project that owns the object, named by pid-less identity:
	// its id and preset only. A publication from a PRIVATE project is legal
	// (docs/12 §2) and its members read it here; naming the project's slug or
	// name would publish an identity the ruling's second axis does not.
	Project projectPayload `json:"project"`
}

// objectPayload is the knowledge object the published version belongs to.
type objectPayload struct {
	ID         string `json:"id"`
	ObjectType string `json:"object_type"`
}

// versionPayload is the published version.
type versionPayload struct {
	ID             string `json:"id"`
	Title          string `json:"title"`
	LifecycleState string `json:"lifecycle_state"`
	SchemaID       string `json:"schema_id"`
	SchemaVersion  string `json:"schema_version"`
	IntegrityHash  string `json:"integrity_hash"`
}

// projectPayload names the owning project without disclosing a private
// project's identity.
type projectPayload struct {
	ID         string `json:"id"`
	Visibility string `json:"visibility"`
}

// handlePublishedKnowledge serves GET /api/v1/knowledge/{knowledgeId}.
//
// The segment in the path is the publication's PID. A segment that is not a
// pid names no publication, and the store answers "not found" for it without
// touching the database — a slug-shaped segment cannot resolve here by
// construction. Both that and a well-formed pid that resolves to nothing get
// the same 404.
func (h *handlers) handlePublishedKnowledge(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("knowledgeId")
	if !knowledgepublish.ValidPID(pid) {
		writeKnowledgeNotFound(w, r)
		return
	}
	entry, found, err := h.read.GetPublishedKnowledge(r.Context(), pid)
	if err != nil {
		observability.LoggerFromContext(r.Context()).Error("knowledge read: resolve failed", "error", err, "pid", pid)
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, CodeKnowledgeUnavailable,
			"knowledge data is temporarily unavailable")
		return
	}
	if !found {
		writeKnowledgeNotFound(w, r)
		return
	}
	// Who may read this publication is ONE decision (mayRead), asked here
	// and — about the publication a preview would report as existing — by
	// the preview route. A publication the network may see is served to
	// anyone; a members-only one only to a member of the project that owns
	// the version, and everyone else is answered the same 404 an unknown pid
	// gets, which is what keeps this route from being a directory of private
	// publications.
	readable, err := h.mayRead(r, entry)
	if err != nil {
		observability.LoggerFromContext(r.Context()).Error("knowledge read: membership read failed", "error", err, "pid", pid)
		writeKnowledgeUnavailable(w, r)
		return
	}
	if !readable {
		writeKnowledgeNotFound(w, r)
		return
	}
	authhttp.WriteJSON(w, http.StatusOK, publishedKnowledgeFromDomain(entry))
}

// mayRead reports whether the caller of r may read one resolved publication:
// the audience rule over the publication's own facts, and, when the audience
// is the project's members, the membership the route reads.
//
// It is the ONLY implementation of that decision on this surface, and it is
// deliberately a function of a resolved PublishedKnowledge rather than a rule
// spelled again wherever a publication's identity might be handed out: the
// read route asks it about the publication a pid names, and the publish
// preview asks it about the publication it would otherwise report as
// existing. A second spelling of it is how the two would come to different
// answers about the same caller and the same row — and the preview DID come
// to the other answer: it handed the publication's pid, name, publisher and
// rights to a caller the read route answers 404.
//
// The error is a FAILURE (the route's 503), never a silent "no": a store blip
// that downgraded a member's view to a 404 would be a read that is wrong in
// the direction nobody reports.
func (h *handlers) mayRead(r *http.Request, entry knowledgepublish.PublishedKnowledge) (bool, error) {
	if entry.Audience() == knowledgepublish.AudienceNetwork {
		return true, nil
	}
	return h.member(r, entry.ProjectID)
}

// mayReadPID answers the same question about the publication a pid names, by
// resolving it the way the public read does — the same read, so the two
// cannot disagree about what the row says. A pid that resolves to nothing is
// not readable (the read route's own 404), and a read that failed is an
// error.
func (h *handlers) mayReadPID(r *http.Request, pid string) (bool, error) {
	entry, found, err := h.read.GetPublishedKnowledge(r.Context(), pid)
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}
	return h.mayRead(r, entry)
}

// writeKnowledgeUnavailable renders the one 503 this surface answers when a
// read failed: no honest answer can be built, and one built over a failed
// read would report facts about a repository nobody finished looking at.
func writeKnowledgeUnavailable(w http.ResponseWriter, r *http.Request) {
	authhttp.WriteError(w, r, http.StatusServiceUnavailable, CodeKnowledgeUnavailable,
		"knowledge data is temporarily unavailable")
}

// member reports whether the request's caller belongs to projectID.
//
// An anonymous caller is never a member. For an authenticated caller the
// answer comes from the project service's own GetMembership, and TWO of its
// refusals mean "not a member":
//
//	ErrMemberNotFound   the caller has no role in a project they may read
//	ErrProjectNotFound  the project does not exist — OR the caller may not
//	                    read it at all, because GetMembership resolves the
//	                    project through the same read gate every project read
//	                    uses, and that gate hides an unreadable project
//	                    behind the not-found error (docs/45: the existence of
//	                    a private project is itself private)
//
// Both are the "no" this route needs. Anything else is a FAILURE rather than a
// silent "not a member" — a store blip that quietly downgraded a member's view
// would be a read that is wrong in the direction nobody would report — and it
// is answered 503.
//
// Why the second case is a "no" and not a failure: leaving it to the default
// branch made this route an EXISTENCE ORACLE. A members-only publication in a
// private project answered a signed-in non-member 503 "temporarily
// unavailable, retry" while an unknown pid answered 404, and the only thing
// separating the two answers was that the first pid named a real publication
// in a real project — a caller could walk the pid space and read the
// difference. The 404 this route promises (see handlePublishedKnowledge) has
// to be the answer to every "you may not see this", and it is the same
// writeKnowledgeNotFound an unknown pid gets, so the two are byte-identical.
func (h *handlers) member(r *http.Request, projectID string) (bool, error) {
	p, ok := authhttp.PrincipalFrom(r.Context())
	if !ok {
		return false, nil
	}
	_, err := h.members.GetMembership(r.Context(), p.User, projectID)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, projects.ErrMemberNotFound), errors.Is(err, projects.ErrProjectNotFound):
		return false, nil
	default:
		return false, err
	}
}

// publishedKnowledgeFromDomain renders the resolved publication as the
// response body.
func publishedKnowledgeFromDomain(k knowledgepublish.PublishedKnowledge) publishedKnowledgePayload {
	return publishedKnowledgePayload{
		PID:           k.PID,
		PublicVersion: k.PublicVersion,
		PublishedBy:   k.PublishedBy,
		PublishedAt:   knowledgepublish.FormatInstant(k.PublishedAt),
		Rights:        rightsBytes(k),
		Audience:      k.Audience(),
		Object:        objectPayload{ID: k.ObjectID, ObjectType: k.ObjectType},
		Version: versionPayload{
			ID:             k.ObjectVersionID,
			Title:          k.Title,
			LifecycleState: k.LifecycleState,
			SchemaID:       k.SchemaID,
			SchemaVersion:  k.SchemaVersion,
			IntegrityHash:  k.IntegrityHash,
		},
		Project: projectPayload{ID: k.ProjectID, Visibility: k.ProjectVisibility},
	}
}

// rightsBytes renders the stored rights declaration for the wire.
//
// A document this build cannot read is rendered as JSON null rather than as an
// empty object: the audience rule has already refused a publication whose
// declaration does not parse (AudienceFor refuses anything that is not the
// project-policy token, and an unreadable document states no token), so this
// branch is only reachable on a members-only read — and reporting `null` says
// "the stored declaration is not one this build can render" instead of
// handing a reader an empty object it might mistake for "no rights reserved".
func rightsBytes(k knowledgepublish.PublishedKnowledge) []byte {
	if !k.RightsValid {
		return []byte("null")
	}
	b, err := k.Rights.Marshal()
	if err != nil {
		return []byte("null")
	}
	return b
}

// writeKnowledgeNotFound answers the existence-hiding 404: no such pid, a
// segment that is not a pid, and a publication this caller's audience does not
// include all answer it, with one code.
func writeKnowledgeNotFound(w http.ResponseWriter, r *http.Request) {
	authhttp.WriteError(w, r, http.StatusNotFound, CodeKnowledgeNotFound, "knowledge object not found")
}
