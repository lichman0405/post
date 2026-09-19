package embedding

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Dimensions is the width of search_documents.embedding
// (infra/migrations/00013_search_projection.sql: `embedding vector(1536)`).
//
// It is a property of the COLUMN, not of any provider: whatever computes a
// vector must produce exactly this many floats, because a 1024-wide vector
// cannot be stored in a 1536-wide column. The batch job checks it by name
// and fails with the provider's own identity in the message, rather than
// letting PostgreSQL report a type mismatch somewhere below it.
const Dimensions = 1536

// Model identifies the embedder that produced a vector, and it is the
// reason the vectors are not just a number list.
//
// A vector is only meaningful against the model that produced it: comparing
// two vectors from different models, or serving a vector computed by a
// model that has been replaced, is a silent correctness failure — the
// numbers are still there and the distances are still numbers. Recording
// the identity on the row is what lets the batch job ask the only question
// that matters for a recompute ("is this row's vector the current model's?")
// and what lets an operator see, after switching models, which rows have
// been recomputed and which have not.
//
// The three parts are the provider's own words, stored verbatim: POST does
// not renumber or rename an external identity (docs/52). All three are
// compared, not just Version — two different implementations can ship the
// same version label.
type Model struct {
	// Provider is which implementation computed the vector, e.g. the
	// in-process one ("post-local") or a hosted service's name.
	Provider string
	// Name is that implementation's own name for the model.
	Name string
	// Version is that implementation's own revision of Name.
	Version string
}

// String renders the identity as "provider/name@version" for a log line or
// a command's report — one token, so a log field never needs three.
func (m Model) String() string {
	return m.Provider + "/" + m.Name + "@" + m.Version
}

// ErrModelIncomplete is returned by Model.Validate: an embedder that cannot
// say which model it is cannot be used, because every row it wrote would be
// indistinguishable from a row written by any other model.
var ErrModelIncomplete = errors.New("embedding: model identity is incomplete")

// Validate reports whether m is usable as an identity. It fails closed on
// any empty part: an empty version is not "unspecified", it is an identity
// that cannot distinguish a model change, which is precisely what the
// metadata exists to do.
func (m Model) Validate() error {
	var missing []string
	if m.Provider == "" {
		missing = append(missing, "provider")
	}
	if m.Name == "" {
		missing = append(missing, "name")
	}
	if m.Version == "" {
		missing = append(missing, "version")
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: %s", ErrModelIncomplete, strings.Join(missing, ", "))
	}
	return nil
}

// Embedder is the embedding port: the one seam through which vectors enter
// POST (docs/20 §5, docs/52 §5).
//
// It is deliberately smaller than a provider SDK: POST asks for vectors for
// texts and asks which model produced them, and nothing about a provider's
// request shape, batching, auth or error taxonomy reaches the callers.
// Implementations are expected to be safe for concurrent use by multiple
// goroutines, though the batch job calls one from a single goroutine today.
type Embedder interface {
	// Model returns the identity of the model this embedder calls. It is
	// recorded on every row the batch job writes, and compared against the
	// stored identity to decide whether a row needs recomputing, so it must
	// be stable for the lifetime of the embedder.
	Model() Model
	// Embed returns one vector per input text, in the same order and with
	// the same length, each exactly Dimensions wide. A provider that cannot
	// answer for a batch returns an error and no vectors — a partial result
	// would let the caller write half a batch and call it a success.
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// FormatVector renders v in pgvector's text input syntax, "[f1,f2,...]".
//
// This is the wire form the batch job writes: the column's type is not one
// the pgx driver knows, so the vector crosses the boundary as text and the
// server parses it (see sqlc.yaml's `vector` override for the generation
// half of the same decision). It is exported because it defines that
// boundary and because the tests compare stored vectors through it.
//
// The values must be finite; the batch job rejects a vector containing NaN
// or an infinity before formatting it, since pgvector would reject it too
// and this way the error names the provider instead of the parser.
func FormatVector(v []float32) string {
	var b strings.Builder
	b.Grow(len(v) * 12)
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		// 32-bit precision: the column is float4, so this is the shortest
		// representation that survives the round trip exactly.
		b.WriteString(strconv.FormatFloat(float64(f), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

// TextFor is the text a document is embedded from: its title and content,
// joined by a newline.
//
// It is the same text search_documents is full-text indexed over
// (internal/persistence/queries/search.sql: `to_tsvector(title || ' ' ||
// content)`), so a vector and an FTS rank describe the same document text
// rather than two different halves of it. The newline (rather than the FTS
// query's space) keeps the title from being glued to the first word of the
// content in a way a tokenizer cannot undo.
func TextFor(title, content string) string {
	return title + "\n" + content
}
