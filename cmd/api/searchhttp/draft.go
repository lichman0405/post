package searchhttp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/researchcontext"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/observability"
)

// The two suffix verbs of the Draft Research Context flow
// (specs/api/openapi.yaml). They are literal suffixes inside a path segment,
// which ServeMux cannot express as a wildcard ("{searchId}:start-project"
// panics with "bad wildcard segment"), so each collection is registered as ONE
// remainder wildcard and this file splits the suffix off — the same shape
// cmd/api/mergehttp uses for ":merge". A segment without a known suffix is the
// 404 it has always been; no route is widened by the trick.
const (
	// startProjectVerb: POST /api/v1/search/{searchId}:start-project.
	startProjectVerb = ":start-project"
	// confirmVerb: POST /api/v1/research-context-drafts/{draftId}:confirm.
	confirmVerb = ":confirm"
	// idempotencyHeader is the header both routes require
	// (components.parameters.IdempotencyKey: required, minLength 8) — and the
	// same spelling cmd/api/mergehttp uses for its own route.
	idempotencyHeader = "Idempotency-Key"
	// minIdempotencyKeyLen is that parameter's minLength. It is checked here
	// (a caller learns the shape of its own request) and again in the
	// database (00134's CHECK), which is the ordinary pair.
	minIdempotencyKeyLen = 8
)

// draftDeadline bounds one of these writes. It is looser than a search's 10s
// because a confirmation runs a state transition (branch + object + commit)
// rather than a read pipeline, and shorter than a merge's budget because the
// work is smaller. It is an outer bound: the store's own statements are what
// actually finish the call.
const draftDeadline = 20 * time.Second

// DraftCommands is the flow's two commands as this transport needs them. The
// interface exists so the handlers are unit-testable against a fake; the
// production value is *researchcontext.Service.
type DraftCommands interface {
	Start(ctx context.Context, actor domain.User, in researchcontext.StartInput) (researchcontext.StartResult, error)
	Confirm(ctx context.Context, actor domain.User, in researchcontext.ConfirmInput) (researchcontext.ConfirmResult, error)
}

// ProvisionEnqueuer is the best-effort hook the start route runs after it
// creates a project: the T0301 provisioning job, so the new project gets its
// GitProvider repository without waiting for the API's startup sweep.
//
// It is a function of the project id rather than a queue interface because the
// job's shape (its type, its payload, its correlation id) is cmd/api's own
// wiring concern (newProvisionJob in cmd/api/provisioning.go): a search
// surface that imported the provisioning domain to build one would be a second
// place that knows how projects are provisioned, and the two would drift.
//
// Optional: nil disables the hook (a deployment with no queue, a unit test),
// and the project row stays the source of truth — provision_status is
// 'pending', which the startup sweep back-fills. A failure is logged and never
// fails the request: the draft and the project are committed by then, and a
// queue that is down must not turn a created project into an error.
type ProvisionEnqueuer func(ctx context.Context, projectID string) error

// startProjectRequest is the contract's body for the start route: `name`,
// `slug` and `research_question` required, `visibility` the contract's enum,
// and the five lists docs/14:25 names. Unknown fields are ignored, as
// everywhere else in this API.
type startProjectRequest struct {
	Name             string   `json:"name"`
	Slug             string   `json:"slug"`
	Visibility       string   `json:"visibility"`
	ResearchQuestion string   `json:"research_question"`
	ReferencedRefs   []string `json:"referenced_refs"`
	DependencyRefs   []string `json:"dependency_refs"`
	CandidateRefs    []string `json:"candidate_refs"`
	Uncertainties    []string `json:"uncertainties"`
	Hypotheses       []string `json:"hypotheses"`
}

