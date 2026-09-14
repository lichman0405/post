// Package integration — T0603-TEST-01 "policy engine integration"
// (blocking): the versioned organization/project policy engine exercised
// end to end against REAL PostgreSQL — real migrations, real pgx stores,
// the real auth guard (session + CSRF) and the real policyhttp handlers,
// exactly as cmd/api/main.go composes them (orgshttp + projectshttp
// alongside). Only the session store is in-memory (the same trade as the
// T0103/T0109 suites; policy persistence itself is the real PostgreSQL
// table and the real pgx PolicyStore).
//
// The acceptance criteria each map to a phase of the test:
//
//   - "Project 不可放宽 org rule"  → stricter project policies are
//     accepted; a write that relaxes an org rule (a weaker value, or an
//     unknown rule rewritten to a different value) is refused with 422
//     POLICY_RELAXES_ORG naming the offending rule, and nothing is
//     persisted or audited. Omitting an org rule is NOT a relaxation —
//     the effective merge keeps the org value in force, and its own
//     subtest asserts exactly that;
//   - "旧 policy version 可查询"    → after later writes the versions list
//     still returns every old version with its original content, and the
//     database itself refuses UPDATE/DELETE on policy_versions
//     (append-only, migrations 00014/00015).
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/orgshttp"
	"github.com/lichman0405/post/cmd/api/policyhttp"
	"github.com/lichman0405/post/cmd/api/projectshttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/policy"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/memstore"
	"github.com/lichman0405/post/internal/persistence/testdb"
)

const policyTaskID = "T0603"

// policyVersionResp is the wire shape of one policy version.
type policyVersionResp struct {
	ID             string          `json:"id"`
	OrganizationID *string         `json:"organization_id"`
	ProjectID      *string         `json:"project_id"`
	Version        string          `json:"version"`
	Policy         json.RawMessage `json:"policy"`
	CreatedBy      string          `json:"created_by"`
}

type policyVersionsResp struct {
	Versions []policyVersionResp `json:"versions"`
}

type effectivePolicyResp struct {
	Org             *policyVersionResp `json:"org_policy"`
	Project         *policyVersionResp `json:"project_policy"`
	EffectivePolicy json.RawMessage    `json:"effective_policy"`
}

// policyMap decodes a policy document into the plain shape assertions use
// (numbers arrive as float64 via encoding/json).
func policyMap(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("policy decode: %v", err)
	}
	return m
}

// decodePolicyVersion reads one version payload off the wire.
func decodePolicyVersion(t *testing.T, resp *http.Response) policyVersionResp {
	t.Helper()
	var v policyVersionResp
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("policy version payload: %v", err)
	}
	return v
}

// newPolicyServer composes the production tree (like cmd/api/main.go) —
// auth + orgs + projects + policy over one real test database — and
// returns the server plus the pool for SQL-level verification.
func newPolicyServer(t *testing.T, ctx context.Context) (*httptest.Server, *pgxpool.Pool) {
	t.Helper()
	pool, _ := testdb.Setup(t, ctx, adminURL(t), policyTaskID)
	sessions := memstore.NewSessions()
	limiter := memstore.NewLimiter()
	cfg := authn.Config{
		WebOrigin:          "http://web.test",
		SessionTTL:         time.Hour,
		LoginLimitPerEmail: 1000,
		LoginLimitPerIP:    10000,
		LoginWindow:        time.Minute,
		SignupLimitPerIP:   10000,
	}
	authAPI := authhttp.New(authhttp.Deps{
		Users:      persistence.NewCredentialStore(pool),
		Sessions:   sessions,
		Limiter:    limiter,
		OIDCClient: nil,
		Cfg:        cfg,
		Secure:     false,
	})
	orgStore := persistence.NewOrgStore(pool)
	orgAPI := orgshttp.New(orgshttp.Deps{Store: orgStore})
	projectStore := persistence.NewProjectStore(pool)
	projectAPI := projectshttp.New(projectshttp.Deps{
		Store: projectStore,
		Orgs:  orgStore,
		Authz: authz.NewMatrixEngine(),
	})
	policyAPI := policyhttp.New(policyhttp.Deps{
		Store:    persistence.NewPolicyStore(pool),
		Orgs:     orgStore,
		Projects: projectStore,
	})
	apiMux := http.NewServeMux()
	apiMux.Handle("/api/v1/auth/", authAPI.Routes())
	apiMux.Handle("/api/v1/organizations", orgAPI.Routes())
	apiMux.Handle("/api/v1/organizations/", orgAPI.Routes())
	apiMux.Handle("/api/v1/projects", projectAPI.Routes())
	apiMux.Handle("/api/v1/projects/", projectAPI.Routes())
	policyAPI.Register(apiMux)
	ts := httptest.NewServer(authAPI.Guard(apiMux))
	t.Cleanup(ts.Close)
	return ts, pool
}

