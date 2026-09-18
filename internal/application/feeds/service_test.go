package feeds

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The use case's contract: wiring fails closed, a read failure is never
// answered as "no feed", and nothing is read that this service cannot
// serialize.

// stubReader is the Reader port with a canned state, recording what it was
// asked for.
type stubReader struct {
	state State
	err   error

	calls   int
	gotTgt  Target
	gotLim  int
	gotCtxs []context.Context
}

func (s *stubReader) LoadFeedState(ctx context.Context, target Target, limit int) (State, error) {
	s.calls++
	s.gotTgt, s.gotLim = target, limit
	s.gotCtxs = append(s.gotCtxs, ctx)
	if s.err != nil {
		return State{}, s.err
	}
	return s.state, nil
}

func mustService(t *testing.T, reader Reader, base string) *Service {
	t.Helper()
	svc, err := NewService(reader, Config{BaseURL: base})
	if err != nil {
		t.Fatalf("NewService(%q): %v", base, err)
	}
	return svc
}

// TestNewServiceRefusesAnUnusableBase: every URL in a feed document is
// absolute, so a deployment whose public origin is not usable cannot serve
// feeds at all. It must fail at wiring time — not at request time, and never
// by serving relative links.
func TestNewServiceRefusesAnUnusableBase(t *testing.T) {
	bad := []string{
		"",
		"   ",
		"post.example.org",             // no scheme: a document's links would be relative
		"/assets",                      // no host
		"ftp://post.example.org",       // not http(s)
		"http://",                      // no host
		"https://post.example.org/app", // a path would be silently dropped or silently kept
		"https://post.example.org/?x=1",
		"https://post.example.org/#frag",
		"https://user:pw@post.example.org",
	}
	for _, base := range bad {
		if _, err := NewService(&stubReader{}, Config{BaseURL: base}); err == nil {
			t.Errorf("NewService(%q) succeeded, want a refusal", base)
		} else if !errors.Is(err, ErrConfig) {
			t.Errorf("NewService(%q) error = %v, want ErrConfig", base, err)
		}
	}
	if _, err := NewService(nil, Config{BaseURL: "https://post.example.org"}); !errors.Is(err, ErrConfig) {
		t.Errorf("NewService(nil reader) error = %v, want ErrConfig", err)
	}
}

// TestBaseURLNormalization: the base is stored in the one form the link
// builders append paths to, so a trailing slash does not produce a doubled
// one ("https://host//assets/…").
func TestBaseURLNormalization(t *testing.T) {
	cases := map[string]string{
		"https://post.example.org":     "https://post.example.org",
		"https://post.example.org/":    "https://post.example.org",
		" https://post.example.org/ ":  "https://post.example.org",
		"http://127.0.0.1:3000":        "http://127.0.0.1:3000",
		"https://POST.example.org":     "https://POST.example.org",
		"https://post.example.org:844": "https://post.example.org:844",
	}
	for in, want := range cases {
		svc := mustService(t, &stubReader{}, in)
		if got := svc.BaseURL(); got != want {
			t.Errorf("BaseURL(%q) = %q, want %q", in, got, want)
		}
	}
	// And the normalized base is what the links are actually built from.
	svc := mustService(t, &stubReader{state: State{
		Found: true, Title: "P", ProjectVisibility: VisibilityPublic, CreatedAt: testInstant,
		Entries: []EntryState{assetVersion(testVersionID, "v1.0", VisibilityPublic)},
	}}, "https://post.example.org/")
	feed, err := svc.Feed(context.Background(), assetTarget())
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if want := "https://post.example.org/assets/" + testAssetID + "/v1.0"; feed.Entries[0].Link != want {
		t.Errorf("entry link = %q, want %q (no doubled slash)", feed.Entries[0].Link, want)
	}
}

// TestFeedMapsStatesNotErrors: an unknown target and a non-public project are
// the same ErrNotFound — the caller cannot tell them apart — while a read
// that failed is ErrStore. Confusing the last two is how a subscriber is told
// a project publishes nothing while the database is down.
func TestFeedMapsStatesNotErrors(t *testing.T) {
	ctx := context.Background()

	unknown := &stubReader{state: State{Found: false}}
	if _, err := mustService(t, unknown, "https://x.example.org").Feed(ctx, projectTarget()); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown target error = %v, want ErrNotFound", err)
	}

	private := &stubReader{state: State{Found: true, ProjectVisibility: VisibilityPrivate,
		Entries: []EntryState{assetVersion(testVersionID, "v1.0", VisibilityPublic)}}}
	if _, err := mustService(t, private, "https://x.example.org").Feed(ctx, projectTarget()); !errors.Is(err, ErrNotFound) {
		t.Errorf("private project error = %v, want ErrNotFound", err)
	}

	// A store failure is ErrStore and NOT ErrNotFound, even though the
	// transport answers both with a status: only one of the two is a
	// statement about the feed, and the difference has to survive the layer
	// that can still tell.
	failing := &stubReader{err: errors.New("connection refused")}
	_, err := mustService(t, failing, "https://x.example.org").Feed(ctx, projectTarget())
	if !errors.Is(err, ErrStore) {
		t.Errorf("store failure error = %v, want ErrStore", err)
	}
	if errors.Is(err, ErrNotFound) {
		t.Error("a store failure is reported as ErrNotFound — a feed would tell subscribers it does not exist because the database blinked")
	}
}