// draftPayload is the draft document both routes answer with. It is the row's
// own fields (migration 00134): the six things docs/14:25 names, the status,
// and — once confirmed — the record of what the confirmation created.
//
// The confirmation fields are pointers so an unconfirmed draft renders them as
// null rather than as empty strings: "no initial state yet" and "an initial
// state whose id is empty" are different statements, and only the first one is
// true of a draft.
type draftPayload struct {
	ID               string    `json:"id"`
	ProjectID        string    `json:"project_id"`
	SearchID         string    `json:"search_id"`
	ResearchQuestion string    `json:"research_question"`
	ReferencedRefs   []string  `json:"referenced_refs"`
	DependencyRefs   []string  `json:"dependency_refs"`
	CandidateRefs    []string  `json:"candidate_refs"`
	Uncertainties    []string  `json:"uncertainties"`
	Hypotheses       []string  `json:"hypotheses"`
	Status           string    `json:"status"`
	CreatedAt        time.Time `json:"created_at"`

	ConfirmedAt               *time.Time `json:"confirmed_at"`
	InitialBranchID           *string    `json:"initial_branch_id"`
	InitialStateID            *string    `json:"initial_state_id"`
	InitialCommitID           *string    `json:"initial_commit_id"`
	ResearchQuestionObjectID  *string    `json:"research_question_object_id"`
	ResearchQuestionVersionID *string    `json:"research_question_version_id"`
}

// startProjectResponse is the contract's 201 for the start route: "body
// carries the draft id, the project id and the draft". `replayed` is the one
// addition — it tells a client that its retry was answered with the first
// call's result rather than creating anything, which is what an
// Idempotency-Key is FOR and what the client otherwise cannot tell.
type startProjectResponse struct {
	DraftID   string       `json:"draft_id"`
	ProjectID string       `json:"project_id"`
	Draft     draftPayload `json:"draft"`
	Replayed  bool         `json:"replayed"`
}

// confirmResponse is the contract's 201 for the confirm route: "body carries
// the branch id and the state commit". The remaining ids are the same
// transition's other members (the state it produced and the research_question
// object version it created), published so a client has the whole trail
// without a second request.
type confirmResponse struct {
	DraftID   string `json:"draft_id"`
	ProjectID string `json:"project_id"`
	BranchID  string `json:"branch_id"`
	// StateCommitID names the transition (docs/09 §2): the row to follow for
	// what the confirmation did.
	StateCommitID             string `json:"state_commit_id"`
	StateID                   string `json:"state_id"`
	ResearchQuestionObjectID  string `json:"research_question_object_id"`
	ResearchQuestionVersionID string `json:"research_question_version_id"`
	Replayed                  bool   `json:"replayed"`
}

// handleSearchVerb serves POST /api/v1/search/{rest...}: the search
// collection's suffix verbs. It is the dispatcher for the whole remainder
// because ServeMux allows exactly one owner of it, so an unknown suffix is
// answered here (404) instead of by a route that does not exist.
func (s *service) handleSearchVerb(w http.ResponseWriter, r *http.Request) {
	rest := r.PathValue("rest")
	if !strings.HasSuffix(rest, startProjectVerb) {
		http.NotFound(w, r)
		return
	}
	searchID := strings.TrimSuffix(rest, startProjectVerb)
	if searchID == "" || strings.Contains(searchID, "/") {
		// "/api/v1/search/:start-project" names no search, and a remainder
		// that crosses a segment boundary is not a verb on this collection.
		http.NotFound(w, r)
		return
	}
	s.handleStartProject(w, r, searchID)
}

// handleDraftVerb serves POST /api/v1/research-context-drafts/{rest...}: the
// drafts collection's suffix verbs, dispatched exactly like the search
// collection's.
func (s *service) handleDraftVerb(w http.ResponseWriter, r *http.Request) {
	rest := r.PathValue("rest")
	if !strings.HasSuffix(rest, confirmVerb) {
		http.NotFound(w, r)
		return
	}
	draftID := strings.TrimSuffix(rest, confirmVerb)
	if draftID == "" || strings.Contains(draftID, "/") {
		http.NotFound(w, r)
		return
	}
	s.handleConfirm(w, r, draftID)
}

