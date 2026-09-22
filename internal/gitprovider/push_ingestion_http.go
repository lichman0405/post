package gitprovider

import (
	"errors"
	"io"
	"net/http"

	"github.com/lichman0405/post/internal/observability"
)

// The push webhook receiver (T0305): POST /api/v1/git/hooks/gitea — the
// delivery target T0301 registers on every provisioned repository
// (POST_GITEA_WEBHOOK_URL). cmd/api registers it on the ROOT mux, outside
// the session/CSRF guard: the provider is a machine, not a browser — the
// HMAC signature over the raw body, verified against the per-repository
// secret in git_repository_provisions, IS the authentication. A forged,
// missing or mis-verified signature answers 401 and is ingested nothing.
//
// Response bodies are always empty: the provider records the status code
// and redelivers non-2xx (with a fresh delivery id — the ingestion's
// dedupe key is the pushed head, so a redelivery that reached us twice is
// a no-op either way). 404 means "this platform does not own the
// repository" — retrying cannot fix it; 403 means "refused by a rule" (a
// direct write to a frozen main, T0601) — retrying cannot fix that either,
// and nothing was written; 5xx means "not recorded, retry" (database down,
// provider fetch failure) — the provider's redelivery is the bounded retry
// policy.

// maxDeliveryBody bounds one delivery body (4 MiB — the payload carries
// paths and a truncated commit list, never file content).
const maxDeliveryBody = 4 << 20

// refusalSurface is this handler's name in post_permission_denials_total
// (docs/26 §3 "permission denied rates"). The surface label is a closed set
// maintained at the sites that consume a refusal — "api" in
// cmd/api/authhttp/envelope.go, "inbox" in internal/events/inbox_store.go —
// and this receiver is the third.
//
// It is counted here rather than at the envelope because it is the one
// refusal site the envelope cannot see: the webhook route is registered on
// the ROOT mux, outside the session/CSRF guard (the provider is a machine
// and the HMAC over the raw body is its authentication), so it answers by
// writing the status directly instead of going through authhttp.WriteError.
// Counting a refusal at the point it is decided, for the one receiver the
// boundary does not wrap, is what keeps "every product refusal in this
// binary is counted" a statement about the tree rather than about the
// call sites that happened to be audited.
const refusalSurface = "gitprovider"

// NewPushWebhookHandler wires the receiver: the ingester performs
// inspection + classification, the store resolves the secret. The handler
// itself is transport-only: read raw bytes, verify, answer. Logging is
// request-scoped through the observability middleware's context logger.
func NewPushWebhookHandler(ingester *PushIngester, store IngestStore) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqLog := observability.LoggerFromContext(r.Context())
		// The signature verifies the RAW body — read it before anything
		// parses or re-encodes it, and read it whole (a chunked read
		// would still be the same bytes, but the provider signs what it
		// sent; reading through LimitReader keeps a hostile body bounded).
		body, err := io.ReadAll(io.LimitReader(r.Body, maxDeliveryBody))
		if err != nil {
			reqLog.Warn("api: webhook delivery unreadable", "error", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		// Non-push events: T0301 registers push only; anything else is a
		// provider-side misconfiguration — the delivery succeeded, there
		// is nothing to ingest, and answering an error would redeliver a
		// no-op forever.
		if r.Header.Get("X-Gitea-Event") != "push" {
			reqLog.Debug("api: webhook delivery ignored (not a push event)",
				"event", r.Header.Get("X-Gitea-Event"))
			w.WriteHeader(http.StatusNoContent)
			return
		}

		signature := r.Header.Get("X-Gitea-Signature")
		deliveryID := r.Header.Get("X-Gitea-Delivery")
		if signature == "" {
			reqLog.Warn("api: webhook delivery rejected (no signature)", "delivery_id", deliveryID)
			observability.Default().ObserveRefusal(refusalSurface, http.StatusUnauthorized)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		// The signature is verified BEFORE anything in the payload is
		// trusted or interpreted: every push delivery — branch or not — is
		// either authenticated here or rejected. ErrNotABranchPush still
		// carries the parsed event so the repository id is available for
		// the secret lookup and the check below.
		ev, parseErr := ParsePushEvent(body, deliveryID)
		if parseErr != nil && !errors.Is(parseErr, ErrNotABranchPush) {
			reqLog.Warn("api: webhook delivery rejected (bad payload)", "delivery_id", deliveryID, "error", parseErr)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		secret, err := store.WebhookSecretByRepoID(r.Context(), ev.RepositoryID)
		if errors.Is(err, ErrRepoNotProvisioned) {
			reqLog.Warn("api: webhook delivery for an unprovisioned repository",
				"delivery_id", deliveryID, "gitea_repo_id", ev.RepositoryID)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if err != nil {
			reqLog.Error("api: webhook secret lookup failed", "delivery_id", deliveryID, "error", err)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if !VerifyPushSignature(secret, body, signature) {
			reqLog.Warn("api: webhook delivery rejected (bad signature)",
				"delivery_id", deliveryID, "ref", ev.Ref)
			observability.Default().ObserveRefusal(refusalSurface, http.StatusUnauthorized)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		if errors.Is(parseErr, ErrNotABranchPush) {
			// A verified delivery for a ref this task does not ingest
			// (tags and ref deletions arrive as their own events on this
			// instance; this guard is defensive).
			reqLog.Debug("api: webhook delivery ignored (not a branch push)", "ref", ev.Ref)
			w.WriteHeader(http.StatusNoContent)
			return
		}

		inserted, err := ingester.Ingest(r.Context(), ev)
		if err != nil {
			// A delivery the frozen-main rule refuses (T0601) is answered
			// 403, distinct from the 5xx that means "not recorded, retry":
			// the refusal is decided from the project's flag and the ref
			// alone, so a redelivery of the same push will be refused in
			// exactly the same way, and the status is how an operator sees
			// that a direct write to a frozen main was attempted. The body
			// stays empty like every other answer here (the provider reads
			// status codes); the code travels in the log line.
			var frozen *MainFrozenRefusalError
			if errors.As(err, &frozen) {
				reqLog.Warn("api: webhook delivery refused (main is frozen)",
					"delivery_id", deliveryID, "ref", ev.Ref, "after", ev.After,
					"code", frozen.Code(), "gitea_repo_id", frozen.RepositoryID)
				// The one refusal in this binary an operator has no other
				// metric for: a direct write to a frozen main (T0601) is a
				// permission decision, and until it is counted here nothing
				// in post_permission_denials_total moves when somebody tries
				// it — post_http_requests_total records the 403 but carries
				// no decision class, so the denial-rate rule cannot see it.
				observability.Default().ObserveRefusal(refusalSurface, http.StatusForbidden)
				w.WriteHeader(http.StatusForbidden)
				return
			}
			// Not recorded — the provider's redelivery is the retry
			// (the dedupe key makes an already-recorded delivery a
			// no-op when it arrives again).
			reqLog.Error("api: webhook ingestion failed", "delivery_id", deliveryID,
				"ref", ev.Ref, "after", ev.After, "error", err)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if inserted {
			reqLog.Info("api: webhook delivery ingested", "delivery_id", deliveryID,
				"ref", ev.Ref, "after", ev.After)
		} else {
			reqLog.Info("api: webhook delivery duplicate (no-op)", "delivery_id", deliveryID,
				"ref", ev.Ref, "after", ev.After)
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
