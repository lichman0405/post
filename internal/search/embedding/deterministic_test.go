package embedding

import (
	"context"
	"errors"
	"math"
	"strconv"
	"strings"
	"testing"
)

func model(version string) Model {
	return Model{Provider: LocalModel.Provider, Name: LocalModel.Name, Version: version}
}

func mustEmbedder(t *testing.T, m Model) Deterministic {
	t.Helper()
	d, err := NewDeterministic(m)
	if err != nil {
		t.Fatalf("NewDeterministic(%v): %v", m, err)
	}
	return d
}

func embedOne(t *testing.T, d Deterministic, text string) []float32 {
	t.Helper()
	vectors, err := d.Embed(context.Background(), []string{text})
	if err != nil {
		t.Fatalf("Embed(%q): %v", text, err)
	}
	if len(vectors) != 1 {
		t.Fatalf("Embed(%q) returned %d vectors, want 1", text, len(vectors))
	}
	return vectors[0]
}

// TestDeterministicVectorsAreReproducible is the property every later test
// depends on: the same text and the same model give the same vector, twice
// in a row, in two embedders built separately (so nothing is cached in the
// instance), and across a fresh Embed call.
func TestDeterministicVectorsAreReproducible(t *testing.T) {
	a := mustEmbedder(t, model("v1"))
	b := mustEmbedder(t, model("v1"))

	texts := []string{
		"",
		"perovskite solar cell stability",
		"Perovskite   SOLAR cell, stability!",
		"a",
		strings.Repeat("term ", 50),
	}
	for _, text := range texts {
		first := embedOne(t, a, text)
		second := embedOne(t, a, text)
		third := embedOne(t, b, text)
		for i := range first {
			if first[i] != second[i] || first[i] != third[i] {
				t.Fatalf("text %q: dimension %d is not reproducible: %v vs %v vs %v",
					text, i, first[i], second[i], third[i])
			}
		}
	}
}

// TestDeterministicVectorShape pins the contract the column demands: exactly
// Dimensions floats, all finite, unit length (or the zero vector, which only
// an empty bag could produce).
func TestDeterministicVectorShape(t *testing.T) {
	d := mustEmbedder(t, model("v1"))
	for _, text := range []string{"", "one term", "many terms here, with punctuation: yes!"} {
		v := embedOne(t, d, text)
		if len(v) != Dimensions {
			t.Fatalf("text %q: vector is %d wide, want %d", text, len(v), Dimensions)
		}
		var sum float64
		for _, f := range v {
			if math.IsNaN(float64(f)) || math.IsInf(float64(f), 0) {
				t.Fatalf("text %q: vector holds a non-finite value %v", text, f)
			}
			sum += float64(f) * float64(f)
		}
		if math.Abs(sum-1) > 1e-6 {
			t.Fatalf("text %q: |v|^2 = %v, want 1 (the vector must be unit length)", text, sum)
		}
	}
}

// TestDeterministicVectorTracksTheModelVersion is the unit-level form of the
// acceptance criterion 换一个 provider 版本重算后，向量跟着变: a version bump
// must move EVERY vector, including the one for a text with no indexable
// terms, and it must move them in the stored form (FormatVector) rather than
// only in the float bits.
func TestDeterministicVectorTracksTheModelVersion(t *testing.T) {
	v1 := mustEmbedder(t, model("v1"))
	v2 := mustEmbedder(t, model("v2"))
	otherProvider := mustEmbedder(t, Model{Provider: "other", Name: "sha256-bag", Version: "v1"})

	texts := []string{"", "perovskite solar cell stability", "a"}
	for _, text := range texts {
		one := FormatVector(embedOne(t, v1, text))
		two := FormatVector(embedOne(t, v2, text))
		other := FormatVector(embedOne(t, otherProvider, text))
		if one == two {
			t.Errorf("text %q: version v1 and v2 produced the identical vector — a version bump must be observable", text)
		}
		if one == other {
			t.Errorf("text %q: two providers produced the identical vector — the provider is part of the identity", text)
		}
	}
}

