package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/lichman0405/post/internal/observability"
	"github.com/lichman0405/post/internal/worker"
)

// newJobTestQueue starts an in-process Redis-protocol server and a queue
// over it (the same pattern internal/worker's tests use).
func newJobTestQueue(t *testing.T) *worker.RedisQueue {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return worker.NewRedisQueue(client, "post-test")
}

// TestEnqueueJobPropagatesRequestCorrelationID proves the T0007 core path
// at the handler level: the request's correlation id (assigned at the edge
// by the middleware) is embedded in the job the request causes, echoed in
// the response, and present on every log line of the request.
func TestEnqueueJobPropagatesRequestCorrelationID(t *testing.T) {
	q := newJobTestQueue(t)
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	handler := observability.Middleware(log)(newJobHandler(q, log))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/internal/jobs",
		strings.NewReader(`{"type":"smoke","payload":{"ref":"rsg/1"}}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response body: %v", err)
	}
	cid := resp["correlation_id"]
	if _, ok := observability.ParseCorrelationID(cid); !ok {
		t.Fatalf("response correlation_id %q is not a valid id", cid)
	}
	if echoed := rec.Header().Get(observability.HeaderCorrelationID); echoed != cid {
		t.Errorf("response header %q != body correlation_id %q", echoed, cid)
	}

	// The queued job itself carries the same id — this is what the worker
	// consumes.
	job, _, err := q.Next(t.Context())
	if err != nil {
		t.Fatalf("queue Next: %v", err)
	}
	if job.CorrelationID != cid {
		t.Errorf("queued job correlation_id = %q, want request id %q", job.CorrelationID, cid)
	}
	if job.Type != "smoke" || job.ID == "" {
		t.Errorf("queued job = %+v, want type smoke and a generated id", job)
	}

	// Every request-scoped log line carries the id.
	out := buf.String()
	for _, want := range []string{
		`"correlation_id":"` + cid + `"`,
		`"msg":"api: job enqueued"`,
		`"msg":"http request completed"`,
		`"status":202`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log output missing %s:\n%s", want, out)
		}
	}
}

func TestEnqueueJobHonoursIncomingCorrelationID(t *testing.T) {
	q := newJobTestQueue(t)
	handler := observability.Middleware(slog.New(slog.DiscardHandler))(newJobHandler(q, slog.New(slog.DiscardHandler)))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/internal/jobs",
		strings.NewReader(`{"type":"smoke"}`))
	req.Header.Set(observability.HeaderCorrelationID, "web-trace-xyz")
	handler.ServeHTTP(rec, req)

	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["correlation_id"] != "web-trace-xyz" {
		t.Errorf("correlation_id = %q, want incoming id web-trace-xyz", resp["correlation_id"])
	}
	job, _, err := q.Next(t.Context())
	if err != nil {
		t.Fatalf("queue Next: %v", err)
	}
	if job.CorrelationID != "web-trace-xyz" {
		t.Errorf("queued job id = %q, want web-trace-xyz", job.CorrelationID)
	}
}

func TestEnqueueJobRejectsBadInputs(t *testing.T) {
	q := newJobTestQueue(t)
	handler := newJobHandler(q, slog.New(slog.DiscardHandler))

	cases := []struct {
		name string
		body string
		want int
	}{
		{"not json", `{`, http.StatusBadRequest},
		{"missing type", `{"payload":{}}`, http.StatusBadRequest},
		{"invalid type", `{"type":"Bad Type!"}`, http.StatusBadRequest},
		{"oversized body", `{"type":"smoke","payload":"` + strings.Repeat("x", 70<<10) + `"}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/internal/jobs", strings.NewReader(tc.body))
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d (body %s)", rec.Code, tc.want, rec.Body.String())
			}
			jobs, processing, _, _ := q.Depth(t.Context())
			if jobs != 0 || processing != 0 {
				t.Errorf("rejected request must enqueue nothing: jobs=%d processing=%d", jobs, processing)
			}
		})
	}
}

// TestEnqueueJobReportsRedisDownWithoutLeakingSecrets: a dead queue answers
// 503, the error text is redacted through config.RedactForOutput before it
// reaches the log, and the request still carries its correlation id.
func TestEnqueueJobReportsRedisDownWithRedaction(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"}) // nobody listens
	defer client.Close()
	q := worker.NewRedisQueue(client, "post-test").WithPollTimeout(time.Second)

	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	handler := observability.Middleware(log)(newJobHandler(q, log))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/internal/jobs",
		strings.NewReader(`{"type":"smoke"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body %s)", rec.Code, rec.Body.String())
	}
	out := buf.String()
	if !strings.Contains(out, `"msg":"api: job enqueue failed"`) {
		t.Errorf("failure not logged: %s", out)
	}
	if !strings.Contains(out, `"correlation_id"`) {
		t.Errorf("failure line lost the correlation id: %s", out)
	}
	// The response error must not carry a raw secret-shaped value even if
	// the queue error text did (defense in depth: the shared redactor runs
	// on every output path).
	if strings.Contains(rec.Body.String(), "ghp_") || strings.Contains(rec.Body.String(), "password=") {
		t.Errorf("unredacted credential in 503 body: %s", rec.Body.String())
	}
}
