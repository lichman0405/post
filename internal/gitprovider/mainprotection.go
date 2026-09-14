package gitprovider

import (
	"context"
	"encoding/base64"
	"net/http"
)

// The Gitea layer of main's double protection (T0302, docs/16 §3): the
// adapter ensures every provisioned repository's main branch carries the
// canonical branch-protection rule. Gitea evaluates rules deny-wins, so an
// extra rule an operator added can only restrict further — never reopen
// main — and the adapter converges only its own rule ("main"), leaving
// foreign rules untouched.
//
// The canonical rule, pinned against the live Gitea 1.27.3 semantics
// (probed before implementation, not read from a doc):
//
//   - enable_push=false blocks EVERY direct push to main, including the
//     repository owner and instance admins (Gitea has no admin bypass for
//     pushes), and including the branch's first push — so a protected main
//     can never be created through Git at all;
//   - enable_force_push=false blocks force pushes (covered by the push
//     block too, kept explicit so a rule edit can never weaken it);
//   - enable_merge_whitelist=true with only the platform merge service in
//     the whitelist makes the PR merge API the single controlled write
//     path: Gitea performs the merge server-side, so merges work while
//     pushes are blocked, and no other identity — admin included — can
//     merge (the API refuses them);
//   - the bypass allowlist (Gitea ≥1.27) is off and empty: no identity may
//     bypass the rule.
//
// The platform's own merge service writes main only through PR merges via
// the adapter's service-account token, so "owner 使用 Git 也无法 direct
// push main" and "平台 merge service 可受控写" hold at this layer by
// construction; the platform-side layer (refguard.go, sweeper.go) covers
// what Gitea alone cannot.

// mainBranch is the only branch this task protects.
const mainBranch = "main"

// canonicalRuleName is the provider-side rule name the platform owns.
const canonicalRuleName = "main"

// mainBootstrapFile and mainBootstrapMessage describe main's bootstrap
// commit: the provider's own auto-init convention (a README naming the
// repository) with a platform-identifying commit message, so the initial
// state of main is auditable as a platform write, not a stray manual one.
const (
	mainBootstrapPath    = "README.md"
	mainBootstrapMessage = "POST repository bootstrap"
)

// branchProtectionRule is the provider-side branch-protection shape the
// adapter reads back (the fields the platform owns; the provider returns
// more fields — approvals, status checks, signed commits — which other
// tasks own and the adapter neither reads nor touches).
type branchProtectionRule struct {
	BranchName          string   `json:"branch_name"`
	RuleName            string   `json:"rule_name"`
	EnablePush          bool     `json:"enable_push"`
	EnablePushWhitelist bool     `json:"enable_push_whitelist"`
	PushWhitelist       []string `json:"push_whitelist_usernames"`
	EnableForcePush     bool     `json:"enable_force_push"`
	// ForcePushAllowlist* is Gitea ≥1.27; unknown to the provider the
	// fields are ignored, which is safe: force pushes are blocked by
	// EnablePush=false alone.
	EnableForcePushAllowlist bool     `json:"enable_force_push_allowlist"`
	ForcePushAllowlist       []string `json:"force_push_allowlist_usernames"`
	EnableMergeWhitelist     bool     `json:"enable_merge_whitelist"`
	MergeWhitelist           []string `json:"merge_whitelist_usernames"`
	EnableBypassAllowlist    bool     `json:"enable_bypass_allowlist"`
	BypassAllowlist          []string `json:"bypass_allowlist_usernames"`
}

// protection maps the provider shape onto the port type.
func (r branchProtectionRule) protection() MainProtection {
	return MainProtection{
		RuleName:              r.RuleName,
		DirectPushBlocked:     !r.EnablePush,
		PushWhitelist:         r.PushWhitelist,
		ForcePushBlocked:      !r.EnableForcePush,
		MergeWhitelistEnabled: r.EnableMergeWhitelist,
		MergeWhitelist:        r.MergeWhitelist,
		BypassEnabled:         r.EnableBypassAllowlist,
		BypassWhitelist:       r.BypassAllowlist,
	}
}