// countPolicyAudit counts the policy.version_set rows of one scope.
func countPolicyAudit(t *testing.T, ctx context.Context, pool *pgxpool.Pool, column, id string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*)::int FROM audit_log WHERE `+column+` = $1 AND action = $2`,
		id, domain.ActionPolicyVersionSet).Scan(&n); err != nil {
		t.Fatalf("count policy audit rows: %v", err)
	}
	return n
}

// TestPolicyEngineIntegration is the T0603-TEST-01 "policy engine
// integration" gate. It drives the full acceptance journey over the wire
// against real PostgreSQL.
func TestPolicyEngineIntegration(t *testing.T) {
	ctx := testCtx(t)
	ts, pool := newPolicyServer(t, ctx)

	alice, aliceID := signup(t, ts.URL, "policy-alice@example.com", "policy-alice")
	bob, bobID := signup(t, ts.URL, "policy-bob@example.com", "policy-bob")
	stranger, _ := signup(t, ts.URL, "policy-stranger@example.com", "policy-stranger")

	// --- alice creates the organization and one org project ---
	resp := alice.do(t, http.MethodPost, "/api/v1/organizations",
		`{"slug":"policy-labs","name":"Policy Labs"}`)
	mustStatus(t, resp, http.StatusCreated)
	var createdOrg orgResponse
	if err := json.NewDecoder(resp.Body).Decode(&createdOrg); err != nil {
		t.Fatalf("org create payload: %v", err)
	}
	orgID := createdOrg.Organization.ID

	resp = alice.do(t, http.MethodPost, "/api/v1/projects",
		fmt.Sprintf(`{"slug":"policy-lab","name":"Policy Lab","purpose":"policy engine","visibility":"private","organization_id":%q}`, orgID))
	mustStatus(t, resp, http.StatusCreated)
	var createdProj projectResponse
	if err := json.NewDecoder(resp.Body).Decode(&createdProj); err != nil {
		t.Fatalf("project create payload: %v", err)
	}
	projectID := createdProj.Project.ID

	t.Run("anonymous callers are 401", func(t *testing.T) {
		anon := newTestUserClient(ts.URL)
		resp := anon.do(t, http.MethodGet, "/api/v1/organizations/"+orgID+"/policy", "")
		mustStatus(t, resp, http.StatusUnauthorized)
		resp = anon.do(t, http.MethodPut, "/api/v1/projects/"+projectID+"/policy",
			`{"version":"v1","policy":{"main_protected":true}}`)
		mustStatus(t, resp, http.StatusUnauthorized)
	})

	t.Run("org owner publishes the lower bound, audited in the same row set", func(t *testing.T) {
		resp := alice.do(t, http.MethodPut, "/api/v1/organizations/"+orgID+"/policy",
			`{"version":"v1","policy":{"main_protected":true,"release_min_reviewers":2}}`)
		mustStatus(t, resp, http.StatusCreated)
		v := decodePolicyVersion(t, resp)
		if v.ID == "" || v.Version != "v1" || v.CreatedBy != aliceID {
			t.Fatalf("org policy v1 = %+v", v)
		}
		if v.OrganizationID == nil || *v.OrganizationID != orgID || v.ProjectID != nil {
			t.Errorf("org policy scope = %+v, want organization %s only", v, orgID)
		}
		m := policyMap(t, v.Policy)
		if m["main_protected"] != true || m["release_min_reviewers"] != float64(2) {
			t.Errorf("org policy v1 rules = %v", m)
		}
		// Reading back returns the same version.
		resp = alice.do(t, http.MethodGet, "/api/v1/organizations/"+orgID+"/policy", "")
		mustStatus(t, resp, http.StatusOK)
		if got := decodePolicyVersion(t, resp); got.ID != v.ID || got.Version != "v1" {
			t.Errorf("org policy read back = %+v, want the v1 row", got)
		}
		// The write landed its audit row in the same transaction.
		var via, actor, target, correlation string
		if err := pool.QueryRow(ctx,
			`SELECT via, actor_id::text, target_ref, correlation_id FROM audit_log
			  WHERE organization_id = $1 AND action = $2`, orgID, domain.ActionPolicyVersionSet).
			Scan(&via, &actor, &target, &correlation); err != nil {
			t.Fatalf("org policy audit row: %v", err)
		}
		if via != domain.ViaSession || actor != aliceID || target != "organization:"+orgID || correlation == "" {
			t.Errorf("org policy audit row = via %s actor %s target %s corr %q", via, actor, target, correlation)
		}
	})

	t.Run("outsiders get existence hiding, members may read", func(t *testing.T) {
		// Stranger: the org does not exist for them (read hiding).
		resp := stranger.do(t, http.MethodGet, "/api/v1/organizations/"+orgID+"/policy", "")
		mustStatus(t, resp, http.StatusNotFound)
		mustEnvelope(t, resp, policy.CodePolicyOrgNotFound)
		// Bob is not a member yet: same hiding.
		resp = bob.do(t, http.MethodGet, "/api/v1/organizations/"+orgID+"/policy", "")
		mustStatus(t, resp, http.StatusNotFound)
		mustEnvelope(t, resp, policy.CodePolicyOrgNotFound)
		// Invite bob as contributor: reads open, governance stays closed.
		resp = alice.do(t, http.MethodPost, "/api/v1/organizations/"+orgID+"/members",
			`{"handle":"policy-bob","role":"contributor"}`)
		mustStatus(t, resp, http.StatusCreated)
		resp = bob.do(t, http.MethodGet, "/api/v1/organizations/"+orgID+"/policy", "")
		mustStatus(t, resp, http.StatusOK)
		resp = bob.do(t, http.MethodPut, "/api/v1/organizations/"+orgID+"/policy",
			`{"version":"v2","policy":{"main_protected":false}}`)
		mustStatus(t, resp, http.StatusForbidden)
		mustEnvelope(t, resp, policy.CodePolicyForbidden)
		// The refused write changed nothing.
		resp = alice.do(t, http.MethodGet, "/api/v1/organizations/"+orgID+"/policy", "")
		mustStatus(t, resp, http.StatusOK)
		if got := decodePolicyVersion(t, resp); got.Version != "v1" {
			t.Errorf("org policy after refused write = %q, want still v1", got.Version)
		}
	})

	t.Run("project policy stricter than the org bound is accepted", func(t *testing.T) {
		resp := alice.do(t, http.MethodPut, "/api/v1/projects/"+projectID+"/policy",
			`{"version":"v1","policy":{"main_protected":true,"release_min_reviewers":3}}`)
		mustStatus(t, resp, http.StatusCreated)
		v := decodePolicyVersion(t, resp)
		if v.ProjectID == nil || *v.ProjectID != projectID || v.OrganizationID != nil {
			t.Errorf("project policy scope = %+v, want project %s only", v, projectID)
		}
		if v.Version != "v1" || v.CreatedBy != aliceID {
			t.Errorf("project policy v1 = %+v", v)
		}
	})

	t.Run("relaxing an org rule is refused and never persisted (acceptance)", func(t *testing.T) {
		// Lower release_min_reviewers: weaker.
		resp := alice.do(t, http.MethodPut, "/api/v1/projects/"+projectID+"/policy",
			`{"version":"v2","policy":{"main_protected":true,"release_min_reviewers":1}}`)
		mustStatus(t, resp, http.StatusUnprocessableEntity)
		mustEnvelope(t, resp, policy.CodePolicyRelaxesOrg)
		// main_protected false: weaker.
		resp = alice.do(t, http.MethodPut, "/api/v1/projects/"+projectID+"/policy",
			`{"version":"v2","policy":{"main_protected":false,"release_min_reviewers":3}}`)
		mustStatus(t, resp, http.StatusUnprocessableEntity)
		var env errorEnvelope
		if err := json.Unmarshal([]byte(readAll(t, resp)), &env); err != nil {
			t.Fatalf("envelope decode: %v", err)
		}
		if env.Code != policy.CodePolicyRelaxesOrg || !strings.Contains(env.Message, "main_protected") {
			t.Errorf("relax envelope = %+v, want POLICY_RELAXES_ORG naming main_protected", env)
		}
		// Nothing persisted, nothing audited: still exactly v1.
		resp = alice.do(t, http.MethodGet, "/api/v1/projects/"+projectID+"/policy", "")
		mustStatus(t, resp, http.StatusOK)
		if got := decodePolicyVersion(t, resp); got.Version != "v1" {
			t.Errorf("project policy after refusals = %q, want still v1", got.Version)
		}
		if n := countPolicyAudit(t, ctx, pool, "project_id", projectID); n != 1 {
			t.Errorf("project policy audit rows = %d, want 1 (refused writes are never audited)", n)
		}
	})

	t.Run("every old version stays queryable (acceptance)", func(t *testing.T) {
		// A second, stricter project version...
		resp := alice.do(t, http.MethodPut, "/api/v1/projects/"+projectID+"/policy",
			`{"version":"v2","policy":{"main_protected":true,"release_min_reviewers":4,"raw_data_retention_days":180}}`)
		mustStatus(t, resp, http.StatusCreated)
		// ...and a second org version.
		resp = alice.do(t, http.MethodPut, "/api/v1/organizations/"+orgID+"/policy",
			`{"version":"v2","policy":{"main_protected":true,"release_min_reviewers":2,"public_asset_ip_review":true}}`)
		mustStatus(t, resp, http.StatusCreated)

		// The project history carries BOTH versions, newest first, each
		// with its original content.
		resp = alice.do(t, http.MethodGet, "/api/v1/projects/"+projectID+"/policy/versions", "")
		mustStatus(t, resp, http.StatusOK)
		var versions policyVersionsResp
		if err := json.NewDecoder(resp.Body).Decode(&versions); err != nil {
			t.Fatalf("versions payload: %v", err)
		}
		if len(versions.Versions) != 2 {
			t.Fatalf("project versions = %d, want 2", len(versions.Versions))
		}
		if versions.Versions[0].Version != "v2" || versions.Versions[1].Version != "v1" {
			t.Errorf("version order = %q, %q, want v2 then v1",
				versions.Versions[0].Version, versions.Versions[1].Version)
		}
		old := policyMap(t, versions.Versions[1].Policy)
		if old["release_min_reviewers"] != float64(3) {
			t.Errorf("old v1 content = %v, want its original release_min_reviewers 3", old)
		}

		// The org history too.
		resp = alice.do(t, http.MethodGet, "/api/v1/organizations/"+orgID+"/policy/versions", "")
		mustStatus(t, resp, http.StatusOK)
		if err := json.NewDecoder(resp.Body).Decode(&versions); err != nil {
			t.Fatalf("org versions payload: %v", err)
		}
		if len(versions.Versions) != 2 || versions.Versions[0].Version != "v2" || versions.Versions[1].Version != "v1" {
			t.Errorf("org versions = %+v, want [v2 v1]", versions.Versions)
		}
	})

	t.Run("effective policy merges the org lower bound with the project overlay", func(t *testing.T) {
		resp := alice.do(t, http.MethodGet, "/api/v1/projects/"+projectID+"/policy/effective", "")
		mustStatus(t, resp, http.StatusOK)
		var eff effectivePolicyResp
		if err := json.NewDecoder(resp.Body).Decode(&eff); err != nil {
			t.Fatalf("effective payload: %v", err)
		}
		if eff.Org == nil || eff.Org.Version != "v2" || eff.Project == nil || eff.Project.Version != "v2" {
			t.Fatalf("effective parts = org %+v project %+v, want both v2", eff.Org, eff.Project)
		}
		m := policyMap(t, eff.EffectivePolicy)
		if m["main_protected"] != true ||
			m["release_min_reviewers"] != float64(4) || // project stricter wins
			m["raw_data_retention_days"] != float64(180) || // project-only
			m["public_asset_ip_review"] != true { // org-only
			t.Errorf("effective rules = %v", m)
		}
	})

	t.Run("org tightening flows into the effective policy immediately", func(t *testing.T) {
		// The org raises the lower bound above the project's own rule.
		resp := alice.do(t, http.MethodPut, "/api/v1/organizations/"+orgID+"/policy",
			`{"version":"v3","policy":{"main_protected":true,"release_min_reviewers":5,"public_asset_ip_review":true}}`)
		mustStatus(t, resp, http.StatusCreated)
		resp = alice.do(t, http.MethodGet, "/api/v1/projects/"+projectID+"/policy/effective", "")
		mustStatus(t, resp, http.StatusOK)
		var eff effectivePolicyResp
		if err := json.NewDecoder(resp.Body).Decode(&eff); err != nil {
			t.Fatalf("effective payload: %v", err)
		}
		if m := policyMap(t, eff.EffectivePolicy); m["release_min_reviewers"] != float64(5) {
			t.Errorf("effective release_min_reviewers = %v, want the org bound 5", m["release_min_reviewers"])
		}
		// The project's own document is untouched — the bound applies at
		// evaluation, the stored version is a snapshot.
		resp = alice.do(t, http.MethodGet, "/api/v1/projects/"+projectID+"/policy", "")
		mustStatus(t, resp, http.StatusOK)
		if got := decodePolicyVersion(t, resp); got.Version != "v2" {
			t.Errorf("project policy after org tightening = %q, want the unchanged v2 snapshot", got.Version)
		}
	})

	t.Run("unknown rules compare by identity, fail closed", func(t *testing.T) {
		// The org publishes a rule the V1 engine does not know (future
		// vocabulary). The write gate compares it by identity: the
		// identical value is provably non-relaxing...
		resp := alice.do(t, http.MethodPut, "/api/v1/organizations/"+orgID+"/policy",
			`{"version":"v4","policy":{"main_protected":true,"release_min_reviewers":5,"public_asset_ip_review":true,"future_rule":{"x":2}}}`)
		mustStatus(t, resp, http.StatusCreated)
		resp = alice.do(t, http.MethodPut, "/api/v1/projects/"+projectID+"/policy",
			`{"version":"v3","policy":{"main_protected":true,"release_min_reviewers":5,"raw_data_retention_days":180,"custom_gate":2,"future_rule":{"x":2}}}`)
		mustStatus(t, resp, http.StatusCreated)
		// ...but rewriting it is not provably non-relaxing, so it is
		// refused (a project must not silently rewrite an unknown rule
		// the org set).
		resp = alice.do(t, http.MethodPut, "/api/v1/projects/"+projectID+"/policy",
			`{"version":"v4","policy":{"main_protected":true,"release_min_reviewers":5,"raw_data_retention_days":180,"custom_gate":2,"future_rule":{"x":1}}}`)
		mustStatus(t, resp, http.StatusUnprocessableEntity)
		mustEnvelope(t, resp, policy.CodePolicyRelaxesOrg)
		// Only v1..v3 exist; v4 never landed.
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*)::int FROM policy_versions WHERE project_id = $1`, projectID).Scan(&n); err != nil {
			t.Fatalf("count project policy versions: %v", err)
		}
		if n != 3 {
			t.Errorf("project policy versions = %d, want 3", n)
		}
	})

	t.Run("omitting an org rule is not a relaxation: the bound still applies", func(t *testing.T) {
		// A second org project whose policy omits the org's rules: the
		// write is valid (the omission cannot relax anything — the merge
		// keeps the org value in force).
		resp := alice.do(t, http.MethodPost, "/api/v1/projects",
			fmt.Sprintf(`{"slug":"policy-lab2","name":"Policy Lab 2","purpose":"omission semantics","visibility":"private","organization_id":%q}`, orgID))
		mustStatus(t, resp, http.StatusCreated)
		var lab2 projectResponse
		if err := json.NewDecoder(resp.Body).Decode(&lab2); err != nil {
			t.Fatalf("lab2 payload: %v", err)
		}
		resp = alice.do(t, http.MethodPut, "/api/v1/projects/"+lab2.Project.ID+"/policy",
			`{"version":"v1","policy":{"main_protected":true}}`)
		mustStatus(t, resp, http.StatusCreated)
		// The effective policy still enforces the org bound the project
		// did not mention.
		resp = alice.do(t, http.MethodGet, "/api/v1/projects/"+lab2.Project.ID+"/policy/effective", "")
		mustStatus(t, resp, http.StatusOK)
		var eff effectivePolicyResp
		if err := json.NewDecoder(resp.Body).Decode(&eff); err != nil {
			t.Fatalf("lab2 effective payload: %v", err)
		}
		if m := policyMap(t, eff.EffectivePolicy); m["release_min_reviewers"] != float64(5) {
			t.Errorf("lab2 effective release_min_reviewers = %v, want the org bound 5", m["release_min_reviewers"])
		}
	})

	t.Run("a version string is never reused", func(t *testing.T) {
		resp := alice.do(t, http.MethodPut, "/api/v1/projects/"+projectID+"/policy",
			`{"version":"v3","policy":{"main_protected":true,"release_min_reviewers":5,"raw_data_retention_days":180,"custom_gate":2,"future_rule":{"x":2}}}`)
		mustStatus(t, resp, http.StatusConflict)
		mustEnvelope(t, resp, policy.CodePolicyVersionTaken)
	})

	t.Run("malformed input is refused with VALIDATION_FAILED", func(t *testing.T) {
		for _, body := range []string{
			`{"version":"v4","policy":{"main_protected":"yes","release_min_reviewers":4}}`, // wrong kind
			`{"version":"v4","policy":{"Main_Protected":true}}`,                            // invalid key
			`{"version":"v4","policy":{"main_protected":null}}`,                            // null rule
			`{"version":"bad version!","policy":{"main_protected":true}}`,                  // version shape
			`{"version":"v4","policy":[1,2]}`,                                              // not an object
			`{"version":"v4"}`,                                                             // missing policy
		} {
			resp := alice.do(t, http.MethodPut, "/api/v1/projects/"+projectID+"/policy", body)
			mustStatus(t, resp, http.StatusBadRequest)
			mustEnvelope(t, resp, policy.CodeValidationFailed)
		}
	})

	t.Run("project writes are owner-only, reads are member-only", func(t *testing.T) {
		// Bob (org contributor, not yet a project member): hidden.
		resp := bob.do(t, http.MethodGet, "/api/v1/projects/"+projectID+"/policy", "")
		mustStatus(t, resp, http.StatusNotFound)
		mustEnvelope(t, resp, policy.CodePolicyProjectNotFound)
		// Seeded as maintainer: he may read...
		seedMember(t, ctx, pool, projectID, bobID, "maintainer")
		resp = bob.do(t, http.MethodGet, "/api/v1/projects/"+projectID+"/policy", "")
		mustStatus(t, resp, http.StatusOK)
		// ...but not govern.
		resp = bob.do(t, http.MethodPut, "/api/v1/projects/"+projectID+"/policy",
			`{"version":"v4","policy":{"main_protected":true,"release_min_reviewers":5,"raw_data_retention_days":180,"custom_gate":2,"future_rule":{"x":2}}}`)
		mustStatus(t, resp, http.StatusForbidden)
		mustEnvelope(t, resp, policy.CodePolicyForbidden)
		// A stranger is hidden on every read.
		resp = stranger.do(t, http.MethodGet, "/api/v1/projects/"+projectID+"/policy/versions", "")
		mustStatus(t, resp, http.StatusNotFound)
		mustEnvelope(t, resp, policy.CodePolicyProjectNotFound)
	})

	t.Run("personal projects have no lower bound", func(t *testing.T) {
		resp := alice.do(t, http.MethodPost, "/api/v1/projects",
			`{"slug":"policy-personal","name":"Policy Personal","purpose":"personal policy lab","visibility":"private"}`)
		mustStatus(t, resp, http.StatusCreated)
		var personal projectResponse
		if err := json.NewDecoder(resp.Body).Decode(&personal); err != nil {
			t.Fatalf("personal project payload: %v", err)
		}
		// false would relax an org rule — but there is no org.
		resp = alice.do(t, http.MethodPut, "/api/v1/projects/"+personal.Project.ID+"/policy",
			`{"version":"v1","policy":{"main_protected":false}}`)
		mustStatus(t, resp, http.StatusCreated)
		resp = alice.do(t, http.MethodGet, "/api/v1/projects/"+personal.Project.ID+"/policy/effective", "")
		mustStatus(t, resp, http.StatusOK)
		var eff effectivePolicyResp
		if err := json.NewDecoder(resp.Body).Decode(&eff); err != nil {
			t.Fatalf("personal effective payload: %v", err)
		}
		if eff.Org != nil || eff.Project == nil {
			t.Fatalf("personal effective parts = org %+v project %+v, want project only", eff.Org, eff.Project)
		}
		if m := policyMap(t, eff.EffectivePolicy); m["main_protected"] != false {
			t.Errorf("personal effective rules = %v, want the project's main_protected false", m)
		}
	})

	t.Run("a deactivated org refuses policy writes", func(t *testing.T) {
		resp := alice.do(t, http.MethodPost, "/api/v1/organizations",
			`{"slug":"retired-labs","name":"Retired Labs"}`)
		mustStatus(t, resp, http.StatusCreated)
		var retired orgResponse
		if err := json.NewDecoder(resp.Body).Decode(&retired); err != nil {
			t.Fatalf("retired org payload: %v", err)
		}
		resp = alice.do(t, http.MethodDelete, "/api/v1/organizations/"+retired.Organization.ID, "")
		mustStatus(t, resp, http.StatusNoContent)
		resp = alice.do(t, http.MethodPut, "/api/v1/organizations/"+retired.Organization.ID+"/policy",
			`{"version":"v1","policy":{"main_protected":true}}`)
		mustStatus(t, resp, http.StatusConflict)
		mustEnvelope(t, resp, policy.CodePolicyOrgDeactivated)
	})

	t.Run("the database itself enforces append-only history", func(t *testing.T) {
		// UPDATE is refused by the trigger (migration 00014)...
		_, err := pool.Exec(ctx,
			`UPDATE policy_versions SET version = 'hacked' WHERE organization_id = $1`, orgID)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "P0001" || !strings.Contains(pgErr.Message, "append-only") {
			t.Fatalf("UPDATE policy_versions error = %v, want P0001 append-only refusal", err)
		}
		// ...DELETE too.
		_, err = pool.Exec(ctx,
			`DELETE FROM policy_versions WHERE organization_id = $1`, orgID)
		if !errors.As(err, &pgErr) || pgErr.Code != "P0001" || !strings.Contains(pgErr.Message, "append-only") {
			t.Fatalf("DELETE policy_versions error = %v, want P0001 append-only refusal", err)
		}
		// Every accepted write is audited; the latest carries its version.
		if n := countPolicyAudit(t, ctx, pool, "organization_id", orgID); n != 4 {
			t.Errorf("org policy audit rows = %d, want 4 (v1, v2, v3, v4)", n)
		}
		if n := countPolicyAudit(t, ctx, pool, "project_id", projectID); n != 3 {
			t.Errorf("project policy audit rows = %d, want 3 (v1, v2, v3)", n)
		}
		var via, target, version string
		if err := pool.QueryRow(ctx,
			`SELECT via, target_ref, after_summary->>'version' FROM audit_log
			  WHERE project_id = $1 AND action = $2
			  ORDER BY occurred_at DESC, id DESC LIMIT 1`, projectID, domain.ActionPolicyVersionSet).
			Scan(&via, &target, &version); err != nil {
			t.Fatalf("newest project policy audit row: %v", err)
		}
		if via != domain.ViaSession || target != "project:"+projectID || version != "v3" {
			t.Errorf("newest project policy audit = via %s target %s version %s", via, target, version)
		}
	})
}
