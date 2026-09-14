package gitprovider_test

import (
	"context"
	"encoding/hex"
	"errors"
	"slices"
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
	prot      gitprovider.MainProtection
	protErr   error
	seedSHA   string
	seedErr   error
	repoSpecs []gitprovider.RepositorySpec
	hookSpecs []gitprovider.WebhookSpec
	protRepos []gitprovider.Repository
	protSpecs []gitprovider.MainProtectionSpec
	seedRepos []gitprovider.Repository
	calls     []string

	// Branch-ref side (T0303): separate error cells per method — the close
	// direction must be able to fail its read (ErrNotFound) while the
	// delete still succeeds, and vice versa.
	branch       gitprovider.BranchRef
	branchErr    error
	getBranchErr error
	deleteErr    error
	branchs      []gitprovider.BranchSpec
	branchGot    []string
	deleted      []string
	getRepo      gitprovider.Repository
	getRepoSet   bool
	getRepoErr   error

	// Push-ingestion side (T0305): canned diff and file reads.
	changedFiles    []gitprovider.FileChange
	changedFilesErr error
	files           map[string][]byte
	readFileErr     error
}

func (f *fakePort) EnsureRepository(_ context.Context, spec gitprovider.RepositorySpec) (gitprovider.Repository, error) {
	f.repoSpecs = append(f.repoSpecs, spec)
	f.calls = append(f.calls, "repo")
	if f.repoErr != nil {
		return gitprovider.Repository{}, f.repoErr
	}
	return f.repo, nil
}

func (f *fakePort) GetRepository(_ context.Context, _, _ string) (gitprovider.Repository, error) {
	if f.getRepoErr != nil {
		return gitprovider.Repository{}, f.getRepoErr
	}
	if f.getRepoSet {
		return f.getRepo, nil
	}
	return gitprovider.Repository{}, gitprovider.ErrNotFound
}

func (f *fakePort) EnsureInitialMain(_ context.Context, repo gitprovider.Repository) (string, error) {
	f.seedRepos = append(f.seedRepos, repo)
	f.calls = append(f.calls, "seed")
	if f.seedErr != nil {
		return "", f.seedErr
	}
	return f.seedSHA, nil
}

func (f *fakePort) EnsureWebhook(_ context.Context, spec gitprovider.WebhookSpec) (gitprovider.Webhook, error) {
	f.hookSpecs = append(f.hookSpecs, spec)
	f.calls = append(f.calls, "hook")
	if f.hookErr != nil {
		return gitprovider.Webhook{}, f.hookErr
	}
	return f.hook, nil
}

func (f *fakePort) EnsureMainProtection(_ context.Context, repo gitprovider.Repository, spec gitprovider.MainProtectionSpec) (gitprovider.MainProtection, error) {
	f.protRepos = append(f.protRepos, repo)
	f.protSpecs = append(f.protSpecs, spec)
	f.calls = append(f.calls, "protect")
	if f.protErr != nil {
		return gitprovider.MainProtection{}, f.protErr
	}
	return f.prot, nil
}

func (f *fakePort) GetMainProtection(context.Context, gitprovider.Repository) (gitprovider.MainProtection, error) {
	return gitprovider.MainProtection{}, gitprovider.ErrNotFound
}

func (f *fakePort) EnsureBranch(_ context.Context, spec gitprovider.BranchSpec) (gitprovider.BranchRef, error) {
	f.branchs = append(f.branchs, spec)
	if f.branchErr != nil {
		return gitprovider.BranchRef{}, f.branchErr
	}
	return f.branch, nil
}

func (f *fakePort) GetBranch(_ context.Context, _ gitprovider.Repository, name string) (gitprovider.BranchRef, error) {
	f.branchGot = append(f.branchGot, name)
	if f.getBranchErr != nil {
		return gitprovider.BranchRef{}, f.getBranchErr
	}
	return f.branch, nil
}

func (f *fakePort) DeleteBranch(_ context.Context, _ gitprovider.Repository, name string) error {
	f.deleted = append(f.deleted, name)
	return f.deleteErr
}

// Push-ingestion side (T0305): scripted diff and file reads.

// ChangedFiles returns the canned diff; ChangedFilesErr overrides it.
func (f *fakePort) ChangedFiles(_ context.Context, _ gitprovider.Repository, _, _ string) ([]gitprovider.FileChange, error) {
	f.calls = append(f.calls, "changed-files")
	if f.changedFilesErr != nil {
		return nil, f.changedFilesErr
	}
	return f.changedFiles, nil
}