// handleStartProject: POST /api/v1/search/{searchId}:start-project.
//
// The order is the order of what a caller is told: who is asking (the guard
// resolved it, this reads it), whether the key that makes the call replayable
// is there, whether the body is the contract's shape, then the command. The
// searchId is not shape-checked here: the application refuses an id that names
// no record with the same answer it gives another actor's record (see
// writeDraftError), so a pre-check would add nothing a caller can act on.
func (s *service) handleStartProject(w http.ResponseWriter, r *http.Request, searchID string) {
	actorID := authhttp.PrincipalID(r.Context())
	if actorID == "" {
		writeSearchError(w, r, errNoActor)
		return
	}
	if s.drafts == nil {
		// Fail closed: the surface is mounted without its flow (a wiring
		// mistake), and a route that cannot run must say so rather than
		// invent a result.
		writeDraftUnavailable(w, r)
		return
	}
	key, ok := draftKey(w, r)
	if !ok {
		return
	}
	req, ok := decodeStartProject(w, r)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), draftDeadline)
	defer cancel()

	res, err := s.drafts.Start(ctx, domain.User{ID: actorID}, researchcontext.StartInput{
		SearchID:         searchID,
		IdempotencyKey:   key,
		Name:             req.Name,
		Slug:             req.Slug,
		Visibility:       domain.ProjectVisibility(req.Visibility),
		ResearchQuestion: req.ResearchQuestion,
		ReferencedRefs:   req.ReferencedRefs,
		DependencyRefs:   req.DependencyRefs,
		CandidateRefs:    req.CandidateRefs,
		Uncertainties:    req.Uncertainties,
		Hypotheses:       req.Hypotheses,
	})
	if err != nil {
		writeDraftError(w, r, err)
		return
	}
	if !res.Replayed {
		s.enqueueProvisioning(r, res.Project.ID)
	}
	writeDraftJSON(w, r, http.StatusCreated, startProjectResponse{
		DraftID:   res.Draft.ID,
		ProjectID: res.Project.ID,
		Draft:     draftPayloadFrom(res.Draft),
		Replayed:  res.Replayed,
	})
}

// enqueueProvisioning runs the optional provisioning hook for a project this
// route just created. A replay does not call it (nothing was created), and a
// failure is logged, never returned — see ProvisionEnqueuer.
func (s *service) enqueueProvisioning(r *http.Request, projectID string) {
	if s.provision == nil {
		return
	}
	// Bounded and independent of the command's own deadline, which has
	// already been cancelled by the time the response is written: a slow
	// queue must not hold the request open past the answer it earned.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Second)
	defer cancel()
	if err := s.provision(ctx, projectID); err != nil {
		observability.LoggerFromContext(r.Context()).Warn(
			"research context: provisioning enqueue failed (the startup sweep back-fills)",
			"project_id", projectID, "error", err)
	}
}

// handleConfirm: POST /api/v1/research-context-drafts/{draftId}:confirm.
func (s *service) handleConfirm(w http.ResponseWriter, r *http.Request, draftID string) {
	actorID := authhttp.PrincipalID(r.Context())
	if actorID == "" {
		writeSearchError(w, r, errNoActor)
		return
	}
	if s.drafts == nil {
		writeDraftUnavailable(w, r)
		return
	}
	key, ok := draftKey(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), draftDeadline)
	defer cancel()

	res, err := s.drafts.Confirm(ctx, domain.User{ID: actorID}, researchcontext.ConfirmInput{
		DraftID:        draftID,
		IdempotencyKey: key,
	})
	if err != nil {
		writeDraftError(w, r, err)
		return
	}
	writeDraftJSON(w, r, http.StatusCreated, confirmResponse{
		DraftID:                   res.Draft.ID,
		ProjectID:                 res.Draft.ProjectID,
		BranchID:                  res.BranchID,
		StateCommitID:             res.StateCommitID,
		StateID:                   res.StateID,
		ResearchQuestionObjectID:  res.QuestionObjectID,
		ResearchQuestionVersionID: res.QuestionVersionID,
		Replayed:                  res.Replayed,
	})
}

// draftKey reads the contract-required Idempotency-Key, refusing a missing or
// too-short one in place (400). Both routes move a project's state or create
// one, and a request that cannot be replayed onto what it already produced is
// refused rather than performed twice.
func draftKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := r.Header.Get(idempotencyHeader)
	if key == "" {
		writeDraftError(w, r, requestError(idempotencyHeader+" is required: it is what makes a repeated request return the project (or the confirmation) it already created instead of creating a second one"))
		return "", false
	}
	if len(key) < minIdempotencyKeyLen {
		writeDraftError(w, r, requestError(idempotencyHeader+" must be at least 8 characters (specs/api/openapi.yaml)"))
		return "", false
	}
	return key, true
}

