package evidencehttp

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/sciobjects"
)

// Wire codes (docs/45: stable codes, no dependency detail). The not-found
// outcomes reuse the owning surfaces' codes, so a hidden project, a foreign
// object and a nonexistent one stay indistinguishable from the real thing.
const (
	CodeValidation  = "EVIDENCE_VALIDATION_FAILED"
	CodeUnavailable = "EVIDENCE_UNAVAILABLE"
)

// ErrValidation marks a client-shaped request (a version_no that is not a
// positive integer). It is answered 400; read failures are answered 503.
var ErrValidation = errors.New("evidence graph validation")

// handlers owns the read-only evidence-graph routes.
type handlers struct {
	service Service
	gate    Gate
}

// reader resolves the caller for the visibility-aware read (T0106): the same
// resolution every other project read runs. Anonymous reads flow — the gate
// decides what an anonymous reader may see.
func reader(r *http.Request) projects.Reader {
	p, ok := authhttp.PrincipalFrom(r.Context())
	if !ok {
		return projects.Reader{}
	}
	return projects.Reader{UserID: p.User.ID, Authenticated: true}
}

// handleObjectEvidence: GET
// /api/v1/projects/{projectId}/objects/{objectId}/evidence — one object's
// evidence, grouped by the exact target version each assertion pins, with
// supports and contradicts in their own groups (acceptance criterion: 为什么
// 相信 / 为什么质疑 answerable from one read). version_no pins one version
// (its group is returned even when it carries no assertions); without it the
// read is an inventory of the object's evidence-bearing versions.
//
// The gate runs FIRST — before the object is even resolved — so a denied
// reader (or an unknown project) answers the existence-hiding
// PROJECT_NOT_FOUND for every object id and every version_no: whether the
// object or its version exists stays indistinguishable from the project not
// existing (docs/45, the same gate-first order every other read surface
// uses). Once the gate passes, the service resolves the object, checks it
// belongs to the path project BEFORE any version lookup, and reads the
// evidence.
//
// The reader is then handed to the read as well (ADR-024), and it is not the
// same question the gate asked: the gate decided the PROJECT was readable,
// the read decides which of its evidence ROWS this reader may be shown. One
// resolved reader serves both, so a request cannot pass one gate as one
// caller and read as another.
func (h *handlers) handleObjectEvidence(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("projectId")
	objectID := r.PathValue("objectId")
	caller := reader(r)
	if _, err := h.gate.Get(r.Context(), caller, projectID); err != nil {
		evidenceError(w, r, err)
		return
	}
	versionNo, ok := parseVersionNo(w, r)
	if !ok {
		return
	}
	out, err := h.service.ObjectEvidence(r.Context(), caller, projectID, objectID, versionNo)
	if err != nil {
		evidenceError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusOK, out)
}

// handleHypothesisEvidence: GET
// /api/v1/projects/{projectId}/hypotheses/{objectId}/evidence — the
// hypothesis page: the assertions pinned to the hypothesis itself and the
// assertions of its subordinate claims, as TWO SEPARATE sections. Nothing is
// merged, counted or netted across them (docs/10 §4 「V1 不自动赋数值权重」;
// CLAUDE.md §9.13; the Supervisor's 2026-09-18 ruling).
//
// Same gate-first order as the object read above, and the same
// existence-hiding answer for an object outside the path project or for an
// object that is not a hypothesis. The reader travels into the read here too,
// for the same reason: the hypothesis page's evidence is read through the
// very same per-target query, so it is the very same rows and must obey the
// very same audience rule (ADR-024; see the note on the object read).
func (h *handlers) handleHypothesisEvidence(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("projectId")
	objectID := r.PathValue("objectId")
	caller := reader(r)
	if _, err := h.gate.Get(r.Context(), caller, projectID); err != nil {
		evidenceError(w, r, err)
		return
	}
	out, err := h.service.HypothesisEvidence(r.Context(), caller, projectID, objectID)
	if err != nil {
		evidenceError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusOK, out)
}

// parseVersionNo reads the optional version_no query parameter. It answers
// (nil, true) when the parameter is absent — "the whole object" — and writes
// the 400 itself when the value is not a positive integer.
func parseVersionNo(w http.ResponseWriter, r *http.Request) (*int, bool) {
	raw := r.URL.Query().Get("version_no")
	if raw == "" {
		return nil, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		authhttp.WriteError(w, r, http.StatusBadRequest, CodeValidation,
			"version_no must be a positive integer")
		return nil, false
	}
	return &n, true
}

// evidenceError maps read outcomes to the wire. The not-found outcomes reuse
// the owning surfaces' codes: a denied read and a nonexistent resource answer
// the same envelope (existence hiding, docs/45).
func evidenceError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, projects.ErrProjectNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, projects.CodeProjectNotFound, "project not found")
	case errors.Is(err, sciobjects.ErrObjectNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, sciobjects.CodeObjectNotFound, "object not found")
	case errors.Is(err, sciobjects.ErrVersionNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, sciobjects.CodeVersionNotFound, "object version not found")
	case errors.Is(err, ErrValidation):
		authhttp.WriteError(w, r, http.StatusBadRequest, CodeValidation, err.Error())
	default:
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, CodeUnavailable, "service unavailable")
	}
}