// enforcementCanonical reports whether the rule enforces the canonical
// policy on every field the platform owns, given the full merge whitelist
// (merge service plus spec extras).
func (r branchProtectionRule) enforcementCanonical(mergeWhitelist []string) bool {
	return !r.EnablePush &&
		!r.EnablePushWhitelist && len(r.PushWhitelist) == 0 &&
		!r.EnableForcePush && !r.EnableForcePushAllowlist && len(r.ForcePushAllowlist) == 0 &&
		r.EnableMergeWhitelist && stringSetEqual(r.MergeWhitelist, mergeWhitelist) &&
		!r.EnableBypassAllowlist && len(r.BypassAllowlist) == 0
}

// protectionBody builds the canonical rule body for the provider. POST
// additionally carries branch_name/rule_name; PATCH must not rename, so it
// sends the enforcement fields only.
func protectionBody(mergeWhitelist []string) map[string]any {
	return map[string]any{
		"enable_push":                      false,
		"enable_push_whitelist":            false,
		"push_whitelist_usernames":         []string{},
		"push_whitelist_teams":             []string{},
		"push_whitelist_deploy_keys":       false,
		"enable_force_push":                false,
		"enable_force_push_allowlist":      false,
		"force_push_allowlist_usernames":   []string{},
		"force_push_allowlist_teams":       []string{},
		"force_push_allowlist_deploy_keys": false,
		"enable_merge_whitelist":           true,
		"merge_whitelist_usernames":        mergeWhitelist,
		"merge_whitelist_teams":            []string{},
		"enable_bypass_allowlist":          false,
		"bypass_allowlist_usernames":       []string{},
		"bypass_allowlist_teams":           []string{},
	}
}

// EnsureInitialMain implements GitPort. main's bootstrap commit is a
// hard requirement of this provider (pinned against the live Gitea
// 1.27.3, probed before implementation): a pull request cannot target a
// main that does not exist (the API refuses to create it), and once the
// protection rule is applied main's very first push is refused too — so a
// repository that reached 'provisioned' with an empty main would deadlock,
// no PR could ever bring main into existence. The platform therefore seeds
// main with one bootstrap commit through the contents API (the service
// identity is the author) BEFORE the rule is applied; afterwards main is
// frozen and every later write is a PR merge.
//
// Idempotent: when main already exists the seed is skipped and the
// existing head SHA is returned. A conflict on the file write (two racing
// seeds, or an operator who created main in the same moment) falls back to
// reading the ref — the write must never be repeated over existing state.
func (a *GiteaAdapter) EnsureInitialMain(ctx context.Context, repo Repository) (string, error) {
	sha, exists, err := a.mainSHA(ctx, repo)
	if err != nil {
		return "", err
	}
	if exists {
		return sha, nil
	}

	body := map[string]any{
		"content": base64.StdEncoding.EncodeToString([]byte("# " + repo.Name + "\n")),
		"message": mainBootstrapMessage,
		"branch":  mainBranch,
	}
	var created struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
		Content struct {
			LastCommitSHA string `json:"last_commit_sha"`
		} `json:"content"`
	}
	path := "/api/v1/repos/" + urlSegment(repo.Owner) + "/" + urlSegment(repo.Name) + "/contents/" + mainBootstrapPath
	code, raw, err := a.call(ctx, http.MethodPost, path, body, &created)
	if err != nil {
		return "", err
	}
	switch code {
	case http.StatusCreated:
		sha := created.Commit.SHA
		if sha == "" {
			sha = created.Content.LastCommitSHA
		}
		return sha, nil
	case http.StatusConflict, http.StatusUnprocessableEntity:
		// The file already exists on main: someone else won the seed race.
		// The ref is the truth — when main exists now, this is a success.
		if sha, exists, err := a.mainSHA(ctx, repo); err == nil && exists {
			return sha, nil
		}
		return "", a.mapStatus(code, "bootstrap main", raw)
	default:
		return "", a.mapStatus(code, "bootstrap main", raw)
	}
}

