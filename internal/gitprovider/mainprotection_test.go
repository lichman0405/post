package gitprovider_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lichman0405/post/internal/config"
	"github.com/lichman0405/post/internal/gitprovider"
)

// canonicalRuleJSON is the provider read-back of a canonical rule for the
// fake's service account "post-git-svc" (newGiteaAdapter's /user answer).
func canonicalRuleJSON() string {
	return `{"branch_name":"main","rule_name":"main","enable_push":false,` +
		`"enable_push_whitelist":false,"push_whitelist_usernames":[],` +
		`"push_whitelist_teams":[],"push_whitelist_deploy_keys":false,` +
		`"enable_force_push":false,"enable_force_push_allowlist":false,` +
		`"force_push_allowlist_usernames":[],"force_push_allowlist_teams":[],` +
		`"force_push_allowlist_deploy_keys":false,"enable_merge_whitelist":true,` +
		`"merge_whitelist_usernames":["post-git-svc"],"merge_whitelist_teams":[],` +
		`"enable_bypass_allowlist":false,"bypass_allowlist_usernames":[],` +
		`"bypass_allowlist_teams":[],"required_approvals":0,"enable_status_check":false}`
}

// driftedRuleJSON is a rule an operator weakened: merge whitelist emptied
// (merges open to anyone with write access) and a bypass identity added.
func driftedRuleJSON() string {
	return `{"branch_name":"main","rule_name":"main","enable_push":false,` +
		`"enable_push_whitelist":false,"push_whitelist_usernames":[],` +
		`"push_whitelist_teams":[],"push_whitelist_deploy_keys":false,` +
		`"enable_force_push":false,"enable_force_push_allowlist":false,` +
		`"force_push_allowlist_usernames":[],"force_push_allowlist_teams":[],` +
		`"force_push_allowlist_deploy_keys":false,"enable_merge_whitelist":true,` +
		`"merge_whitelist_usernames":[],"merge_whitelist_teams":[],` +
		`"enable_bypass_allowlist":true,"bypass_allowlist_usernames":["postadmin"],` +
		`"bypass_allowlist_teams":[],"required_approvals":0,"enable_status_check":false}`
}

func protectionRepo() gitprovider.Repository {
	return gitprovider.Repository{Owner: "post-git-svc", Name: "p-x", ID: 1}
}

// TestGiteaMainProtectionCreatedWhenMissing: no rule for main exists — the
// canonical rule is created with the merge service in the merge whitelist.
func TestGiteaMainProtectionCreatedWhenMissing(t *testing.T) {
	f := &fakeGitea{routes: []giteaRoute{
		{method: http.MethodGet, prefix: "/api/v1/user", status: http.StatusOK, body: `{"login":"post-git-svc"}`},
		{method: http.MethodGet, prefix: "/api/v1/repos/post-git-svc/p-x/branch_protections",
			status: http.StatusOK, body: `[]`},
		{method: http.MethodPost, prefix: "/api/v1/repos/post-git-svc/p-x/branch_protections",
			status: http.StatusCreated, body: canonicalRuleJSON()},
	}}
	a := newGiteaAdapter(t, f)
	ctx := testCtx(t)

	prot, err := a.EnsureMainProtection(ctx, protectionRepo(), gitprovider.MainProtectionSpec{})
	if err != nil {
		t.Fatalf("EnsureMainProtection: %v", err)
	}
	if !prot.DirectPushBlocked || !prot.ForcePushBlocked || !prot.MergeWhitelistEnabled {
		t.Errorf("protection = %+v, want the canonical enforcement", prot)
	}
	if want := []string{"post-git-svc"}; !equalStrings(prot.MergeWhitelist, want) {
		t.Errorf("merge whitelist = %v, want %v", prot.MergeWhitelist, want)
	}

	posts := f.requests(http.MethodPost, "/api/v1/repos/post-git-svc/p-x/branch_protections")
	if len(posts) != 1 {
		t.Fatalf("POST branch_protections calls = %d, want 1", len(posts))
	}
	body := posts[0].body
	if body["branch_name"] != "main" || body["rule_name"] != "main" {
		t.Errorf("create body names = %q/%q, want main/main", body["branch_name"], body["rule_name"])
	}
	if body["enable_push"] != false || body["enable_force_push"] != false {
		t.Errorf("create body must block direct and force pushes, got enable_push=%v enable_force_push=%v",
			body["enable_push"], body["enable_force_push"])
	}
	if body["enable_merge_whitelist"] != true {
		t.Errorf("create body enable_merge_whitelist = %v, want true", body["enable_merge_whitelist"])
	}
	wl, _ := body["merge_whitelist_usernames"].([]any)
	if len(wl) != 1 || wl[0] != "post-git-svc" {
		t.Errorf("create body merge whitelist = %v, want [post-git-svc]", wl)
	}
	if body["enable_bypass_allowlist"] != false {
		t.Errorf("create body enable_bypass_allowlist = %v, want false", body["enable_bypass_allowlist"])
	}
}

