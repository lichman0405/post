package attestationhttp

import (
	"net/http"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/attestations"
	"github.com/lichman0405/post/internal/observability"
)

// The public read (T0812): GET /api/v1/attestations/{attestationId}.
//
// # It is the simplest read on the platform, and that is the design
//
// There is no audience rule to apply here, and its absence is not an
// oversight. A knowledge publication needs one because publishing does not
// widen anything (owner ruling L3-20260916-1: 发布不等于公开) — a publication
// of a version whose own axis is restricted stays members-only. An attestation
// has no such second axis: it is not a statement ABOUT a payload that could be
// more or less visible, it IS the payload, and the command refuses to write
// one whose target is not already public. So the read is: resolve, render,
// done — and the only decision left is the one every read of an unknown id
// makes.
//
// # The two 404s are one 404
//
// A segment that is not a pid, and a pid that names no attestation, answer the
// SAME body and code. Anything else would let a caller walk the pid space and
// read the difference between "there is such an attestation" and "there is
// not" — the existence oracle the knowledge read's own comments record, closed
// here by never having a second answer to give.
//
// # The response is the projection, verbatim
//
// attestations.PublicAttestation is rendered as-is. It has no field for the
// attesting project, the basis state or the internal review, and the query
// behind it (ResolvePublicAttestation) selects none of the three — so there is
// no field to drop here, and this handler could not disclose the private side
// if it tried. What it does carry is the record's own statement about itself
// (attestations.Disclosure), which is the machine-readable half of
// "an attestation is not evidence".

// Wire codes (docs/45: stable codes, no dependency detail).
const (
	// CodeAttestationNotFound: the pid names no attestation. It is the one
	// code for "no such pid" and "not a pid at all", because they must be
	// indistinguishable.
	CodeAttestationNotFound = attestations.CodeAttestationNotFound
	// CodeAttestationUnavailable: the read failed, so no honest answer can
	// be built. A page built over a failed read would report facts about a
	// repository nobody finished looking at.
	CodeAttestationUnavailable = attestations.CodeServiceUnavailable
)

// handleAttestation serves GET /api/v1/attestations/{attestationId}.
//
// The segment in the path is the attestation's PID. A segment that is not a
// pid names no attestation, and the store answers "not found" for it without
// touching the database — a uuid-shaped or slug-shaped segment cannot resolve
// here by construction.
func (h *handlers) handleAttestation(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("attestationId")
	entry, found, err := h.read.GetPublicAttestation(r.Context(), pid)
	if err != nil {
		observability.LoggerFromContext(r.Context()).Error("attestation read: resolve failed", "error", err, "pid", pid)
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, CodeAttestationUnavailable,
			"attestation data is temporarily unavailable")
		return
	}
	if !found {
		writeAttestationNotFound(w, r)
		return
	}
	authhttp.WriteJSON(w, http.StatusOK, entry)
}

// writeAttestationNotFound answers the existence-hiding 404.
func writeAttestationNotFound(w http.ResponseWriter, r *http.Request) {
	authhttp.WriteError(w, r, http.StatusNotFound, CodeAttestationNotFound, "attestation not found")
}