// TestDeterministicVectorSeparatesUnrelatedTexts checks that the projection
// carries information rather than noise: identical text is identical, and
// texts sharing terms are closer than texts that share none. It is a
// property of this implementation (a bag of terms), not a claim that it
// understands meaning, and it is asserted so that a change which silently
// flattens the embedding into a constant would fail here rather than in a
// retrieval test three tasks later.
func TestDeterministicVectorSeparatesUnrelatedTexts(t *testing.T) {
	d := mustEmbedder(t, model("v1"))
	near := cosine(embedOne(t, d, "perovskite solar cell stability"),
		embedOne(t, d, "perovskite solar cell degradation"))
	far := cosine(embedOne(t, d, "perovskite solar cell stability"),
		embedOne(t, d, "medieval manuscript illumination"))
	if far >= near {
		t.Fatalf("an unrelated text scored %v, a related one %v: the vector carries no term information", far, near)
	}
	same := cosine(embedOne(t, d, "perovskite solar cell stability"),
		embedOne(t, d, "perovskite solar cell stability"))
	if math.Abs(same-1) > 1e-6 {
		t.Fatalf("a text is not maximally similar to itself: %v", same)
	}
}

// TestDeterministicRespectsCancellation: the context a batch runs under
// carries the job's timeout, so a cancelled context must stop the work
// rather than be ignored until the batch finishes.
func TestDeterministicRespectsCancellation(t *testing.T) {
	d := mustEmbedder(t, model("v1"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	vectors, err := d.Embed(ctx, []string{"anything"})
	if err == nil {
		t.Fatalf("Embed on a cancelled context returned %d vector(s) and no error", len(vectors))
	}
	if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("Embed on a cancelled context returned %v, want the context error", err)
	}
}

// TestNewDeterministicRejectsIncompleteIdentities: an embedder that cannot
// say which model it is would write rows whose provenance cannot distinguish
// a model change, so construction fails closed rather than degrading.
func TestNewDeterministicRejectsIncompleteIdentities(t *testing.T) {
	cases := map[string]Model{
		"no provider": {Name: "n", Version: "v1"},
		"no name":     {Provider: "p", Version: "v1"},
		"no version":  {Provider: "p", Name: "n"},
		"empty":       {},
	}
	for name, m := range cases {
		if _, err := NewDeterministic(m); err == nil {
			t.Errorf("%s: NewDeterministic accepted %v", name, m)
		}
	}
}

// TestFormatVectorIsPgvectorsInputSyntax pins the wire form: the column is
// written from this string, so a change here is a change to what PostgreSQL
// parses.
func TestFormatVectorIsPgvectorsInputSyntax(t *testing.T) {
	got := FormatVector([]float32{1, -0.5, 0, 0.125})
	want := "[1,-0.5,0,0.125]"
	if got != want {
		t.Fatalf("FormatVector = %q, want %q", got, want)
	}
	if got := FormatVector(nil); got != "[]" {
		t.Fatalf("FormatVector(nil) = %q, want %q", got, "[]")
	}
	// Every value must survive the text round trip as a float32: that is
	// what the server does with this string (pgvector parses it into
	// float4), so a formatting that loses precision would store a different
	// vector than the embedder computed.
	for _, v := range []float32{0, 1, -1, 0.1, float32(math.Pi), 1e-7, -3.4028235e38} {
		text := FormatVector([]float32{v})
		back, err := strconv.ParseFloat(strings.Trim(text, "[]"), 32)
		if err != nil {
			t.Fatalf("FormatVector(%v) = %q, which does not parse: %v", v, text, err)
		}
		if float32(back) != v {
			t.Fatalf("FormatVector(%v) = %q round-trips to %v", v, text, float32(back))
		}
	}
}

// cosine is the similarity the vectors are compared by, computed here so the
// test does not depend on the implementation for its own yardstick.
func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
