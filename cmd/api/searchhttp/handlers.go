package searchhttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/observability"
)

// Wire codes (docs/45: stable codes, no dependency detail).
const (
	// CodeSearchInvalidRequest: the request is not a search that can be run
	// as given (no question, a filter that is not an object, an unknown entity
	// type, a limit out of range). The caller can fix it.
	CodeSearchInvalidRequest = "SEARCH_INVALID_REQUEST"
	// CodeSearchUnauthenticated: the route is behind the auth guard
	// (specs/api's global `security: [{bearerAuth: []}]` applies to POST
	// /search, which is not marked `security: []`), so this is only reachable
	// if the guard was bypassed at wiring time. It is a code rather than a
	// panic for the same reason the check exists: the answer to "who is
	// searching" must never be "nobody", and an unanswered question is a 401.
	CodeSearchUnauthenticated = "SEARCH_UNAUTHENTICATED"
	// CodeSearchUnavailable: a dependency failed. The cause is logged and not
	// returned.
	CodeSearchUnavailable = "SEARCH_UNAVAILABLE"
	// CodeSearchRecordFailed: the answer was produced and could not be
	// recorded, so it is not returned (service.go). Its own code because it
	// is the one failure where the platform DID the work and chose not to
	// hand it over — a retry is a new search either way, but an operator
	// reading the logs needs to see the write, not the read.
	CodeSearchRecordFailed = "SEARCH_RECORD_FAILED"
)

// maxBodyBytes bounds the request body. It is generous for a question (64 KiB
// is a small paper) and it is a bound because an unbounded JSON decoder on a
// public route is a way to spend the server's memory from outside.
const maxBodyBytes = 64 << 10

// searchDeadline bounds one whole search, and the number is docs/27 §SLO's:
// "full answer < 10s, timeout → structured fallback". It is the OUTER bound
// and it is deliberately larger than the sum of the inner ones — the planner
// takes 2s of it (planner.DefaultTimeout) and the answer 6s
// (answer.DefaultTimeout), leaving 2s for retrieval and ranking, whose own
// SLO is p95 < 2s. The nesting is what makes the fallbacks work: each step
// times out into ITS fallback and the caller's context is the backstop for
// the case where the steps together still exceed the budget.
const searchDeadline = 10 * time.Second

// searchRequest is the contract's request document
// (specs/api/openapi.yaml, POST /search): `query` required, `filters` an
// optional object.
//
// The two fields are decode targets and nothing else — the handler turns them
// into the pipeline's arguments immediately, so there is no request type
// drifting away from what the pipeline is given.
type searchRequest struct {
	Query string `json:"query"`
	// Filters is kept as raw bytes: the platform stores and matches the
	// caller's filter verbatim (its shape is the retrieval's business, see
	// decodeSearch), so decoding it into a map here would rewrite what the
	// caller sent (numbers through float64, key order lost) before anything
	// that understands it saw it.
	Filters json.RawMessage `json:"filters"`
}

// searchResponse is the contract's response: the search's id and the answer.
//
// The answer is nested rather than flattened into the envelope, and it is
// json.RawMessage rather than a struct, because the answer document is the
// ANSWER LAYER's canonical rendering (answer.Answer.CanonicalJSON, pinned by
// tests/answer's golden fixtures). Re-encoding it here through a local struct
// would give the platform a second definition of an answer's shape, and the
// fixtures would then be pinning bytes that no caller ever sees.
//
// search_id is what makes the response addressable: POST
// /search/{searchId}:start-project resumes these sources (T0908), and the
// record behind the id is written before this document is (service.go).
type searchResponse struct {
	SearchID string          `json:"search_id"`
	Answer   json.RawMessage `json:"answer"`
}

// handleSearch serves POST /api/v1/search.
//
// The order is the order of what a caller is told: who is asking (the guard
// resolved it, this reads it), whether the request is the contract's shape,
// then the search itself. Nothing is logged as a failure until a dependency
// has actually failed — a caller's 400 is not a platform event.
func (s *service) handleSearch(w http.ResponseWriter, r *http.Request) {
	actorID := authhttp.PrincipalID(r.Context())
	if actorID == "" {
		writeSearchError(w, r, errNoActor)
		return
	}

	req, ok := decodeSearch(w, r)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), searchDeadline)
	defer cancel()

	ans, searchID, err := s.search(ctx, actorID, strings.TrimSpace(req.Query), req.Filters)
	if err != nil {
		writeSearchError(w, r, err)
		return
	}
	doc, err := ans.CanonicalJSON()
	if err != nil {
		// Unreachable while the answer carries only JSON-representable
		// fields; kept because "the struct is encodable" is not a reason to
		// write bytes the answer layer did not render.
		observability.LoggerFromContext(r.Context()).Error("search: answer render failed", "error", err)
		writeSearchError(w, r, ErrUnavailable)
		return
	}
	writeSearchJSON(w, r, http.StatusOK, searchResponse{SearchID: searchID, Answer: doc})
}