// decodeStartProject parses the body and checks the shape the contract
// declares: JSON, and the three required fields present. Content rules (a
// slug's form, a name's length, the question's minimum) stay where they are
// owned — the project surface and the research_question schema — with the two
// exceptions the application makes for reasons the caller cannot otherwise
// see (see researchcontext.Service.Start).
func decodeStartProject(w http.ResponseWriter, r *http.Request) (startProjectRequest, bool) {
	var req startProjectRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err := dec.Decode(&req); err != nil {
		writeDraftError(w, r, requestError("request body must be a JSON object with `name`, `slug` and `research_question`"))
		return startProjectRequest{}, false
	}
	return req, true
}

// writeDraftError maps the flow's outcomes onto the wire (docs/45): one code
// per failure shape, the cause logged for the ones the caller cannot act on.
//
// Two mappings are judgements worth reading:
//
//   - A search id that names no record and a search record that belongs to
//     another actor answer the SAME 409 with the SAME code. The contract lists
//     one 409 for this route ("The search record is not the caller's, or was
//     already started"); telling the two apart would let a caller enumerate
//     which search ids exist, which is exactly the disclosure docs/45's
//     existence-hiding rule forbids. The message covers both readings.
//   - A draft the caller may not confirm and a draft that does not exist answer
//     the same 404 (existence hiding, the shape
//     internal/application/projects' write path uses). specs/api/openapi.yaml
//     lists no 404 for the confirm route; that gap is reported rather than
//     closed by publishing a different answer for each case.
func writeDraftError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, errNoActor):
		authhttp.WriteError(w, r, http.StatusUnauthorized, CodeSearchUnauthenticated,
			"this route requires an authenticated session")
	case errors.Is(err, ErrInvalidRequest):
		// This transport's own refusals (the missing key, the body that is
		// not the contract's shape) carry their sentence in the error
		// (requestFault, handlers.go). They are the same class the search
		// route answers 400 for, and the same class
		// researchcontext.ErrValidation names one layer down — a request the
		// caller can fix. Without this case they would fall to the default
		// and be answered 503 retryable, which tells the caller to try again
		// with a request that will never work.
		authhttp.WriteError(w, r, http.StatusBadRequest, researchcontext.CodeValidationFailed, err.Error())
	case errors.Is(err, researchcontext.ErrValidation):
		authhttp.WriteError(w, r, http.StatusBadRequest, researchcontext.CodeValidationFailed, err.Error())
	case errors.Is(err, researchcontext.ErrUngroundedRef):
		// The ref is the caller's own input echoed back, so naming it
		// discloses nothing; the sentence says what to do about it.
		message := "a draft ref was not returned by the search it comes from"
		var ungrounded *researchcontext.UngroundedRefError
		if errors.As(err, &ungrounded) {
			message = "ref " + ungrounded.Ref + " was not returned by this search, so it cannot be carried by its draft (refs may only be chosen from the search's own selected refs)"
		}
		authhttp.WriteError(w, r, http.StatusBadRequest, researchcontext.CodeValidationFailed, message)
	case errors.Is(err, researchcontext.ErrSearchNotFound), errors.Is(err, researchcontext.ErrSearchNotOwned):
		authhttp.WriteError(w, r, http.StatusConflict, researchcontext.CodeSearchNotOwned,
			"this search record is not the caller's, or does not exist")
	case errors.Is(err, researchcontext.ErrAlreadyStarted):
		authhttp.WriteError(w, r, http.StatusConflict, researchcontext.CodeAlreadyStarted,
			"this search already started a draft research context")
	case errors.Is(err, researchcontext.ErrIdempotencyConflict):
		authhttp.WriteError(w, r, http.StatusConflict, researchcontext.CodeIdempotencyConflict,
			"this Idempotency-Key was used for a different request")
	case errors.Is(err, researchcontext.ErrDraftNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, researchcontext.CodeDraftNotFound,
			"draft research context not found")
	case errors.Is(err, researchcontext.ErrNotConfirmable):
		// The contract's own sentence for this 409 (specs/api/openapi.yaml).
		// It is deliberately not "already confirmed": that is one of the two
		// ways to be unconfirmable, and the other — the project's main branch
		// carries research state this draft did not make — would be a false
		// thing to say to the caller.
		authhttp.WriteError(w, r, http.StatusConflict, researchcontext.CodeNotConfirmable,
			"this draft research context is not in a state that can be confirmed")
	case errors.Is(err, projects.ErrSlugTaken):
		authhttp.WriteError(w, r, http.StatusConflict, projects.CodeProjectSlugTaken,
			"a project with this slug already exists")
	case errors.Is(err, projects.ErrForbidden):
		authhttp.WriteError(w, r, http.StatusForbidden, projects.CodeProjectForbidden,
			"you may not create a project here")
	case errors.Is(err, projects.ErrOrgNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, projects.CodeOrgNotFound, "organization not found")
	case errors.Is(err, projects.ErrOrgDeactivated):
		authhttp.WriteError(w, r, http.StatusConflict, projects.CodeOrgDeactivated,
			"the organization is deactivated")
	case errors.Is(err, projects.ErrProgramNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, projects.CodeProgramNotFound, "program not found")
	case errors.Is(err, projects.ErrProgramOrgMismatch):
		authhttp.WriteError(w, r, http.StatusBadRequest, projects.CodeProgramOrgMismatch,
			"the program does not belong to the project's organization")
	case errors.Is(err, projects.ErrValidation):
		authhttp.WriteError(w, r, http.StatusBadRequest, projects.CodeValidationFailed, err.Error())
	default:
		observability.LoggerFromContext(r.Context()).Error("research context: command failed", "error", err)
		writeDraftUnavailable(w, r)
	}
}