// mainSHA reads main's head commit. exists=false means main has no ref
// (the empty repository at provisioning time), not an error.
func (a *GiteaAdapter) mainSHA(ctx context.Context, repo Repository) (sha string, exists bool, err error) {
	var refs []struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	code, raw, err := a.call(ctx, http.MethodGet,
		"/api/v1/repos/"+urlSegment(repo.Owner)+"/"+urlSegment(repo.Name)+"/git/refs/heads/"+mainBranch,
		nil, &refs)
	if err != nil {
		return "", false, err
	}
	if code == http.StatusNotFound {
		return "", false, nil
	}
	if code != http.StatusOK {
		return "", false, a.mapStatus(code, "read main ref", raw)
	}
	if len(refs) == 0 {
		return "", false, nil
	}
	return refs[0].Object.SHA, true, nil
}

// EnsureMainProtection implements GitPort. The rule search reads the full
// rule list rather than the single-rule endpoint: Gitea ≥1.27 allows
// several rules per branch, and the single-rule endpoint returns the
// highest-priority one — which may be an operator's foreign rule. Only the
// rule named "main" is ever converged; when it is absent a new one is
// created (deny-wins keeps foreign rules harmless, and the next sweep
// finds the canonical rule by name instead of stacking another).
func (a *GiteaAdapter) EnsureMainProtection(ctx context.Context, repo Repository, spec MainProtectionSpec) (MainProtection, error) {
	owner, err := a.Owner(ctx)
	if err != nil {
		return MainProtection{}, err
	}
	full := dedupe(append([]string{owner}, spec.MergeWhitelist...))

	rulesPath := "/api/v1/repos/" + urlSegment(repo.Owner) + "/" + urlSegment(repo.Name) + "/branch_protections"
	var rules []branchProtectionRule
	code, raw, err := a.call(ctx, http.MethodGet, rulesPath, nil, &rules)
	if err != nil {
		return MainProtection{}, err
	}
	if code == http.StatusNotFound {
		return MainProtection{}, ErrNotFound
	}
	if code != http.StatusOK {
		return MainProtection{}, a.mapStatus(code, "list branch protections", raw)
	}

	for _, r := range rules {
		if r.BranchName != mainBranch || r.RuleName != canonicalRuleName {
			continue
		}
		if r.enforcementCanonical(full) {
			return r.protection(), nil
		}
		body := protectionBody(full)
		var patched branchProtectionRule
		code, raw, err := a.call(ctx, http.MethodPatch, rulesPath+"/"+urlSegment(r.RuleName), body, &patched)
		if err != nil {
			return MainProtection{}, err
		}
		if code != http.StatusOK {
			return MainProtection{}, a.mapStatus(code, "repair branch protection", raw)
		}
		return patched.protection(), nil
	}

	// No canonical rule: create it. main need not exist yet — the provider
	// accepts the rule for a branch that has no refs, and the rule then
	// blocks the branch's very first push (see the comment above).
	body := protectionBody(full)
	body["branch_name"] = mainBranch
	body["rule_name"] = canonicalRuleName
	var created branchProtectionRule
	code, raw, err = a.call(ctx, http.MethodPost, rulesPath, body, &created)
	if err != nil {
		return MainProtection{}, err
	}
	if code != http.StatusCreated {
		return MainProtection{}, a.mapStatus(code, "create branch protection", raw)
	}
	return created.protection(), nil
}

// GetMainProtection implements GitPort.
func (a *GiteaAdapter) GetMainProtection(ctx context.Context, repo Repository) (MainProtection, error) {
	var rule branchProtectionRule
	code, raw, err := a.call(ctx, http.MethodGet,
		"/api/v1/repos/"+urlSegment(repo.Owner)+"/"+urlSegment(repo.Name)+"/branch_protections/"+mainBranch,
		nil, &rule)
	if err != nil {
		return MainProtection{}, err
	}
	if code == http.StatusNotFound {
		return MainProtection{}, ErrNotFound
	}
	if code != http.StatusOK {
		return MainProtection{}, a.mapStatus(code, "get branch protection", raw)
	}
	return rule.protection(), nil
}

// dedupe removes duplicate logins, keeping order (stable output for tests
// and the provider request body).
func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
