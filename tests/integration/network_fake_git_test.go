package integration

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/lichman0405/post/internal/gitprovider"
)

// The in-memory Git transport the T0810 loop runs on.
//
// WHY A FAKE AND NOT GITEA: this suite must run in CI, and CI's
// migration-integration job brings up PostgreSQL and nothing else — no
// Gitea, no Redis. Every Gitea-dependent suite in this package SKIPS there
// (requireGitea), which is why none of them can carry an acceptance
// criterion that reads 「完整链路 CI 可跑」. What the loop needs from the
// transport is small and precisely bounded — repositories, refs, commits,
// changed-file diffs, file content, and one pull-request merge — so the
// transport is the one half of the graph this suite does not take from
// production, and it is taken from production everywhere else: the real
// Provisioner, the real BranchRefSyncer, the real PushIngester behind the
// real HMAC receiver, the real ForkImporter, the real merge command behind
// the real mergegit bridge, and PostgreSQL for every row.
//
// What that leaves untested HERE is the Gitea adapter itself (the HTTP
// calls, the protection rule, the provider's own merge whitelist). That is
// not this suite's claim to make: the same task's G3 override runs the
// real-instance halves (gates.json task_overrides gitea-real-services), and
// the adapter has its own provider-level suites (gitea_protection_test.go,
// gitea_branch_ref_test.go, gitea_push_ingestion_test.go).
//
// The fake is deliberately strict where the real provider is strict — a
// repository that does not exist, a ref that does not exist, a commit that
// does not exist, a merge whose pinned refs moved, a second merge of a
// merged pull request — because a permissive fake would let the loop pass
// on a graph the real transport would refuse.

// netGitServiceLogin is the fake adapter's own service identity: the owner
// namespace every repository is provisioned into, exactly as the real
// adapter resolves it from its token.
const netGitServiceLogin = "post-git-svc"

// netGitMergeLogin is the identity the fake reports as the merger of a
// pull request. It is what the platform's RefGuard judges a main update by
// (gitprovider.RefGuard.MergeService) — the loop wires the guard with THIS
// value, so a merge the fake performed reads as the one controlled write
// path, and any other actor would be refused as a direct write to main.
const netGitMergeLogin = "post-merge-svc"

// netGitBootstrapPath is the file the fake seeds main with (the real
// adapter's EnsureInitialMain writes the platform's bootstrap commit too).
// It is deliberately NOT a scientific manifest: it belongs to no schema,
// and the loop's assertions below show it never reaches an ingestion —
// which is the invariant the fork import's baseline exists to keep.
const netGitBootstrapPath = "README.md"

// netGitCommit is one commit: a full tree snapshot, its parents, and the
// message. Snapshots (not deltas) are what makes ChangedFiles a diff
// between two trees with no history walk, which is all the ingestion asks
// of the transport.
type netGitCommit struct {
	sha     string
	parents []string
	files   map[string]string
	message string
}

// netGitPull is one provider-side pull request (the transport artifact the
// merge creates; the platform's own Research PR is a PostgreSQL row).
type netGitPull struct {
	number   int64
	head     string
	base     string
	merged   bool
	mergeSHA string
	mergedBy string
}

// netGitRepo is one provider-side repository.
type netGitRepo struct {
	id   int64
	name string

	refs map[string]string // branch name → commit sha
	// commits is keyed by sha. Imported commits are shared between the
	// source and the target repository the same way a real fetch makes one
	// object reachable from two repositories.
	commits map[string]*netGitCommit

	webhookID     int64
	webhookURL    string
	webhookSecret string

	protected      bool
	mergeWhitelist []string

	pulls  []netGitPull
	nextPR int64
}

// netGit is the transport. Every method takes the lock: the merge saga and
// the ingestion can run on different goroutines (the HTTP handlers the
// loop drives), and a fake that raced would invent a failure the real
// provider would not have.
type netGit struct {
	mu     sync.Mutex
	nextID int64
	repos  map[string]*netGitRepo
}

func newNetGit() *netGit {
	return &netGit{repos: map[string]*netGitRepo{}}
}

// netRepoKey is the repository's namespace key.
func netRepoKey(owner, name string) string { return owner + "/" + name }

