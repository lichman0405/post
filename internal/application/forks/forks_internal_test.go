package forks

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/relationcatalog"
)

// TestForkedFromIsTheCatalogRelation is the anti-invention check the audit
// comment promises: the lineage relation name this package stores is the
// canonical vocabulary's, so a rename in internal/rsg/relationcatalog
// cannot leave the literal here silently behind.
func TestForkedFromIsTheCatalogRelation(t *testing.T) {
	if !relationcatalog.Valid(relationForkedFrom) {
		t.Fatalf("relation %q is not in the relation catalog — the lineage relation name must be the vocabulary's", relationForkedFrom)
	}
}

// TestForkAuditNamesTheParentAndTheRelation pins the audit entry's shape:
// the scope is the project being forked (its maintainers are the ones who
// need to see an external fork), the target ref names the fork project, and
// the metadata carries the canonical relation name.
func TestForkAuditNamesTheParentAndTheRelation(t *testing.T) {
	actor := domain.User{ID: "u-1", Handle: "curie"}
	parent := domain.Project{ID: "p-1", Slug: "mof-gas"}
	forkProject := domain.Project{ID: "p-2", Slug: "mof-gas-curie", Visibility: domain.VisibilityPrivate}
	source := domain.Branch{ID: "b-1", Name: "main"}
	forkBranch := domain.Branch{ID: "b-2", Name: "fork/main"}
	entry := forkAudit(actor, parent, forkProject, source, forkBranch)
	if entry.Action != ActionProjectForked {
		t.Fatalf("action = %q, want %q", entry.Action, ActionProjectForked)
	}
	if entry.ProjectID != parent.ID {
		t.Fatalf("scope = %q, want the parent %q", entry.ProjectID, parent.ID)
	}
	if entry.TargetRef != "project:p-2" {
		t.Fatalf("target = %q, want project:p-2", entry.TargetRef)
	}
	after, ok := entry.AfterSummary.(map[string]any)
	if !ok || after["fork_project_id"] != "p-2" || after["parent_project_id"] != "p-1" {
		t.Fatalf("after summary = %#v, want both project ids", entry.AfterSummary)
	}
	meta, ok := entry.Metadata.(map[string]any)
	if !ok || meta["relation_type"] != relationForkedFrom {
		t.Fatalf("metadata = %#v, want the relation name", entry.Metadata)
	}
}

