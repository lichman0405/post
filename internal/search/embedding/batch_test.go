package embedding

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/persistence/sqlc"
)

// TestNewWorkerRefusesUnusableInputs: every one of these produces a row whose
// provenance cannot distinguish a model change, or a batch loop that cannot
// make progress, so construction refuses rather than starting a job that
// quietly writes unrepairable state.
func TestNewWorkerRefusesUnusableInputs(t *testing.T) {
	ok := mustEmbedder(t, model("v1"))
	// A pool that is never connected to anything: NewWorker only stores it,
	// so this test needs no database (pgxpool with no minimum connections
	// does not dial at construction).
	pool, err := pgxpool.New(context.Background(), "postgres://postgres@127.0.0.1:1/post")
	if err != nil {
		t.Fatalf("build a lazy pool: %v", err)
	}
	defer pool.Close()

	cases := map[string]func() (*Worker, error){
		"nil pool":       func() (*Worker, error) { return NewWorker(nil, ok) },
		"nil embedder":   func() (*Worker, error) { return NewWorker(pool, nil) },
		"batch size 0":   func() (*Worker, error) { return NewWorker(pool, ok, WithBatchSize(0)) },
		"batch size -1":  func() (*Worker, error) { return NewWorker(pool, ok, WithBatchSize(-1)) },
		"interval 0":     func() (*Worker, error) { return NewWorker(pool, ok, WithInterval(0)) },
		"interval -time": func() (*Worker, error) { return NewWorker(pool, ok, WithInterval(-1)) },
	}
	for name, build := range cases {
		if _, err := build(); err == nil {
			t.Errorf("%s: NewWorker accepted it", name)
		}
	}
	// The one case that must succeed, so the table above is not passing
	// because NewWorker refuses everything.
	w, err := NewWorker(pool, ok, WithBatchSize(3), WithInterval(time.Second))
	if err != nil {
		t.Fatalf("NewWorker refused a usable configuration: %v", err)
	}
	if w.Model() != ok.Model() {
		t.Fatalf("Worker.Model() = %v, want %v", w.Model(), ok.Model())
	}
	if w.batchSize != 3 {
		t.Fatalf("WithBatchSize(3) left batchSize = %d", w.batchSize)
	}
}

// TestCheckVectorsRefusesAProviderBreakingThePortContract drives the guard
// directly with the three ways an embedder can be wrong about its own
// contract. Without it each of these reaches the database: a short result
// would pair vectors with the wrong rows, and a wrong width or a NaN would
// come back as a pgvector parse error naming the value rather than the
// provider.
func TestCheckVectorsRefusesAProviderBreakingThePortContract(t *testing.T) {
	rows := []sqlc.SearchDocumentsNeedingEmbeddingRow{{EntityRef: "a"}, {EntityRef: "b"}}
	good := func(n int, f float32) [][]float32 {
		out := make([][]float32, n)
		for i := range out {
			out[i] = make([]float32, Dimensions)
			out[i][0] = f
		}
		return out
	}

	if err := checkVectors(model("v1"), rows, good(2, 1)); err != nil {
		t.Fatalf("checkVectors refused a conforming batch: %v", err)
	}

	if err := checkVectors(model("v1"), rows, good(1, 1)); err == nil {
		t.Error("checkVectors accepted one vector for two documents")
	}
	if err := checkVectors(model("v1"), rows, good(3, 1)); err == nil {
		t.Error("checkVectors accepted three vectors for two documents")
	}

	short := good(2, 1)
	short[1] = short[1][:Dimensions-1]
	err := checkVectors(model("v1"), rows, short)
	if err == nil {
		t.Fatal("checkVectors accepted a vector of the wrong width")
	}
	// The message must name the row AND the provider: an operator reading the
	// dead letter has to know which embedder to fix.
	if !strings.Contains(err.Error(), "b") || !strings.Contains(err.Error(), model("v1").String()) {
		t.Errorf("wrong-width error does not name the row and the model: %v", err)
	}

	for _, bad := range []float32{float32(math.NaN()), float32(math.Inf(1)), float32(math.Inf(-1))} {
		v := good(2, 1)
		v[0][3] = bad
		if err := checkVectors(model("v1"), rows, v); err == nil {
			t.Errorf("checkVectors accepted a vector holding %v", bad)
		}
	}
}