// repo resolves one repository or answers the provider's not-found.
func (g *netGit) repo(owner, name string) (*netGitRepo, error) {
	r, ok := g.repos[netRepoKey(owner, name)]
	if !ok {
		return nil, fmt.Errorf("%w: repository %s does not exist", gitprovider.ErrNotFound, netRepoKey(owner, name))
	}
	return r, nil
}

// repository builds the port's Repository value for a repo.
func (r *netGitRepo) repository() gitprovider.Repository {
	defaultBranch := ""
	if _, ok := r.refs["main"]; ok {
		defaultBranch = "main"
	}
	return gitprovider.Repository{
		Owner:         netGitServiceLogin,
		Name:          r.name,
		ID:            r.id,
		CloneURL:      "http://host.invalid/" + netGitServiceLogin + "/" + r.name + ".git",
		Private:       true,
		DefaultBranch: defaultBranch,
	}
}

// commitContent addresses a commit's content: the same parents, tree and
// message are the same commit, as on a real transport.
func netCommitSHA(parents []string, files map[string]string, message string) string {
	h := sha1.New()
	for _, p := range parents {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		h.Write([]byte(p))
		h.Write([]byte{0})
		h.Write([]byte(files[p]))
		h.Write([]byte{0})
	}
	h.Write([]byte(message))
	return hex.EncodeToString(h.Sum(nil))
}

// newCommit records one commit on the repository and returns its sha. The
// caller decides where (if anywhere) the commit's ref points.
func (r *netGitRepo) newCommit(parents []string, files map[string]string, message string) string {
	sha := netCommitSHA(parents, files, message)
	if _, ok := r.commits[sha]; !ok {
		tree := make(map[string]string, len(files))
		for k, v := range files {
			tree[k] = v
		}
		r.commits[sha] = &netGitCommit{sha: sha, parents: parents, files: tree, message: message}
	}
	return sha
}

// headOf returns the commit a branch ref points at.
func (r *netGitRepo) headOf(branch string) (*netGitCommit, error) {
	sha, ok := r.refs[branch]
	if !ok || sha == "" {
		return nil, fmt.Errorf("%w: branch %s does not exist", gitprovider.ErrNotFound, branch)
	}
	commit, ok := r.commits[sha]
	if !ok {
		return nil, fmt.Errorf("%w: commit %s is not in %s", gitprovider.ErrNotFound, sha, r.name)
	}
	return commit, nil
}

// changedFiles is the tree diff the ingestion reads: added, modified and
// removed paths, sorted (the port promises the authoritative set, not an
// order).
func changedFiles(base, head map[string]string) []gitprovider.FileChange {
	var out []gitprovider.FileChange
	paths := map[string]bool{}
	for p := range head {
		paths[p] = true
	}
	for p := range base {
		paths[p] = true
	}
	sorted := make([]string, 0, len(paths))
	for p := range paths {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)
	for _, p := range sorted {
		before, hadBefore := base[p]
		after, hasAfter := head[p]
		switch {
		case !hadBefore && hasAfter:
			out = append(out, gitprovider.FileChange{Path: p, Kind: gitprovider.ChangeAdded})
		case hadBefore && !hasAfter:
			out = append(out, gitprovider.FileChange{Path: p, Kind: gitprovider.ChangeRemoved})
		case before != after:
			out = append(out, gitprovider.FileChange{Path: p, Kind: gitprovider.ChangeModified})
		}
	}
	return out
}

// ---- the GitPort surface ------------------------------------------------

// EnsureRepository provisions (or adopts) a repository in the service
// namespace. The caller's name is deterministic per project (RepositoryName),
// which is what makes the same spec the same repository.
func (g *netGit) EnsureRepository(_ context.Context, spec gitprovider.RepositorySpec) (gitprovider.Repository, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if spec.Name == "" {
		return gitprovider.Repository{}, fmt.Errorf("%w: a repository needs a name", gitprovider.ErrConflict)
	}
	key := netRepoKey(netGitServiceLogin, spec.Name)
	repo, ok := g.repos[key]
	if !ok {
		g.nextID++
		repo = &netGitRepo{
			id:      g.nextID,
			name:    spec.Name,
			refs:    map[string]string{},
			commits: map[string]*netGitCommit{},
		}
		g.repos[key] = repo
	}
	return repo.repository(), nil
}

// GetRepository reads one repository.
func (g *netGit) GetRepository(_ context.Context, owner, name string) (gitprovider.Repository, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	repo, err := g.repo(owner, name)
	if err != nil {
		return gitprovider.Repository{}, err
	}
	return repo.repository(), nil
}