// TestGiteaMainProtectionConvergesDrift: an existing rule an operator
// weakened is repaired in place (PATCH), never duplicated (no POST).
func TestGiteaMainProtectionConvergesDrift(t *testing.T) {
	f := &fakeGitea{routes: []giteaRoute{
		{method: http.MethodGet, prefix: "/api/v1/user", status: http.StatusOK, body: `{"login":"post-git-svc"}`},
		{method: http.MethodGet, prefix: "/api/v1/repos/post-git-svc/p-x/branch_protections",
			status: http.StatusOK, body: "[" + driftedRuleJSON() + "]"},
		{method: http.MethodPatch, prefix: "/api/v1/repos/post-git-svc/p-x/branch_protections/main",
			status: http.StatusOK, body: canonicalRuleJSON()},
	}}
	a := newGiteaAdapter(t, f)
	ctx := testCtx(t)

	prot, err := a.EnsureMainProtection(ctx, protectionRepo(), gitprovider.MainProtectionSpec{})
	if err != nil {
		t.Fatalf("EnsureMainProtection: %v", err)
	}
	if prot.BypassEnabled {
		t.Error("protection still reports a bypass identity after converge")
	}

	if got := len(f.requests(http.MethodPatch, "/api/v1/repos/post-git-svc/p-x/branch_protections")); got != 1 {
		t.Errorf("PATCH calls = %d, want 1 (repair in place)", got)
	}
	if got := len(f.requests(http.MethodPost, "/api/v1/repos/post-git-svc/p-x/branch_protections")); got != 0 {
		t.Errorf("POST calls = %d, want 0 (no duplicate rule)", got)
	}
	patches := f.requests(http.MethodPatch, "/api/v1/repos/post-git-svc/p-x/branch_protections")
	body := patches[0].body
	if body["enable_merge_whitelist"] != true {
		t.Errorf("PATCH body enable_merge_whitelist = %v, want true", body["enable_merge_whitelist"])
	}
	if body["enable_bypass_allowlist"] != false {
		t.Errorf("PATCH body enable_bypass_allowlist = %v, want false", body["enable_bypass_allowlist"])
	}
	wl, _ := body["merge_whitelist_usernames"].([]any)
	if len(wl) != 1 || wl[0] != "post-git-svc" {
		t.Errorf("PATCH body merge whitelist = %v, want [post-git-svc]", wl)
	}
}

// TestGiteaMainProtectionIdempotent: a canonical rule costs one list read
// and no writes.
func TestGiteaMainProtectionIdempotent(t *testing.T) {
	f := &fakeGitea{routes: []giteaRoute{
		{method: http.MethodGet, prefix: "/api/v1/user", status: http.StatusOK, body: `{"login":"post-git-svc"}`},
		{method: http.MethodGet, prefix: "/api/v1/repos/post-git-svc/p-x/branch_protections",
			status: http.StatusOK, body: "[" + canonicalRuleJSON() + "]"},
	}}
	a := newGiteaAdapter(t, f)
	ctx := testCtx(t)

	for i := 0; i < 2; i++ {
		if _, err := a.EnsureMainProtection(ctx, protectionRepo(), gitprovider.MainProtectionSpec{}); err != nil {
			t.Fatalf("ensure call %d: %v", i, err)
		}
	}
	if got := len(f.requests(http.MethodPost, "/api/v1/repos/post-git-svc/p-x/branch_protections")); got != 0 {
		t.Errorf("POST calls = %d, want 0", got)
	}
	if got := len(f.requests(http.MethodPatch, "/api/v1/repos/post-git-svc/p-x/branch_protections")); got != 0 {
		t.Errorf("PATCH calls = %d, want 0", got)
	}
}