// ReadFile returns canned content per (sha, path) key; ReadFileErr
// overrides it. The script must be set before the call records the key.
func (f *fakePort) ReadFile(_ context.Context, _ gitprovider.Repository, sha, path string) ([]byte, error) {
	f.calls = append(f.calls, "read-file")
	if f.readFileErr != nil {
		return nil, f.readFileErr
	}
	if v, ok := f.files[sha+":"+path]; ok {
		return v, nil
	}
	return nil, gitprovider.ErrNotFound
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

	// The provider order is policy (T0302): the bootstrap seed runs while
	// main is still writable and BEFORE the webhook exists (the bootstrap
	// push must not fire a delivery), and protection lands before the
	// store records success.
	if want := []string{"repo", "seed", "hook", "protect"}; !slices.Equal(port.calls, want) {
		t.Errorf("port call order = %v, want %v", port.calls, want)
	}
	if len(port.seedRepos) != 1 || port.seedRepos[0].Name != "p-"+testProjectID {
		t.Errorf("EnsureInitialMain repos = %+v, want one call with the provisioned repository", port.seedRepos)
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
	if len(port.repoSpecs) != 0 || len(port.hookSpecs) != 0 || len(port.seedRepos) != 0 || len(port.protRepos) != 0 {
		t.Errorf("skipped provision touched the provider: repos=%d seeds=%d hooks=%d protects=%d",
			len(port.repoSpecs), len(port.seedRepos), len(port.hookSpecs), len(port.protRepos))
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

// TestProvisionProtectsMainBeforeSuccess: main protection (T0302) is part
// of provisioning — the repository reaches 'provisioned' only with the
// rule applied, and a protection or seed failure fails the provisioning.
func TestProvisionProtectsMainBeforeSuccess(t *testing.T) {
	t.Run("seeded and protected before the record", func(t *testing.T) {
		port := &fakePort{
			repo:    gitprovider.Repository{Owner: "o", Name: "n", ID: 1},
			hook:    gitprovider.Webhook{ID: 7, Active: true},
			seedSHA: "sha-seed",
		}
		store := &fakeStore{project: gitprovider.PendingProject{ID: testProjectID}}
		p := newTestProvisioner(port, store)

		if err := p.Provision(t.Context(), testProjectID); err != nil {
			t.Fatalf("Provision: %v", err)
		}
		if len(port.seedRepos) != 1 {
			t.Fatalf("EnsureInitialMain calls = %d, want 1", len(port.seedRepos))
		}
		if len(port.protRepos) != 1 {
			t.Fatalf("EnsureMainProtection calls = %d, want 1", len(port.protRepos))
		}
		if port.seedRepos[0].Name != "n" {
			t.Errorf("seeded repository = %+v, want the provisioned one", port.seedRepos[0])
		}
		if port.protRepos[0].Name != "n" {
			t.Errorf("protected repository = %+v, want the provisioned one", port.protRepos[0])
		}
		if store.record == nil {
			t.Fatal("no provision record — the store must commit only after seed + protection")
		}
	})

	t.Run("seed failure fails the provisioning", func(t *testing.T) {
		port := &fakePort{
			repo:    gitprovider.Repository{Owner: "o", Name: "n", ID: 1},
			hook:    gitprovider.Webhook{ID: 7, Active: true},
			seedErr: gitprovider.ErrUnavailable,
		}
		store := &fakeStore{project: gitprovider.PendingProject{ID: testProjectID}}
		p := newTestProvisioner(port, store)

		err := p.Provision(t.Context(), testProjectID)
		if !errors.Is(err, gitprovider.ErrUnavailable) {
			t.Errorf("Provision error = %v, want ErrUnavailable", err)
		}
		if len(port.hookSpecs) != 0 || len(port.protRepos) != 0 {
			t.Errorf("seed failure still registered hook/protection: hooks=%d protects=%d",
				len(port.hookSpecs), len(port.protRepos))
		}
		if store.record != nil {
			t.Error("provision record produced despite the seed failure")
		}
	})

	t.Run("protection failure fails the provisioning", func(t *testing.T) {
		port := &fakePort{
			repo:    gitprovider.Repository{Owner: "o", Name: "n", ID: 1},
			hook:    gitprovider.Webhook{ID: 7, Active: true},
			protErr: gitprovider.ErrUnauthorized,
		}
		store := &fakeStore{project: gitprovider.PendingProject{ID: testProjectID}}
		p := newTestProvisioner(port, store)

		err := p.Provision(t.Context(), testProjectID)
		if !errors.Is(err, gitprovider.ErrUnauthorized) {
			t.Errorf("Provision error = %v, want ErrUnauthorized", err)
		}
		if store.record != nil {
			t.Error("provision record produced despite the protection failure")
		}
	})
}