// EnsureWebhook makes sure the repository carries one active webhook for
// the URL and secret it was given: an existing one is updated in place
// (the secret rotates, exactly as the real adapter rotates it).
func (g *netGit) EnsureWebhook(_ context.Context, spec gitprovider.WebhookSpec) (gitprovider.Webhook, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	repo, err := g.repo(spec.Repository.Owner, spec.Repository.Name)
	if err != nil {
		return gitprovider.Webhook{}, err
	}
	if repo.webhookID == 0 {
		g.nextID++
		repo.webhookID = g.nextID
	}
	repo.webhookURL = spec.URL
	repo.webhookSecret = spec.Secret
	return gitprovider.Webhook{ID: repo.webhookID, Active: true}, nil
}

// EnsureInitialMain seeds main with the platform's bootstrap commit when
// the repository has no main yet, and answers main's head either way.
func (g *netGit) EnsureInitialMain(_ context.Context, repo gitprovider.Repository) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	r, err := g.repo(repo.Owner, repo.Name)
	if err != nil {
		return "", err
	}
	if sha, ok := r.refs["main"]; ok && sha != "" {
		return sha, nil
	}
	sha := r.newCommit(nil, map[string]string{
		netGitBootstrapPath: "# " + r.name + "\n\nplatform bootstrap commit\n",
	}, "chore: seed the repository")
	r.refs["main"] = sha
	return sha, nil
}

// EnsureMainProtection installs the canonical rule and reports it.
func (g *netGit) EnsureMainProtection(_ context.Context, repo gitprovider.Repository, spec gitprovider.MainProtectionSpec) (gitprovider.MainProtection, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	r, err := g.repo(repo.Owner, repo.Name)
	if err != nil {
		return gitprovider.MainProtection{}, err
	}
	r.protected = true
	// The adapter always includes its own service account in the merge
	// whitelist — the platform's controlled write path cannot be
	// whitelisted away by a caller forgetting to pass it.
	r.mergeWhitelist = append([]string{netGitMergeLogin}, spec.MergeWhitelist...)
	return gitprovider.MainProtection{
		RuleName:              "main",
		DirectPushBlocked:     true,
		ForcePushBlocked:      true,
		MergeWhitelistEnabled: true,
		MergeWhitelist:        r.mergeWhitelist,
	}, nil
}

// GetMainProtection reads the rule back.
func (g *netGit) GetMainProtection(_ context.Context, repo gitprovider.Repository) (gitprovider.MainProtection, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	r, err := g.repo(repo.Owner, repo.Name)
	if err != nil {
		return gitprovider.MainProtection{}, err
	}
	if !r.protected {
		return gitprovider.MainProtection{}, fmt.Errorf("%w: main has no protection rule", gitprovider.ErrNotFound)
	}
	return gitprovider.MainProtection{
		RuleName:              "main",
		DirectPushBlocked:     true,
		ForcePushBlocked:      true,
		MergeWhitelistEnabled: true,
		MergeWhitelist:        r.mergeWhitelist,
	}, nil
}

// EnsureBranch makes sure a branch ref exists, forked from spec.ForkRef
// (a commit sha or a ref name; empty resolves to the default branch). An
// existing ref of the same name is adopted, never moved — the sync is
// idempotent, and a redelivered job must not rewind a line somebody has
// already pushed to.
func (g *netGit) EnsureBranch(_ context.Context, spec gitprovider.BranchSpec) (gitprovider.BranchRef, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	r, err := g.repo(spec.Repository.Owner, spec.Repository.Name)
	if err != nil {
		return gitprovider.BranchRef{}, err
	}
	if sha, ok := r.refs[spec.Name]; ok && sha != "" {
		return gitprovider.BranchRef{Name: spec.Name, HeadSHA: sha}, nil
	}
	forkRef := spec.ForkRef
	if forkRef == "" {
		forkRef = "main"
	}
	sha, ok := r.refs[forkRef]
	if !ok {
		if _, isCommit := r.commits[forkRef]; isCommit {
			sha = forkRef
		} else {
			return gitprovider.BranchRef{}, fmt.Errorf("%w: nothing to fork %s from (%s)",
				gitprovider.ErrNotFound, spec.Name, forkRef)
		}
	}
	r.refs[spec.Name] = sha
	return gitprovider.BranchRef{Name: spec.Name, HeadSHA: sha}, nil
}