// TestGiteaMainProtectionLeavesForeignRuleUntouched: a rule the operator
// created on main is not the platform's to rewrite — the canonical rule is
// added alongside it (the provider evaluates deny-wins, so the foreign
// rule cannot reopen main).
func TestGiteaMainProtectionLeavesForeignRuleUntouched(t *testing.T) {
	f := &fakeGitea{routes: []giteaRoute{
		{method: http.MethodGet, prefix: "/api/v1/user", status: http.StatusOK, body: `{"login":"post-git-svc"}`},
		{method: http.MethodGet, prefix: "/api/v1/repos/post-git-svc/p-x/branch_protections",
			status: http.StatusOK,
			body: `[{"branch_name":"main","rule_name":"custom","enable_push":false,` +
				`"enable_merge_whitelist":true,"merge_whitelist_usernames":["postadmin"]}]`},
		{method: http.MethodPost, prefix: "/api/v1/repos/post-git-svc/p-x/branch_protections",
			status: http.StatusCreated, body: canonicalRuleJSON()},
	}}
	a := newGiteaAdapter(t, f)
	ctx := testCtx(t)

	if _, err := a.EnsureMainProtection(ctx, protectionRepo(), gitprovider.MainProtectionSpec{}); err != nil {
		t.Fatalf("EnsureMainProtection: %v", err)
	}
	if got := len(f.requests(http.MethodPatch, "/api/v1/repos/post-git-svc/p-x/branch_protections")); got != 0 {
		t.Errorf("PATCH calls = %d, want 0 (foreign rules are not rewritten)", got)
	}
	if got := len(f.requests(http.MethodPost, "/api/v1/repos/post-git-svc/p-x/branch_protections")); got != 1 {
		t.Errorf("POST calls = %d, want 1 (canonical rule created alongside)", got)
	}
}

// TestGiteaMainProtectionSpecExtras: spec extras join the merge whitelist
// next to the adapter's own service account, deduplicated.
func TestGiteaMainProtectionSpecExtras(t *testing.T) {
	f := &fakeGitea{routes: []giteaRoute{
		{method: http.MethodGet, prefix: "/api/v1/user", status: http.StatusOK, body: `{"login":"post-git-svc"}`},
		{method: http.MethodGet, prefix: "/api/v1/repos/post-git-svc/p-x/branch_protections",
			status: http.StatusOK, body: `[]`},
		{method: http.MethodPost, prefix: "/api/v1/repos/post-git-svc/p-x/branch_protections",
			status: http.StatusCreated, body: canonicalRuleJSON()},
	}}
	a := newGiteaAdapter(t, f)
	ctx := testCtx(t)

	if _, err := a.EnsureMainProtection(ctx, protectionRepo(), gitprovider.MainProtectionSpec{
		MergeWhitelist: []string{"ops", "post-git-svc"}, // the duplicate must collapse
	}); err != nil {
		t.Fatalf("EnsureMainProtection: %v", err)
	}
	posts := f.requests(http.MethodPost, "/api/v1/repos/post-git-svc/p-x/branch_protections")
	wl, _ := posts[0].body["merge_whitelist_usernames"].([]any)
	if len(wl) != 2 {
		t.Errorf("merge whitelist = %v, want [post-git-svc ops]", wl)
	}
}