// TestForkBranchNameDerivation pins the copy target: a named branch is
// taken as given, the default is fork/<source>, and the fork's own main is
// refused.
func TestForkBranchNameDerivation(t *testing.T) {
	source := domain.Branch{Name: "main"}
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "default", in: "", want: "fork/main"},
		{name: "named", in: "try-1", want: "try-1"},
		{name: "trimmed", in: " try-1 ", want: "try-1"},
		{name: "the fork's own main", in: "main", wantErr: true},
		{name: "main with whitespace", in: " main ", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := forkBranchName(tc.in, source)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("forkBranchName(%q) = %q, want a refusal", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("forkBranchName(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("forkBranchName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestSlugTokenReducesToTheAlphabet: slugs are lowercase letters, digits
// and single dashes with no leading or trailing dash.
func TestSlugTokenReducesToTheAlphabet(t *testing.T) {
	cases := map[string]string{
		"Curie":        "curie",
		"Marie Curie":  "marie-curie",
		"--a--b--":     "a-b",
		"Ünïcode":      "n-code",
		"!":            "",
		"a.b_c":        "a-b-c",
		"  spaced  ok": "spaced-ok",
	}
	for in, want := range cases {
		if got := slugToken(in); got != want {
			t.Fatalf("slugToken(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestTruncateCutsOnRuneBoundaries: derived names must never fail the
// domain's shape checks because a multibyte character was cut in half.
func TestTruncateCutsOnRuneBoundaries(t *testing.T) {
	long := strings.Repeat("研", 100)
	got := truncate(long, 10)
	if len(got) > 10 {
		t.Fatalf("truncate returned %d bytes, want <= 10", len(got))
	}
	for _, r := range got {
		if r != '研' {
			t.Fatalf("truncate produced %q, want whole runes", got)
		}
	}
}

// TestForkSlugIsAValidSlugForEveryHandle the domain allows: the derived slug
// is an identity, so it must satisfy the domain's own rule at every length
// the domain accepts — the bound is not a detail of this package, and a
// value ValidProjectSlug refuses would fail the project insert and make a
// legal fork impossible. Swept rather than sampled: every handle length from
// 1 to 64 against every parent-slug length from 1 to 64, in three handle
// alphabets (the domain allows [a-z0-9-_]), plus the empty handle that falls
// back to the actor's id.
func TestForkSlugIsAValidSlugForEveryHandle(t *testing.T) {
	alphabets := []string{"a", "b9_", "c_", "d"}
	parentIDs := []string{"11111111-1111-4111-8111-111111111111", "", "not-a-uuid"}
	checked := 0
	for _, parentID := range parentIDs {
		for parentLen := 1; parentLen <= 64; parentLen++ {
			parentSlug := strings.Repeat("p", parentLen)
			for _, alphabet := range alphabets {
				for handleLen := 0; handleLen <= 64; handleLen++ {
					handle := strings.Repeat(alphabet, handleLen/len(alphabet)+1)[:handleLen]
					if handleLen > 0 && !domain.ValidHandle(handle) {
						t.Fatalf("fixture handle %q is not one domain.ValidHandle allows", handle)
					}
					actor := domain.User{ID: "77777777-7777-4777-8777-777777777777", Handle: handle}
					for _, slug := range []string{
						forkSlug(parentSlug, actor, parentID),
						forkSlugReserve(parentSlug, actor, parentID),
					} {
						checked++
						if !domain.ValidProjectSlug(slug) {
							t.Fatalf("fork slug %q (handle %d chars, parent slug %d chars, parent id %q) is not a valid project slug",
								slug, handleLen, parentLen, parentID)
						}
					}
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("nothing was checked")
	}
	t.Logf("checked %d derived slugs", checked)
}

// TestForkSlugKeepsTheForksDistinct: what the shortening must not lose. The
// slug is the identity of a fork, so pairs that differ must derive different
// names even when both names had to be cut: two actors whose handles share a
// long prefix, two long-named parents for one actor, and the derived name
// against the pair's reserved name.
func TestForkSlugKeepsTheForksDistinct(t *testing.T) {
	parent := domain.Project{ID: "11111111-1111-4111-8111-111111111111", Slug: strings.Repeat("mof", 30)}
	other := domain.Project{ID: "22222222-2222-4222-8222-222222222222", Slug: strings.Repeat("mof", 30)}
	curie := domain.User{ID: "77777777-7777-4777-8777-777777777777", Handle: strings.Repeat("curie", 12) + "a"}
	curieB := domain.User{ID: "88888888-8888-4888-8888-888888888888", Handle: strings.Repeat("curie", 12) + "b"}
	if curie.Handle == curieB.Handle || len(curie.Handle) != len(curieB.Handle) || curie.Handle[:60] != curieB.Handle[:60] {
		t.Fatalf("fixture handles are not two long handles sharing a prefix: %q %q", curie.Handle, curieB.Handle)
	}
	if !domain.ValidHandle(curie.Handle) || !domain.ValidHandle(curieB.Handle) {
		t.Fatalf("fixture handles are not ones the domain allows: %q %q", curie.Handle, curieB.Handle)
	}
	cases := []struct {
		name string
		a, b string
	}{
		{"two actors with a shared long handle prefix", forkSlug(parent.Slug, curie, parent.ID), forkSlug(parent.Slug, curieB, parent.ID)},
		{"two long-named parents for one actor", forkSlug(parent.Slug, curie, parent.ID), forkSlug(other.Slug, curie, other.ID)},
		{"the derived name and the reserved one", forkSlug(parent.Slug, curie, parent.ID), forkSlugReserve(parent.Slug, curie, parent.ID)},
		{"no handle at all, two actors", forkSlug(parent.Slug, domain.User{ID: curie.ID}, parent.ID), forkSlug(parent.Slug, domain.User{ID: curieB.ID}, parent.ID)},
	}
	for _, tc := range cases {
		if tc.a == tc.b {
			t.Errorf("%s: both derived %q", tc.name, tc.a)
		}
	}
}

// TestForkSlugReserveIsDeterministicForThePair: the property the F1 fix
// rests on. Escalating past a squatted derived name is safe only because the
// reserved name is a function of the pair and nothing else: two concurrent
// requests of one (parent, actor) derive the same name, so the unique index
// still arbitrates and the pair cannot end up with two fork projects. A name
// that varied per call would break exactly that.
func TestForkSlugReserveIsDeterministicForThePair(t *testing.T) {
	parent := domain.Project{ID: "11111111-1111-4111-8111-111111111111", Slug: "mof-gas"}
	curie := domain.User{ID: "77777777-7777-4777-8777-777777777777", Handle: "curie"}
	other := domain.User{ID: "88888888-8888-4888-8888-888888888888", Handle: "curie"}
	want := "mof-gas-curie-" + pairDigestOf(parent.ID, curie.ID)
	for i := 0; i < 100; i++ {
		if got := forkSlugReserve(parent.Slug, curie, parent.ID); got != forkSlugReserve(parent.Slug, curie, parent.ID) {
			t.Fatalf("the reserved name is not a function of the pair: %q then %q", got, forkSlugReserve(parent.Slug, curie, parent.ID))
		}
		if got := forkSlug(parent.Slug, curie, parent.ID); got != want {
			t.Fatalf("the derived name = %q, want %q", got, want)
		}
	}
	if forkSlugReserve(parent.Slug, curie, parent.ID) == forkSlugReserve(parent.Slug, other, parent.ID) {
		t.Fatalf("two actors reserved the same name for one project")
	}
}

// pairDigestOf re-derives the pair's digest from its definition — sha256
// over the parent id, a NUL and the actor id, the first four bytes in hex —
// so the assertions below pin the VALUE rather than calling the function
// they are checking.
func pairDigestOf(parentID, actorID string) string {
	sum := sha256.Sum256([]byte(parentID + "\x00" + actorID))
	return hex.EncodeToString(sum[:4])
}

// TestForkSlugCarriesThePairDigest pins the rule's two halves at once: the
// name is built from what it always was (the parent's slug and the actor's
// handle, readable), and it now always ends with the digest of (parent id,
// actor id). The digest is what lets one actor fork two projects that share
// a slug, which an organization-scoped slug makes possible and a
// slug-based name would turn into a permanent refusal.
func TestForkSlugCarriesThePairDigest(t *testing.T) {
	first := domain.Project{ID: "11111111-1111-4111-8111-111111111111", Slug: "mof-curie"}
	second := domain.Project{ID: "22222222-2222-4222-8222-222222222222", Slug: "mof-curie"}
	curie := domain.User{ID: "77777777-7777-4777-8777-777777777777", Handle: "curie"}
	gotFirst := forkSlug(first.Slug, curie, first.ID)
	gotSecond := forkSlug(second.Slug, curie, second.ID)
	if want := "mof-curie-curie-" + pairDigestOf(first.ID, curie.ID); gotFirst != want {
		t.Errorf("derived name = %q, want %q", gotFirst, want)
	}
	if gotFirst == gotSecond {
		t.Fatalf("two same-named parents derived one name %q — the second fork would be refused by the first fork's own project", gotFirst)
	}
	// The reserved name is the same name under the reserve mark: still one
	// function of the pair, and never equal to the derived one.
	reserved := forkSlugReserve(first.Slug, curie, first.ID)
	if reserved != gotFirst+forkReserveMark {
		t.Errorf("reserved name = %q, want the derived name plus %q", reserved, forkReserveMark)
	}
}