// GetBranch reads one branch ref. A ref with no commit answers not-found,
// which is the adapter's shape for "the ref carries nothing".
func (g *netGit) GetBranch(_ context.Context, repo gitprovider.Repository, name string) (gitprovider.BranchRef, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	r, err := g.repo(repo.Owner, repo.Name)
	if err != nil {
		return gitprovider.BranchRef{}, err
	}
	sha, ok := r.refs[name]
	if !ok || sha == "" {
		return gitprovider.BranchRef{}, fmt.Errorf("%w: branch %s does not exist", gitprovider.ErrNotFound, name)
	}
	return gitprovider.BranchRef{Name: name, HeadSHA: sha}, nil
}

// ImportBranch copies one commit from the source repository into a branch
// ref of the target — the fork's content copy. The commit object becomes
// reachable from both repositories, exactly as a fetch would make it.
func (g *netGit) ImportBranch(_ context.Context, spec gitprovider.ImportBranchSpec) (gitprovider.BranchRef, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	source, err := g.repo(spec.Source.Owner, spec.Source.Name)
	if err != nil {
		return gitprovider.BranchRef{}, err
	}
	commit, ok := source.commits[spec.SourceSHA]
	if !ok {
		return gitprovider.BranchRef{}, fmt.Errorf("%w: the source carries no commit %s",
			gitprovider.ErrNotFound, spec.SourceSHA)
	}
	target, err := g.repo(spec.Target.Owner, spec.Target.Name)
	if err != nil {
		return gitprovider.BranchRef{}, err
	}
	target.commits[commit.sha] = commit
	target.refs[spec.Name] = commit.sha
	return gitprovider.BranchRef{Name: spec.Name, HeadSHA: commit.sha}, nil
}

// ListBranches lists every branch ref of a repository.
func (g *netGit) ListBranches(_ context.Context, repo gitprovider.Repository) ([]gitprovider.BranchRef, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	r, err := g.repo(repo.Owner, repo.Name)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(r.refs))
	for name := range r.refs {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]gitprovider.BranchRef, 0, len(names))
	for _, name := range names {
		out = append(out, gitprovider.BranchRef{Name: name, HeadSHA: r.refs[name]})
	}
	return out, nil
}

// DeleteBranch deletes one branch ref. Idempotent, like the adapter.
func (g *netGit) DeleteBranch(_ context.Context, repo gitprovider.Repository, name string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	r, err := g.repo(repo.Owner, repo.Name)
	if err != nil {
		return err
	}
	delete(r.refs, name)
	return nil
}

// ChangedFiles is the diff between two commits — the authoritative set the
// webhook payload cannot give. An empty base diffs against the empty tree.
func (g *netGit) ChangedFiles(_ context.Context, repo gitprovider.Repository, baseSHA, headSHA string) ([]gitprovider.FileChange, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	r, err := g.repo(repo.Owner, repo.Name)
	if err != nil {
		return nil, err
	}
	head, ok := r.commits[headSHA]
	if !ok {
		return nil, fmt.Errorf("%w: %s carries no commit %s", gitprovider.ErrNotFound, r.name, headSHA)
	}
	base := map[string]string{}
	if baseSHA != "" {
		commit, ok := r.commits[baseSHA]
		if !ok {
			return nil, fmt.Errorf("%w: %s carries no commit %s", gitprovider.ErrNotFound, r.name, baseSHA)
		}
		base = commit.files
	}
	return changedFiles(base, head.files), nil
}

// ReadFile reads one file's content at one commit.
func (g *netGit) ReadFile(_ context.Context, repo gitprovider.Repository, sha, path string) ([]byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	r, err := g.repo(repo.Owner, repo.Name)
	if err != nil {
		return nil, err
	}
	commit, ok := r.commits[sha]
	if !ok {
		return nil, fmt.Errorf("%w: %s carries no commit %s", gitprovider.ErrNotFound, r.name, sha)
	}
	content, ok := commit.files[path]
	if !ok {
		return nil, fmt.Errorf("%w: %s has no %s at %s", gitprovider.ErrNotFound, r.name, path, sha)
	}
	return []byte(content), nil
}

// ---- the PullRequestMerger surface --------------------------------------

