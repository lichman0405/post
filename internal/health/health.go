// Package health provides the POST health surface shared by the Go HTTP
// services: GET /healthz (process liveness only) and GET /readyz
// (dependency readiness with per-dependency truth).
//
// Contract (docs/26, T0006):
//
//   - /healthz never depends on a downstream service: it answers 200 while
//     the process is alive, no probes run;
//   - /readyz runs every registered probe and answers 200 only when all
//     dependencies are reachable; any failed probe answers 503 with the
//     per-dependency state in the body — a down dependency is reported
//     truthfully, never as a crash and never as a false "ok";
//   - probe error strings appear verbatim in the response detail, so probes
//     must never put credential values into an error (they carry connection
//     outcomes only).
package health

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/lichman0405/post/internal/version"
)

// Probe is one readiness check: a downstream dependency the service can
// actually reach. Check must return nil when the dependency is up and an
// error (never containing a secret) when it is not.
type Probe struct {
	Name  string
	Check func(ctx context.Context) error
}

// Handler serves GET /healthz and GET /readyz for one service.
type Handler struct {
	service string
	probes  []Probe
	timeout time.Duration
}

// NewHandler builds the health surface for service. Each probe gets
// timeout (default 2s) to answer; probes run sequentially.
func NewHandler(service string, probes ...Probe) *Handler {
	return &Handler{service: service, probes: probes, timeout: 2 * time.Second}
}

// WithProbeTimeout overrides the per-probe timeout (default 2s).
func (h *Handler) WithProbeTimeout(d time.Duration) *Handler {
	if d > 0 {
		h.timeout = d
	}
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/healthz":
		h.healthz(w)
	case r.Method == http.MethodGet && r.URL.Path == "/readyz":
		h.readyz(w, r.Context())
	case r.URL.Path == "/healthz" || r.URL.Path == "/readyz":
		writeJSON(w, http.StatusMethodNotAllowed,
			map[string]string{"error": "method not allowed: use GET"})
	default:
		writeJSON(w, http.StatusNotFound,
			map[string]string{"error": "not found"})
	}
}

type healthPayload struct {
	Service string `json:"service"`
	Status  string `json:"status"`
	Version string `json:"version"`
}

// healthz reports process liveness only. It must not depend on downstream
// services: no probe runs on this path.
func (h *Handler) healthz(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, healthPayload{
		Service: h.service,
		Status:  "ok",
		Version: version.Version,
	})
}

type checkState struct {
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

type readyPayload struct {
	Service string                `json:"service"`
	Status  string                `json:"status"`
	Version string                `json:"version"`
	Checks  map[string]checkState `json:"checks"`
}

// readyz runs every probe and reports the truth: 200 only when every
// dependency answered; 503 with per-check detail otherwise. A failing
// probe never takes the process down.
func (h *Handler) readyz(w http.ResponseWriter, ctx context.Context) {
	checks := make(map[string]checkState, len(h.probes))
	allUp := true
	for _, p := range h.probes {
		probeCtx, cancel := context.WithTimeout(ctx, h.timeout)
		err := p.Check(probeCtx)
		cancel()
		state := checkState{Status: "up"}
		if err != nil {
			state = checkState{Status: "down", Detail: err.Error()}
			allUp = false
		}
		checks[p.Name] = state
	}

	payload := readyPayload{
		Service: h.service,
		Status:  "ready",
		Version: version.Version,
		Checks:  checks,
	}
	code := http.StatusOK
	if !allUp {
		payload.Status = "not_ready"
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, payload)
}

// writeJSON answers with a JSON body; the writer is sorted by key so the
// output is deterministic (checks is a map).
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "")
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}