// TestGiteaGetMainProtection: reads the rule, and maps absence onto
// ErrNotFound.
func TestGiteaGetMainProtection(t *testing.T) {
	f := &fakeGitea{routes: []giteaRoute{
		{method: http.MethodGet, prefix: "/api/v1/repos/post-git-svc/p-x/branch_protections/main",
			status: http.StatusOK, body: canonicalRuleJSON()},
		{method: http.MethodGet, prefix: "/api/v1/repos/post-git-svc/p-y/branch_protections/main",
			status: http.StatusNotFound, body: `{"message":"no such rule"}`},
	}}
	a := newGiteaAdapter(t, f)
	ctx := testCtx(t)

	prot, err := a.GetMainProtection(ctx, protectionRepo())
	if err != nil {
		t.Fatalf("GetMainProtection: %v", err)
	}
	if !prot.DirectPushBlocked || !prot.ForcePushBlocked {
		t.Errorf("protection = %+v, want canonical enforcement", prot)
	}

	_, err = a.GetMainProtection(ctx, gitprovider.Repository{Owner: "post-git-svc", Name: "p-y", ID: 2})
	if !errors.Is(err, gitprovider.ErrNotFound) {
		t.Errorf("missing rule error = %v, want ErrNotFound", err)
	}
}

// TestGiteaMainProtectionRepositoryMissing: the provider's 404 on the rule
// list means the repository does not exist, not "no rule".
func TestGiteaMainProtectionRepositoryMissing(t *testing.T) {
	f := &fakeGitea{routes: []giteaRoute{
		{method: http.MethodGet, prefix: "/api/v1/user", status: http.StatusOK, body: `{"login":"post-git-svc"}`},
		{method: http.MethodGet, prefix: "/api/v1/repos/post-git-svc/p-x/branch_protections",
			status: http.StatusNotFound, body: `{"message":"repository does not exist"}`},
	}}
	a := newGiteaAdapter(t, f)
	ctx := testCtx(t)

	_, err := a.EnsureMainProtection(ctx, protectionRepo(), gitprovider.MainProtectionSpec{})
	if !errors.Is(err, gitprovider.ErrNotFound) {
		t.Errorf("missing repository error = %v, want ErrNotFound", err)
	}
}

// equalStrings compares two string slices in order (both sides here are
// already canonical order).
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---- main bootstrap ------------------------------------------------------

// TestGiteaInitialMainSeeded: an absent main (404, or an empty ref list —
// both shapes the provider produces) is seeded with one bootstrap commit
// through the contents API and the commit SHA is returned.
func TestGiteaInitialMainSeeded(t *testing.T) {
	t.Run("ref missing (404)", func(t *testing.T) {
		f := &fakeGitea{routes: []giteaRoute{
			{method: http.MethodGet, prefix: "/api/v1/repos/post-git-svc/p-x/git/refs/heads/main",
				status: http.StatusNotFound, body: `{"message":"not found"}`},
			{method: http.MethodPost, prefix: "/api/v1/repos/post-git-svc/p-x/contents/README.md",
				status: http.StatusCreated, body: `{"commit":{"sha":"c-bootstrap"}}`},
		}}
		a := newGiteaAdapter(t, f)
		ctx := testCtx(t)

		sha, err := a.EnsureInitialMain(ctx, protectionRepo())
		if err != nil {
			t.Fatalf("EnsureInitialMain: %v", err)
		}
		if sha != "c-bootstrap" {
			t.Errorf("sha = %q, want c-bootstrap", sha)
		}
		posts := f.requests(http.MethodPost, "/api/v1/repos/post-git-svc/p-x/contents/README.md")
		if len(posts) != 1 {
			t.Fatalf("contents POST calls = %d, want 1", len(posts))
		}
		body := posts[0].body
		if body["branch"] != "main" {
			t.Errorf("bootstrap branch = %v, want main", body["branch"])
		}
		if body["message"] != "POST repository bootstrap" {
			t.Errorf("bootstrap message = %v, want the platform-identifying message", body["message"])
		}
		if body["content"] != "IyBwLXgK" { // base64("# p-x\n")
			t.Errorf("bootstrap content = %v, want base64 of \"# p-x\\n\"", body["content"])
		}
	})

	t.Run("ref list empty (200 [])", func(t *testing.T) {
		f := &fakeGitea{routes: []giteaRoute{
			{method: http.MethodGet, prefix: "/api/v1/repos/post-git-svc/p-x/git/refs/heads/main",
				status: http.StatusOK, body: `[]`},
			{method: http.MethodPost, prefix: "/api/v1/repos/post-git-svc/p-x/contents/README.md",
				status: http.StatusCreated, body: `{"content":{"last_commit_sha":"c-fallback"}}`},
		}}
		a := newGiteaAdapter(t, f)
		ctx := testCtx(t)

		sha, err := a.EnsureInitialMain(ctx, protectionRepo())
		if err != nil {
			t.Fatalf("EnsureInitialMain: %v", err)
		}
		if sha != "c-fallback" {
			t.Errorf("sha = %q, want c-fallback (the last_commit_sha fallback)", sha)
		}
	})
}