// MergePullRequest performs the provider-side merge. The pins are the
// contract: a target or source ref that is not where the platform planned
// the merge over is a refusal, never a merge of whatever happens to be
// there. A second merge of an already merged pair is reported as merged
// (the provider's own answer, which is how a retried merge converges)
// rather than merged again.
func (g *netGit) MergePullRequest(_ context.Context, spec gitprovider.PullRequestMergeSpec) (gitprovider.PullRequestMergeResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	repo, err := g.repo(spec.Repository.Owner, spec.Repository.Name)
	if err != nil {
		return gitprovider.PullRequestMergeResult{}, err
	}
	base, err := netBranchName(spec.TargetRef)
	if err != nil {
		return gitprovider.PullRequestMergeResult{}, err
	}
	head, err := netBranchName(spec.SourceRef)
	if err != nil {
		return gitprovider.PullRequestMergeResult{}, err
	}
	target, err := repo.headOf(base)
	if err != nil {
		return gitprovider.PullRequestMergeResult{}, err
	}
	if spec.TargetSHA != "" && target.sha != spec.TargetSHA {
		return gitprovider.PullRequestMergeResult{}, fmt.Errorf(
			"%w: %s is at %s, not the %s the merge was planned against",
			gitprovider.ErrConflict, spec.TargetRef, target.sha, spec.TargetSHA)
	}
	source, err := repo.headOf(head)
	if err != nil {
		return gitprovider.PullRequestMergeResult{}, err
	}
	if spec.SourceSHA != "" && source.sha != spec.SourceSHA {
		return gitprovider.PullRequestMergeResult{}, fmt.Errorf(
			"%w: %s is at %s, not the %s the merge was planned against",
			gitprovider.ErrConflict, spec.SourceRef, source.sha, spec.SourceSHA)
	}
	number := g.pullNumber(repo, base, head)
	mergeSHA := repo.newCommit([]string{target.sha, source.sha}, source.files,
		"Merge "+head+" into "+base)
	repo.refs[base] = mergeSHA
	for i := range repo.pulls {
		if repo.pulls[i].number == number {
			repo.pulls[i].merged = true
			repo.pulls[i].mergeSHA = mergeSHA
			repo.pulls[i].mergedBy = netGitMergeLogin
		}
	}
	return gitprovider.PullRequestMergeResult{
		Number: number,
		SHA:    mergeSHA,
		Actor:  netGitMergeLogin,
	}, nil
}

// pullNumber finds the pull request for a ref pair or opens one — the
// adapter's own find-or-create, narrowed to the partner the platform's
// merge needs.
func (g *netGit) pullNumber(repo *netGitRepo, base, head string) int64 {
	for _, pr := range repo.pulls {
		if pr.base == base && pr.head == head {
			return pr.number
		}
	}
	repo.nextPR++
	repo.pulls = append(repo.pulls, netGitPull{number: repo.nextPR, base: base, head: head})
	return repo.nextPR
}

// netBranchName turns a full branch ref into a branch name, refusing
// anything that is not one.
func netBranchName(ref string) (string, error) {
	name := strings.TrimPrefix(ref, "refs/heads/")
	if name == ref || name == "" {
		return "", fmt.Errorf("%w: %q is not a branch ref", gitprovider.ErrConflict, ref)
	}
	return name, nil
}

// ---- the test's own provider-side probes --------------------------------

// head reads a branch's current sha without a repository handle (the
// fixture's own probe).
func (g *netGit) head(owner, name, branch string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	repo, ok := g.repos[netRepoKey(owner, name)]
	if !ok {
		return ""
	}
	return repo.refs[branch]
}

// fileAt reads one file's content at a ref (the fixture's own probe: what
// the transport actually carries, independent of any platform row).
func (g *netGit) fileAt(owner, name, branch, path string) (string, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	repo, ok := g.repos[netRepoKey(owner, name)]
	if !ok {
		return "", false
	}
	head, err := repo.headOf(branch)
	if err != nil {
		return "", false
	}
	content, ok := head.files[path]
	return content, ok
}

// refNames lists one repository's refs, sorted (an assertion surface).
func (g *netGit) refNames(owner, name string) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	repo, ok := g.repos[netRepoKey(owner, name)]
	if !ok {
		return nil
	}
	out := make([]string, 0, len(repo.refs))
	for ref := range repo.refs {
		out = append(out, ref)
	}
	sort.Strings(out)
	return out
}
