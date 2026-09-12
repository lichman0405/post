package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"

	"github.com/lichman0405/post/internal/config"
	"github.com/lichman0405/post/internal/observability"
	"github.com/lichman0405/post/internal/worker"
)

// newJobHandler is the T0007 scaffold enqueue surface: POST /internal/jobs
// puts one job on the Redis queue so a request can be traced by correlation
// id across API -> queue -> worker (docs/26 §2).
//
// This endpoint exists because the domain-event outbox (docs/52 §17) has
// not landed yet; it is deliberately NOT part of the public API contract
// (specs/api/openapi.yaml). The outbox consumer replaces it when the event
// tasks arrive — see follow_up_issues in the task result.
//
// The correlation id is never read from the request body: it comes from the
// HTTP edge (the middleware in main.go), exactly like every other request.
func newJobHandler(queue *worker.RedisQueue, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqLog := observability.LoggerFromContext(r.Context())

		// 64 KiB cap: payloads carry identity/version/reference only
		// (docs/52 §17), never bulk data.
		body := http.MaxBytesReader(w, r.Body, 64<<10)
		defer body.Close()

		var in struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.NewDecoder(body).Decode(&in); err != nil {
			reqLog.Warn("api: job enqueue rejected (bad body)", "error", err)
			writeJSONError(w, http.StatusBadRequest, "request body must be a JSON object with a string \"type\"")
			return
		}
		if !jobTypeRe.MatchString(in.Type) {
			reqLog.Warn("api: job enqueue rejected (invalid type)", "job_type", in.Type)
			writeJSONError(w, http.StatusBadRequest, "job type must match [a-z][a-z0-9-]*")
			return
		}

		id, err := observability.NewRandomID()
		if err != nil {
			reqLog.Error("api: job id generation failed", "error", err)
			writeJSONError(w, http.StatusInternalServerError, "internal error")
			return
		}
		cid, ok := observability.FromContext(r.Context())
		if !ok {
			cid, err = observability.NewCorrelationID()
			if err != nil {
				reqLog.Error("api: correlation id generation failed", "error", err)
				writeJSONError(w, http.StatusInternalServerError, "internal error")
				return
			}
		}

		job := worker.Job{
			ID:            id,
			Type:          in.Type,
			CorrelationID: string(cid),
			Payload:       in.Payload,
		}
		if err := queue.Enqueue(r.Context(), job); err != nil {
			// The error text may embed connection details; redact with the
			// shared config redactor before it reaches the log or the
			// response (internal/config.RedactForOutput).
			reqLog.Error("api: job enqueue failed",
				"job_id", id, "job_type", in.Type,
				"error", config.RedactForOutput(err.Error()))
			writeJSONError(w, http.StatusServiceUnavailable,
				"job queue unavailable: "+config.RedactForOutput(err.Error()))
			return
		}

		reqLog.Info("api: job enqueued", "job_id", id, "job_type", in.Type)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"job_id":         id,
			"correlation_id": string(cid),
		})
	})
}

// jobTypeRe is the accepted job-type shape; the worker dead-letters any
// type it does not know, so the API only enforces the format here.
var jobTypeRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

func writeJSONError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