// decodeSearch parses the request body and checks the shape the contract
// declares. It answers the caller itself on failure and reports whether the
// handler should continue.
//
// # What is checked here and what is not
//
// Here: that the body is JSON, that `query` is a string, and that `filters`,
// when present, is a JSON OBJECT — which is exactly what specs/api's schema
// says (`filters: {type: object}`). Not here: whether the question is usable,
// whether the filter matches anything, whether the entity types exist. Those
// are the retrieval's judgements about what a request MEANS, and the
// transport does not pre-empt them with a second copy of the rule.
//
// The one thing this function does beyond the contract's types is refuse a
// blank question before the pipeline runs. It is not a semantic judgement but
// the same check retrieval.Request.normalize makes (ErrNoQuery), moved
// earlier on purpose: the planner is called BEFORE the retrieval, so a blank
// question would otherwise cost a provider call that the retrieval is certain
// to reject afterwards.
//
// Unknown top-level fields are ignored, as everywhere else in this API: a
// client that sends a field the contract does not have is sending a request
// the platform can still answer, and refusing it would be a stricter reading
// than the contract's.
func decodeSearch(w http.ResponseWriter, r *http.Request) (searchRequest, bool) {
	var req searchRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err := dec.Decode(&req); err != nil {
		writeSearchError(w, r, requestError("request body must be a JSON object with a `query` string"))
		return searchRequest{}, false
	}
	if strings.TrimSpace(req.Query) == "" {
		writeSearchError(w, r, requestError("`query` is required"))
		return searchRequest{}, false
	}
	// RawMessage is nil when the field is absent and the four bytes "null"
	// when it is present and null; both mean "no filter", which is the
	// contract's optional field.
	if len(req.Filters) > 0 && !bytes.Equal(bytes.TrimSpace(req.Filters), []byte("null")) {
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(req.Filters, &probe); err != nil {
			writeSearchError(w, r, requestError("`filters` must be an object"))
			return searchRequest{}, false
		}
		req.Filters = bytes.TrimSpace(req.Filters)
	} else {
		req.Filters = nil
	}
	return req, true
}

// requestError builds an invalid-request error whose message is safe to
// return: it names the field and never the value the caller sent.
func requestError(msg string) error {
	return &requestFault{msg: msg}
}

// requestFault is an ErrInvalidRequest that carries a caller-facing sentence.
// It exists so the wire message for a malformed body is written next to the
// check that found it rather than in a second switch in writeSearchError.
type requestFault struct{ msg string }

func (e *requestFault) Error() string { return e.msg }
func (e *requestFault) Is(target error) bool {
	return target == ErrInvalidRequest
}

// writeSearchError maps a pipeline error onto the wire envelope (docs/45):
// one code per failure shape, and every non-caller failure logged with its
// cause.
func writeSearchError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, errNoActor):
		authhttp.WriteError(w, r, http.StatusUnauthorized, CodeSearchUnauthenticated,
			"search requires an authenticated session")
	case errors.Is(err, ErrInvalidRequest):
		message := "the search request is not valid"
		var fault *requestFault
		if errors.As(err, &fault) {
			message = fault.msg
		}
		authhttp.WriteError(w, r, http.StatusBadRequest, CodeSearchInvalidRequest, message)
	case errors.Is(err, ErrRecordFailed):
		observability.LoggerFromContext(r.Context()).Error("search: the answer could not be recorded",
			"error", err)
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, CodeSearchRecordFailed,
			"the search could not be recorded, so it was not answered")
	default:
		observability.LoggerFromContext(r.Context()).Error("search: pipeline failed", "error", err)
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, CodeSearchUnavailable,
			"search is temporarily unavailable")
	}
}

// writeSearchJSON writes a success envelope.
//
// It states its own headers rather than leaning on the layer above it. This
// response does not leave through authhttp's envelope writers — the body is
// the contract's searchResponse and the answer inside it is the answer
// layer's own canonical rendering, so there is nothing for those writers to
// wrap — which is exactly why nosniff is set here: it is what pins the media
// type as data, and an exit that is safe only because internal/security's
// edge added the header is an exit that is unsafe everywhere else it is
// mounted (tests/security/exits_test.go classifies this function and probes
// it without the edge).
//
// An answer is never cached by a shared cache: it is the caller's own scoped
// result (a private project's sources reach no other reader), it changes as
// the corpus does, and the citation trail it publishes is recorded under this
// actor rather than derived from the bytes.
func writeSearchJSON(w http.ResponseWriter, r *http.Request, status int, body any) {
	doc, err := json.Marshal(body)
	if err != nil {
		observability.LoggerFromContext(r.Context()).Error("search: response marshal failed", "error", err)
		authhttp.WriteError(w, r, http.StatusInternalServerError, CodeSearchUnavailable,
			"search is temporarily unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// nosniff, for the same reason every envelope writer states it: a JSON
	// document is data, and the browser must not be free to decide otherwise
	// from the bytes.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(doc)
}
