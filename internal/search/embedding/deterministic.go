package embedding

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"math"
	"strings"
	"unicode"
)

// Deterministic is POST's V1 embedder: it computes a vector from the text
// in-process, with no provider, no network and no state, so the same text
// and the same Model always produce the same vector — in this process, in
// another process, and on another day.
//
// # What it is
//
// Each term of the text is hashed (together with the model identity) to one
// dimension and contributes a signed count there; the vector is then
// scaled to unit length. Two texts therefore share dimensions exactly when
// they share terms, so this behaves like a lexical similarity, and it is
// honest to say so: it is NOT a learned semantic model, and nothing in POST
// may treat it as one (CLAUDE.md §9: relevance never becomes causation, and
// no score is invented). It is what it is used as: a PLACEHOLDER wired into
// the worker until a real provider lands (the package documentation says why
// none does here), distinguishable in the data by embedding_provider =
// "post-local". What it does provide is a real, storable,
// comparable vector of the right width, produced by a real implementation of
// the port, so that the write path, the provenance columns, the recompute
// job and every consumer of search_documents.embedding can be built and
// tested without a network — which is the whole of what this task may ship
// (see the package documentation on why).
//
// # Why the model identity is mixed into every dimension
//
// Because the version-tracking property must hold unconditionally. If the
// identity were not part of the hash, the vector of a document with no
// indexable terms would be the same all-zero vector under every model, and
// "a new model version changes the vectors" would quietly be false for
// exactly those rows. Mixing it in makes two different models produce
// different vectors for every input, including the empty one, so a
// recompute after a model change is observable on every row.
type Deterministic struct {
	model Model
}

// NewDeterministic builds the in-process embedder for m, failing closed on
// an incomplete identity: the identity is written to every row this
// embedder touches, and a row whose provenance cannot distinguish a model
// change is a row the recompute pass can never repair.
func NewDeterministic(m Model) (Deterministic, error) {
	if err := m.Validate(); err != nil {
		return Deterministic{}, err
	}
	return Deterministic{model: m}, nil
}

// LocalModel is the identity of the in-process embedder this repository
// ships, and it names the implementation for what it is.
//
// It is a constant rather than configuration because it describes the
// implementation the binary contains: "post-local / sha256-bag / v1" is
// true of this code and of no other. A deployment that replaces the
// implementation replaces this identity with it — which is exactly the
// change the provenance columns exist to make visible, and why Model, not a
// string, is what the port returns.
var LocalModel = Model{Provider: "post-local", Name: "sha256-bag", Version: "v1"}

// Model returns the identity this embedder records.
func (d Deterministic) Model() Model { return d.model }

// Embed computes one vector per text. It never calls out and never blocks
// on anything but ctx: the work is bounded by the text lengths, so a batch
// of a few dozen documents is microseconds of hashing.
func (d Deterministic) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, text := range texts {
		// Cancellation is checked per text: the caller's context carries
		// the job's timeout, and a wedged caller must be able to stop a
		// long batch rather than wait for it.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		out[i] = d.vector(text)
	}
	return out, nil
}

// vector is the bag-of-terms projection of one text onto Dimensions, unit
// length, with the model identity as one implicit component.
func (d Deterministic) vector(text string) []float32 {
	v := make([]float32, Dimensions)
	d.add(v, "")
	for _, term := range terms(text) {
		d.add(v, term)
	}
	return normalize(v)
}

// add mixes one term's contribution into v: every (model, term) pair maps to
// one dimension and one sign, so the vector is a function of the set of
// terms and their counts — never of their order, and never of a clock, a
// map iteration order or a random seed.
func (d Deterministic) add(v []float32, term string) {
	h := sha256.New()
	// Length-prefixed so that ("a", "bc") and ("ab", "c") cannot collide in
	// the hash input.
	for _, part := range []string{d.model.Provider, d.model.Name, d.model.Version, term} {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(part)))
		h.Write(n[:])
		h.Write([]byte(part))
	}
	sum := h.Sum(nil)
	idx := binary.BigEndian.Uint32(sum[:4]) % Dimensions
	if sum[4]&1 == 0 {
		v[idx]--
	} else {
		v[idx]++
	}
}

// terms splits text into its indexable terms: lowercased runs of letters
// and digits. Splitting on everything else means punctuation and whitespace
// never become part of a term, and case never splits one term into two.
func terms(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// normalize scales v to unit length in place and returns it. A zero vector
// (no terms and no model component, which add guarantees cannot happen)
// stays zero rather than dividing by zero.
func normalize(v []float32) []float32 {
	var sum float64
	for _, f := range v {
		sum += float64(f) * float64(f)
	}
	if sum == 0 {
		return v
	}
	norm := float32(math.Sqrt(sum))
	for i := range v {
		v[i] /= norm
	}
	return v
}
