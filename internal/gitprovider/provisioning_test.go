package gitprovider_test

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/lichman0405/post/internal/gitprovider"
)

// fakePort is a scripted GitPort: canned results, every call recorded.
type fakePort struct {
	repo      gitprovider.Repository
	repoErr   error
	hook      gitprovider.Webhook
	hookErr   error
	repoSpecs []gitprovider.RepositorySpec
	hookSpecs []gitprovider.WebhookSpec
}

func (f *fakePort) EnsureRepository(_ context.Context, spec gitprovider.RepositorySpec) (gitprovider.Repository, error) {
	f.repoSpecs = append(f.repoSpecs, spec)
	if f.repoErr != nil {
		return gitprovider.Repository{}, f.repoErr
	}
	return f.repo, nil
}

func (f *fakePort) GetRepository(context.Context, string, string) (gitprovider.Repository, error) {
	return gitprovider.Repository{}, gitprovider.ErrNotFound
}

func (f *fakePort) EnsureWebhook(_ context.Context, spec gitprovider.WebhookSpec) (gitprovider.Webhook, error) {
	f.hookSpecs = append(f.hookSpecs, spec)
	if f.hookErr != nil {
		return gitprovider.Webhook{}, f.hookErr
	}
	return f.hook, nil
}

// fakeStore is a scripted ProvisionStore: it invokes the provisioning
// function the store contract passes in (recording the project), honours
// the skipped flag, and can fail outright (a store-level failure).
type fakeStore struct {
	project  gitprovider.PendingProject
	skipped  bool
	storeErr error
	calls    []string
	record   *gitprovider.ProvisionRecord
	fnErr    error
}

func (f *fakeStore) ProvisioningBacklog(context.Context) ([]gitprovider.PendingProject, error) {
	return nil, nil
}

func (f *fakeStore) Provision(_ context.Context, projectID string, fn func(gitprovider.PendingProject) (*gitprovider.ProvisionRecord, error)) (bool, bool, error) {
	f.calls = append(f.calls, projectID)
	if f.storeErr != nil {
		return false, false, f.storeErr
	}
	if f.skipped {
		return false, true, nil
	}
	p := f.project
	if p.ID == "" {
		p.ID = projectID
	}
	rec, err := fn(p)
	if err != nil {
		f.fnErr = err
		return false, false, err
	}
	f.record = rec
	return true, false, nil
}

const testProjectID = "c9f0f895-8b6d-4b16-9c0e-1f2a3b4c5d6e"

func newTestProvisioner(port gitprovider.GitPort, store gitprovider.ProvisionStore) *gitprovider.Provisioner {
	return gitprovider.NewProvisioner(port, store, "http://host/api/v1/git/hooks/gitea")
}

func TestRepositoryName(t *testing.T) {
	if got := gitprovider.RepositoryName(testProjectID); got != "p-"+testProjectID {
		t.Errorf("RepositoryName = %q, want p-<uuid>", got)
	}
}

// TestNewWebhookSecret: 32 bytes of entropy, hex-encoded — and fresh every
// call, so one compromised secret never signs for another repository.
func TestNewWebhookSecret(t *testing.T) {
	s1, err := gitprovider.NewWebhookSecret()
	if err != nil {
		t.Fatalf("NewWebhookSecret: %v", err)
	}
	s2, err := gitprovider.NewWebhookSecret()
	if err != nil {
		t.Fatalf("NewWebhookSecret: %v", err)
	}
	for name, s := range map[string]string{"s1": s1, "s2": s2} {
		if len(s) != 64 {
			t.Errorf("%s length = %d, want 64 hex chars", name, len(s))
		}
		raw, err := hex.DecodeString(s)
		if err != nil || len(raw) != 32 {
			t.Errorf("%s = %q: not 32 decoded bytes (err %v)", name, s, err)
		}
	}
	if s1 == s2 {
		t.Error("two secrets are identical — entropy source is broken")
	}
}

// TestProvisionHappyPath: the full policy — name p-<uuid>, private, bounded
// description, push-only events, fresh 64-hex secret, and the record that
// gets committed to the canonical store.
func TestProvisionHappyPath(t *testing.T) {
	port := &fakePort{
		repo: gitprovider.Repository{Owner: "post-git-svc", Name: "p-" + testProjectID, ID: 42, CloneURL: "u"},
		hook: gitprovider.Webhook{ID: 9, Active: true},
	}
	store := &fakeStore{project: gitprovider.PendingProject{ID: testProjectID, Slug: "battery-lab", Name: "Battery Lab"}}
	p := newTestProvisioner(port, store)

	if err := p.Provision(t.Context(), testProjectID); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	if len(port.repoSpecs) != 1 {
		t.Fatalf("EnsureRepository calls = %d, want 1", len(port.repoSpecs))
	}
	spec := port.repoSpecs[0]
	if spec.Name != "p-"+testProjectID {
		t.Errorf("repo spec name = %q, want p-<uuid>", spec.Name)
	}
	if !spec.Private {
		t.Error("repo spec private = false, want true (Git-layer privacy is unconditional)")
	}
	if want := "POST project battery-lab (Battery Lab)"; spec.Description != want {
		t.Errorf("repo description = %q, want %q", spec.Description, want)
	}

	if len(port.hookSpecs) != 1 {
		t.Fatalf("EnsureWebhook calls = %d, want 1", len(port.hookSpecs))
	}
	hook := port.hookSpecs[0]
	if hook.Repository.ID != 42 || hook.Repository.Owner != "post-git-svc" {
		t.Errorf("hook repo = %+v, want the provisioned repository", hook.Repository)
	}
	if hook.URL != "http://host/api/v1/git/hooks/gitea" {
		t.Errorf("hook URL = %q", hook.URL)
	}
	if len(hook.Events) != 1 || hook.Events[0] != "push" {
		t.Errorf("hook events = %v, want [push]", hook.Events)
	}
	if len(hook.Secret) != 64 {
		t.Errorf("hook secret length = %d, want 64 hex chars", len(hook.Secret))
	}

	if store.record == nil {
		t.Fatal("no provision record committed")
	}
	rec := *store.record
	if rec.ProjectID != testProjectID || rec.Owner != "post-git-svc" || rec.Name != "p-"+testProjectID ||
		rec.GiteaRepoID != 42 || rec.WebhookID != 9 || rec.WebhookSecret != hook.Secret {
		t.Errorf("record = %+v, want project facts + the same secret the hook got", rec)
	}
}