// TestFeedNormalizesAndBoundsTheRead: the reader is asked for the CANONICAL
// target (a uuid spelled in another case is one entity, and the entry ids are
// built from the canonical form) and for at most the cap — an anonymous read
// is bounded where the read happens, not only where the rendering is.
func TestFeedNormalizesAndBoundsTheRead(t *testing.T) {
	ctx := context.Background()
	reader := &stubReader{state: State{Found: true, Title: "P", ProjectVisibility: VisibilityPublic,
		CreatedAt: testInstant, Entries: []EntryState{assetVersion(testVersionID, "v1.0", VisibilityPublic)}}}

	svc := mustService(t, reader, "https://x.example.org")
	if _, err := svc.Feed(ctx, Target{Kind: KindProject, ID: strings.ToUpper(testProject)}); err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if reader.gotTgt.ID != testProject {
		t.Errorf("reader asked for %q, want the canonical lowercase uuid %q", reader.gotTgt.ID, testProject)
	}
	if reader.gotLim != DefaultMaxEntries {
		t.Errorf("reader limit = %d, want the default cap %d", reader.gotLim, DefaultMaxEntries)
	}

	reader.gotLim = 0
	svc = mustService(t, reader, "https://x.example.org")
	if _, err := svc.Feed(ctx, assetTarget()); err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if reader.gotLim != DefaultMaxEntries {
		t.Errorf("reader limit = %d, want the default cap %d", reader.gotLim, DefaultMaxEntries)
	}

	custom, err := NewService(reader, Config{BaseURL: "https://x.example.org", MaxEntries: 7})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if _, err := custom.Feed(ctx, assetTarget()); err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if reader.gotLim != 7 {
		t.Errorf("reader limit = %d, want the configured 7", reader.gotLim)
	}

	// A malformed target never reaches the reader at all: the shape is
	// settled before storage is touched (a bad uuid would be SQLSTATE 22P02).
	before := reader.calls
	if _, err := svc.Feed(ctx, Target{Kind: KindProject, ID: "not-a-uuid"}); !errors.Is(err, ErrValidation) {
		t.Errorf("malformed target error = %v, want ErrValidation", err)
	}
	if _, err := svc.Feed(ctx, Target{Kind: "feed", ID: testProject}); !errors.Is(err, ErrValidation) {
		t.Errorf("unknown kind error = %v, want ErrValidation", err)
	}
	if reader.calls != before {
		t.Errorf("the reader was called %d times for malformed targets, want 0", reader.calls-before)
	}
}

// TestDocumentRefusesAnUnknownFormatBeforeReading: the format is the
// caller's own request, and it is answerable without touching storage — which
// is also what keeps it a 400 rather than a 404 on the wire (a 404 would say
// something about the target).
func TestDocumentRefusesAnUnknownFormatBeforeReading(t *testing.T) {
	ctx := context.Background()
	reader := &stubReader{state: publicProjectState(assetVersion(testVersionID, "v1.0", VisibilityPublic))}
	svc := mustService(t, reader, "https://x.example.org")

	for _, format := range []Format{"", "json", "xml", "ATOM"} {
		doc, err := svc.Document(ctx, projectTarget(), format)
		if err == nil {
			t.Errorf("Document(%q) succeeded, want a refusal", format)
			continue
		}
		if !errors.Is(err, ErrValidation) {
			t.Errorf("Document(%q) error = %v, want ErrValidation", format, err)
		}
		if doc != nil {
			t.Errorf("Document(%q) returned bytes with an error: %q", format, doc)
		}
	}
	if reader.calls != 0 {
		t.Errorf("the state was read %d times for unanswerable formats, want 0", reader.calls)
	}

	// The two valid formats render, and what they render is Render's own
	// output for the built feed — no second serialization path.
	for _, format := range []Format{FormatAtom, FormatRSS} {
		doc, err := svc.Document(ctx, projectTarget(), format)
		if err != nil {
			t.Fatalf("Document(%s): %v", format, err)
		}
		feed, err := svc.Feed(ctx, projectTarget())
		if err != nil {
			t.Fatalf("Feed: %v", err)
		}
		want, err := Render(feed, format)
		if err != nil {
			t.Fatalf("Render: %v", err)
		}
		if string(doc) != string(want) {
			t.Errorf("Document(%s) is not Render's output", format)
		}
	}
}

// TestDocumentPropagatesTheNotFoundsAndStoreFailures: Document is the one
// call the transport makes, so the mapping it needs has to survive IT, not
// only Feed.
func TestDocumentPropagatesTheNotFoundsAndStoreFailures(t *testing.T) {
	ctx := context.Background()

	private := &stubReader{state: State{Found: true, ProjectVisibility: VisibilityPrivate}}
	if _, err := mustService(t, private, "https://x.example.org").Document(ctx, projectTarget(), FormatAtom); !errors.Is(err, ErrNotFound) {
		t.Errorf("private project Document error = %v, want ErrNotFound", err)
	}

	failing := &stubReader{err: errors.New("timeout")}
	if _, err := mustService(t, failing, "https://x.example.org").Document(ctx, projectTarget(), FormatRSS); !errors.Is(err, ErrStore) {
		t.Errorf("store failure Document error = %v, want ErrStore", err)
	}
}
