package authhttp

import (
	"encoding/json"
	"net/http"

	"github.com/lichman0405/post/internal/observability"
)

// Error envelope (docs/22 §5, docs/45): one shape for every API failure —
// code, human message, the request id, optional details. Raw dependency
// errors (SQL, Redis, provider text) never reach this envelope; they are
// logged and mapped to a stable code.
type errorEnvelope struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
	Retryable bool   `json:"retryable"`
}

// WriteError renders the envelope with the given status. The message is
// client-safe by construction: callers pass fixed strings, never error
// text from a dependency. Exported so product packages (orgshttp) render
// the identical error shape.
//
// This function is also the permission-denial counter's insertion point
// (docs/26 §3 "permission denied rates"). Every product refusal in this
// binary — 400 call sites across 40 handler packages, plus the guard's own
// CSRF and Origin refusals — leaves through here, so counting the verdict
// where it is CONSUMED costs one call and cannot drift from the handlers.
//
// "Every" is a claim about the tree, so here is the check, and here is where
// it stops. In non-test code under cmd/ and internal/:
//
//	grep -rnE 'Status(Unauthorized|Forbidden)' --include='*.go' cmd internal \
//	  | grep -v _test.go | grep -v 'authhttp.WriteError'
//
// returns 14 files. Twelve of them write nothing to a client, and each is one
// of: this function's own callers (auth_middleware, auth_handler — they land
// here); code→status tables and helpers whose values are handed to rsgError,
// which is this function (rsghttp/handlers.go:520, reviewhttp:220,
// pullrequestshttp:329, and the freezehttp/aborthttp/mergehttp maps); readers
// of an UPSTREAM status (internal/gitprovider/gitea*.go turns Gitea's 401/403
// into a domain error); and this counter's own switch
// (internal/observability/metrics.go). The other two DO answer a client
// without this envelope, and they are named here rather than left for a
// reader to rediscover:
//
//  1. the push webhook receiver (internal/gitprovider/push_ingestion_http.go),
//     which writes the status directly because it is mounted on the root mux
//     outside the session/CSRF guard — the provider is a machine and the HMAC
//     over the raw body is its authentication, so there is no session for
//     this envelope to report on. That file counts its own three refusals
//     (401 no signature, 401 bad signature, 403 frozen main) under surface
//     "gitprovider", which is what makes this paragraph true rather than
//     approximately true: the frozen-main 403 is a permission decision an
//     operator must be able to alert on, and no other family carries it.
//  2. the fake identity provider in
//     internal/application/authn/oidctest/provider.go, which answers 401 as
//     the IdP rather than as POST. It is a test double — no non-test file
//     imports the package (grep 'authn/oidctest' outside _test.go returns
//     only doc comments) — so it is not a product refusal and is not counted.
//
// Which class is counted: 403 (forbidden) and 401 (unauthenticated) only.
// A 404 is NOT counted even when it is the refusal — the platform answers
// not-found for a denied read on purpose (internal/application/projects
// service.go: "Read existence hiding: a denied read looks exactly like an
// unknown project"), so at this point a refusal and a genuine miss are the
// same value by construction and counting it as a denial would report every
// typo'd URL as an access-control event.
func WriteError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	observability.Default().ObserveRefusal("api", status)
	w.Header().Set("Content-Type", "application/json")
	// nosniff on every envelope, from the one function that renders every
	// envelope (T1106). The edge middleware (internal/security.Headers)
	// sends the same header on every response; stating it here too is what
	// makes the fact belong to the exit rather than to the chain — a
	// handler reached without the edge (a unit test, a future mount) still
	// cannot have its JSON reinterpreted as something executable.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorEnvelope{
		Code:      code,
		Message:   message,
		RequestID: requestID(r),
		Retryable: status == http.StatusServiceUnavailable,
	})
}

func requestID(r *http.Request) string {
	id, ok := observability.FromContext(r.Context())
	if !ok {
		return ""
	}
	return id.String()
}

// WriteJSON renders a success payload.
func WriteJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// logAuthFailure keeps one structured line per failed authentication for
// the security trail (docs/50: no credentials, ever).
func logAuthFailure(r *http.Request, code string) {
	log := observability.LoggerFromContext(r.Context())
	log.Warn("authentication rejected",
		"code", code, "method", r.Method, "path", r.URL.Path)
}