// writeDraftUnavailable is the answer for a dependency failure and for a
// surface mounted without its flow: one code, because the caller can act on
// neither.
func writeDraftUnavailable(w http.ResponseWriter, r *http.Request) {
	authhttp.WriteError(w, r, http.StatusServiceUnavailable, researchcontext.CodeServiceUnavailable,
		"the draft research context flow is temporarily unavailable")
}

// writeDraftJSON writes a success envelope, stating its own headers for the
// reason writeSearchJSON does: this response does not leave through
// authhttp's envelope writers, so nosniff is set here rather than assumed from
// the edge (internal/security's headers are the edge's, not this exit's).
//
// no-store: the body names a project the caller may have just created, and a
// shared cache holding it would hand the next reader a draft id that is not
// theirs to ask about.
func writeDraftJSON(w http.ResponseWriter, r *http.Request, status int, body any) {
	doc, err := json.Marshal(body)
	if err != nil {
		observability.LoggerFromContext(r.Context()).Error("research context: response marshal failed", "error", err)
		writeDraftUnavailable(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(doc)
}

// draftPayloadFrom renders one stored draft for the wire. Every field is the
// stored value; nothing is derived (a replay must answer what the first call
// answered).
func draftPayloadFrom(d researchcontext.Draft) draftPayload {
	return draftPayload{
		ID:                        d.ID,
		ProjectID:                 d.ProjectID,
		SearchID:                  d.SearchID,
		ResearchQuestion:          d.ResearchQuestion,
		ReferencedRefs:            d.ReferencedRefs,
		DependencyRefs:            d.DependencyRefs,
		CandidateRefs:             d.CandidateRefs,
		Uncertainties:             d.Uncertainties,
		Hypotheses:                d.Hypotheses,
		Status:                    d.Status,
		CreatedAt:                 d.CreatedAt,
		ConfirmedAt:               d.ConfirmedAt,
		InitialBranchID:           d.InitialBranchID,
		InitialStateID:            d.InitialStateID,
		InitialCommitID:           d.InitialCommitID,
		ResearchQuestionObjectID:  d.QuestionObjectID,
		ResearchQuestionVersionID: d.QuestionVersionID,
	}
}