// TestGiteaInitialMainIdempotent: main already exists — no seed write, the
// existing head SHA is returned.
func TestGiteaInitialMainIdempotent(t *testing.T) {
	f := &fakeGitea{routes: []giteaRoute{
		{method: http.MethodGet, prefix: "/api/v1/repos/post-git-svc/p-x/git/refs/heads/main",
			status: http.StatusOK, body: `[{"ref":"refs/heads/main","object":{"sha":"c-existing"}}]`},
	}}
	a := newGiteaAdapter(t, f)
	ctx := testCtx(t)

	sha, err := a.EnsureInitialMain(ctx, protectionRepo())
	if err != nil {
		t.Fatalf("EnsureInitialMain: %v", err)
	}
	if sha != "c-existing" {
		t.Errorf("sha = %q, want c-existing", sha)
	}
	if got := len(f.requests(http.MethodPost, "/api/v1/repos/post-git-svc/p-x/contents/README.md")); got != 0 {
		t.Errorf("contents POST calls = %d, want 0 (main exists, never reseed)", got)
	}
}

// TestGiteaInitialMainConflictFallsBackToRef: the seed write races another
// writer and the provider reports a conflict — the ref is re-read, and a
// now-existing main is the success (the write must never repeat over
// existing state).
func TestGiteaInitialMainConflictFallsBackToRef(t *testing.T) {
	var refReads int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/post-git-svc/p-x/git/refs/heads/main":
			refReads++
			if refReads == 1 {
				// The seed probe: main absent, the write is attempted.
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"not found"}`))
				return
			}
			// The conflict fallback: the racing writer created main.
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[{"ref":"refs/heads/main","object":{"sha":"c-racer"}}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/post-git-svc/p-x/contents/README.md":
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"message":"file already exists"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"no route"}`))
		}
	}))
	t.Cleanup(srv.Close)
	a := gitprovider.NewGiteaAdapter(gitprovider.Config{BaseURL: srv.URL, Token: config.Secret("t")})
	ctx := testCtx(t)

	sha, err := a.EnsureInitialMain(ctx, protectionRepo())
	if err != nil {
		t.Fatalf("EnsureInitialMain: %v", err)
	}
	if sha != "c-racer" {
		t.Errorf("sha = %q, want c-racer (the racing writer's commit)", sha)
	}
	if refReads != 2 {
		t.Errorf("ref reads = %d, want 2 (initial probe + conflict fallback)", refReads)
	}
}

// TestGiteaInitialMainRepositoryMissing: the seed on a repository that does
// not exist maps onto ErrNotFound (the contents write is the discriminator
// — the ref probe alone cannot tell a missing repo from a missing branch).
func TestGiteaInitialMainRepositoryMissing(t *testing.T) {
	f := &fakeGitea{routes: []giteaRoute{
		{method: http.MethodGet, prefix: "/api/v1/repos/post-git-svc/p-x/git/refs/heads/main",
			status: http.StatusNotFound, body: `{"message":"not found"}`},
		{method: http.MethodPost, prefix: "/api/v1/repos/post-git-svc/p-x/contents/README.md",
			status: http.StatusNotFound, body: `{"message":"repository does not exist"}`},
	}}
	a := newGiteaAdapter(t, f)
	ctx := testCtx(t)

	_, err := a.EnsureInitialMain(ctx, protectionRepo())
	if !errors.Is(err, gitprovider.ErrNotFound) {
		t.Errorf("missing repository error = %v, want ErrNotFound", err)
	}
}