// TestProvisionSkipsAlreadyProvisioned: the store's skip (a redelivered job
// on a provisioned project) makes the provider work unreachable — no repo,
// no hook, no error.
func TestProvisionSkipsAlreadyProvisioned(t *testing.T) {
	port := &fakePort{}
	store := &fakeStore{skipped: true}
	p := newTestProvisioner(port, store)

	if err := p.Provision(t.Context(), testProjectID); err != nil {
		t.Fatalf("Provision (skipped) = %v, want nil", err)
	}
	if len(port.repoSpecs) != 0 || len(port.hookSpecs) != 0 {
		t.Errorf("skipped provision touched the provider: repos=%d hooks=%d", len(port.repoSpecs), len(port.hookSpecs))
	}
}

// TestProvisionPropagatesProviderFailure: a provider outage surfaces as
// ErrUnavailable and never reaches the webhook step.
func TestProvisionPropagatesProviderFailure(t *testing.T) {
	port := &fakePort{repoErr: gitprovider.ErrUnavailable}
	store := &fakeStore{project: gitprovider.PendingProject{ID: testProjectID}}
	p := newTestProvisioner(port, store)

	err := p.Provision(t.Context(), testProjectID)
	if !errors.Is(err, gitprovider.ErrUnavailable) {
		t.Errorf("Provision error = %v, want ErrUnavailable", err)
	}
	if len(port.hookSpecs) != 0 {
		t.Errorf("failed repo provision still registered a hook: %+v", port.hookSpecs)
	}
}

// TestProvisionRejectsInvalidProjectID: an id that cannot become a
// repository name fails before any provider call (defense in depth — the
// canonical store only produces uuids).
func TestProvisionRejectsInvalidProjectID(t *testing.T) {
	port := &fakePort{}
	store := &fakeStore{project: gitprovider.PendingProject{ID: "not-a-uuid"}}
	p := newTestProvisioner(port, store)

	err := p.Provision(t.Context(), "not-a-uuid")
	if !errors.Is(err, gitprovider.ErrConflict) {
		t.Errorf("Provision error = %v, want ErrConflict", err)
	}
	if !strings.Contains(err.Error(), "cannot become a repository name") {
		t.Errorf("error %q does not explain the rejection", err)
	}
	if len(port.repoSpecs) != 0 {
		t.Errorf("invalid id reached the provider: %+v", port.repoSpecs)
	}
}

// TestProvisionBoundedDescription: an enormous slug/name cannot exceed the
// provider's description length, and truncation stays on rune boundaries.
func TestProvisionBoundedDescription(t *testing.T) {
	port := &fakePort{repo: gitprovider.Repository{Owner: "o", Name: "n", ID: 1}}
	long := strings.Repeat("研", 300)
	store := &fakeStore{project: gitprovider.PendingProject{ID: testProjectID, Slug: long, Name: long}}
	p := newTestProvisioner(port, store)

	if err := p.Provision(t.Context(), testProjectID); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	desc := port.repoSpecs[0].Description
	if got := utf8.RuneCountInString(desc); got > 500 {
		t.Errorf("description rune count = %d, want ≤ 500", got)
	}
	if !utf8.ValidString(desc) {
		t.Error("description is not valid UTF-8 (rune truncation split a rune)")
	}
}

// TestProvisionStoreFailurePropagates: a store-level failure surfaces
// untouched (the job layer decides retries, not the provisioner).
func TestProvisionStoreFailurePropagates(t *testing.T) {
	port := &fakePort{}
	store := &fakeStore{storeErr: errors.New("database is down")}
	p := newTestProvisioner(port, store)

	err := p.Provision(t.Context(), testProjectID)
	if err == nil || !strings.Contains(err.Error(), "database is down") {
		t.Errorf("Provision error = %v, want the store failure", err)
	}
	if len(port.repoSpecs) != 0 {
		t.Errorf("store failure still reached the provider: %+v", port.repoSpecs)
	}
}

// TestProvisionHookFailurePropagates: the repository exists but the webhook
// fails — the error surfaces (the store records the project failed; the
// next retry re-uses the idempotent EnsureRepository/EnsureWebhook).
func TestProvisionHookFailurePropagates(t *testing.T) {
	port := &fakePort{
		repo:    gitprovider.Repository{Owner: "o", Name: "n", ID: 1},
		hookErr: gitprovider.ErrUnavailable,
	}
	store := &fakeStore{project: gitprovider.PendingProject{ID: testProjectID}}
	p := newTestProvisioner(port, store)

	err := p.Provision(t.Context(), testProjectID)
	if !errors.Is(err, gitprovider.ErrUnavailable) {
		t.Errorf("Provision error = %v, want ErrUnavailable", err)
	}
	if len(port.repoSpecs) != 1 || len(port.hookSpecs) != 1 {
		t.Errorf("repo/hook calls = %d/%d, want 1/1", len(port.repoSpecs), len(port.hookSpecs))
	}
}
