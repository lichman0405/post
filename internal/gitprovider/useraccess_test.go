package gitprovider_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/gitprovider"
)

// The service-level tests for the user-access policy (T0304): what the
// service decides, in what order, and which failure mode each step
// produces. The provider and the store are scripted fakes sharing one
// event log, so cross-boundary ordering — provider delete BEFORE the
// canonical flip, grant BEFORE the record — is asserted, not assumed.

const testUserID = "11111111-2222-3333-4444-555555555555"

// fakeUserPort is a scripted UserAccessPort: canned results, every call
// recorded into the shared order log.
type fakeUserPort struct {
	order     *[]string
	ensureErr error
	grantErr  error
	mintErr   error
	deleteErr error
	revokeErr error
	minted    gitprovider.UserToken
	grants    []string // "owner/name user level" per GrantAccess
	deletes   []string // "name id" per DeleteUserToken
	revokes   []string // "owner/name user" per RevokeAccess
}

func (f *fakeUserPort) EnsureGitUser(_ context.Context, platformUserID string) (gitprovider.GitUser, error) {
	*f.order = append(*f.order, "port.ensure:"+platformUserID)
	if f.ensureErr != nil {
		return gitprovider.GitUser{}, f.ensureErr
	}
	return gitprovider.GitUser{Name: gitprovider.GitUserName(platformUserID)}, nil
}

func (f *fakeUserPort) GrantAccess(_ context.Context, repo gitprovider.Repository, user gitprovider.GitUser, level gitprovider.AccessLevel) error {
	*f.order = append(*f.order, "port.grant:"+repo.Owner+"/"+repo.Name+":"+user.Name+":"+string(level))
	f.grants = append(f.grants, repo.Owner+"/"+repo.Name+" "+user.Name+" "+string(level))
	return f.grantErr
}

func (f *fakeUserPort) RevokeAccess(_ context.Context, repo gitprovider.Repository, user gitprovider.GitUser) error {
	*f.order = append(*f.order, "port.revoke:"+repo.Owner+"/"+repo.Name+":"+user.Name)
	f.revokes = append(f.revokes, repo.Owner+"/"+repo.Name+" "+user.Name)
	return f.revokeErr
}

func (f *fakeUserPort) CreateUserToken(_ context.Context, user gitprovider.GitUser, spec gitprovider.UserTokenSpec) (gitprovider.UserToken, error) {
	*f.order = append(*f.order, "port.mint:"+user.Name+":"+spec.Name+":"+string(spec.Level))
	if f.mintErr != nil {
		return gitprovider.UserToken{}, f.mintErr
	}
	return f.minted, nil
}

func (f *fakeUserPort) DeleteUserToken(_ context.Context, user gitprovider.GitUser, tokenID int64) error {
	*f.order = append(*f.order, "port.delete:"+user.Name+":"+itoa(tokenID))
	f.deletes = append(f.deletes, user.Name+" "+itoa(tokenID))
	return f.deleteErr
}

// fakeAccessStore is a scripted AccessStore sharing the same order log.
type fakeAccessStore struct {
	order        *[]string
	repoRef      gitprovider.RepoRef
	repoRefErr   error
	access       gitprovider.AccessRecord
	accessErr    error
	upserts      []gitprovider.AccessRecord
	upsertErr    error
	insertErr    error
	token        gitprovider.TokenRecord
	tokenErr     error
	list         []gitprovider.TokenRecord
	listErr      error
	marksToken   []string
	marksAccess  []string // "project user" per MarkAccessRevoked
	markTokenErr error
}

func (f *fakeAccessStore) RepoRef(_ context.Context, projectID string) (gitprovider.RepoRef, error) {
	*f.order = append(*f.order, "store.ref:"+projectID)
	if f.repoRefErr != nil {
		return gitprovider.RepoRef{}, f.repoRefErr
	}
	return f.repoRef, nil
}

func (f *fakeAccessStore) UpsertGitIdentity(_ context.Context, userID, giteaUsername string) error {
	*f.order = append(*f.order, "store.identity:"+userID+":"+giteaUsername)
	return nil
}

func (f *fakeAccessStore) GetAccess(_ context.Context, projectID, userID string) (gitprovider.AccessRecord, error) {
	*f.order = append(*f.order, "store.getAccess:"+projectID+":"+userID)
	if f.accessErr != nil {
		return gitprovider.AccessRecord{}, f.accessErr
	}
	return f.access, nil
}

func (f *fakeAccessStore) UpsertAccess(_ context.Context, rec gitprovider.AccessRecord) error {
	*f.order = append(*f.order, "store.upsert:"+rec.ProjectID+":"+rec.UserID+":"+string(rec.Permission))
	f.upserts = append(f.upserts, rec)
	return f.upsertErr
}

func (f *fakeAccessStore) MarkAccessRevoked(_ context.Context, projectID, userID string) error {
	*f.order = append(*f.order, "store.revokeAccess:"+projectID+":"+userID)
	f.marksAccess = append(f.marksAccess, projectID+" "+userID)
	return nil
}

func (f *fakeAccessStore) InsertToken(_ context.Context, rec gitprovider.TokenRecord) (gitprovider.TokenRecord, error) {
	*f.order = append(*f.order, "store.insert:"+rec.TokenName)
	if f.insertErr != nil {
		return gitprovider.TokenRecord{}, f.insertErr
	}
	rec.ID = "00000000-0000-4000-8000-000000000099" // canonical id assigned
	return rec, nil
}

func (f *fakeAccessStore) GetToken(_ context.Context, tokenID string) (gitprovider.TokenRecord, error) {
	*f.order = append(*f.order, "store.get:"+tokenID)
	if f.tokenErr != nil {
		return gitprovider.TokenRecord{}, f.tokenErr
	}
	return f.token, nil
}

func (f *fakeAccessStore) ListTokens(_ context.Context, projectID, userID string) ([]gitprovider.TokenRecord, error) {
	*f.order = append(*f.order, "store.list:"+projectID)
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.list, nil
}

func (f *fakeAccessStore) MarkTokenRevoked(_ context.Context, tokenID string) error {
	*f.order = append(*f.order, "store.revokeToken:"+tokenID)
	f.marksToken = append(f.marksToken, tokenID)
	return f.markTokenErr
}

func newTestUserAccess(port gitprovider.UserAccessPort, store gitprovider.AccessStore) *gitprovider.UserAccess {
	return gitprovider.NewUserAccess(port, store, "http://127.0.0.1:3000")
}

func testRepoRef() gitprovider.RepoRef {
	return gitprovider.RepoRef{
		ProjectID: testProjectID,
		Owner:     "post-git-svc",
		Name:      "p-" + testProjectID,
	}
}

// TestIssueTokenHappyPath: the full policy — the access mapping is
// recorded, the credential comes back once with a ready clone URL, and
// the canonical record carries the provider token id, never the value.
func TestIssueTokenHappyPath(t *testing.T) {
	order := &[]string{}
	port := &fakeUserPort{order: order, minted: gitprovider.UserToken{ID: 42, Name: "post-x", Value: "deadbeef"}}
	store := &fakeAccessStore{order: order, repoRef: testRepoRef()}
	s := newTestUserAccess(port, store)

	issued, err := s.IssueToken(t.Context(), testUserID, testProjectID, gitprovider.AccessWrite)
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	if issued.Value != "deadbeef" {
		t.Errorf("value = %q, want the minted credential", issued.Value)
	}
	rec := issued.Record
	if rec.ID == "" || rec.GiteaTokenID != 42 || rec.Status != "active" ||
		rec.Scope != gitprovider.AccessWrite || rec.GiteaUsername != "u-"+testUserID {
		t.Errorf("record = %+v, want id filled, provider id 42, active, write scope, shadow login", rec)
	}
	if len(port.grants) != 1 || !strings.Contains(port.grants[0], "post-git-svc/p-"+testProjectID) ||
		!strings.Contains(port.grants[0], "u-"+testUserID) || !strings.HasSuffix(port.grants[0], "write") {
		t.Errorf("grants = %v, want the write collaborator grant for the shadow login", port.grants)
	}
	if len(store.upserts) != 1 || store.upserts[0].Permission != gitprovider.AccessWrite {
		t.Errorf("upserts = %+v, want one write access row", store.upserts)
	}

	wantClone := "http://u-" + testUserID + ":deadbeef@127.0.0.1:3000/post-git-svc/p-" + testProjectID + ".git"
	if issued.CloneURL != wantClone {
		t.Errorf("clone URL = %q, want %q", issued.CloneURL, wantClone)
	}

	wantOrder := []string{
		"store.ref:" + testProjectID,
		"port.ensure:" + testUserID,
		"store.identity:" + testUserID + ":u-" + testUserID,
		"port.grant:post-git-svc/p-" + testProjectID + ":u-" + testUserID + ":write",
		"store.upsert:" + testProjectID + ":" + testUserID + ":write",
		"port.mint:u-" + testUserID + ":post-",
		"store.insert:",
	}
	if len(*order) != len(wantOrder) {
		t.Fatalf("call order = %v, want %d steps", *order, len(wantOrder))
	}
	for i := range wantOrder {
		if !strings.HasPrefix((*order)[i], wantOrder[i]) {
			t.Errorf("call[%d] = %q, want prefix %q (full order %v)", i, (*order)[i], wantOrder[i], *order)
		}
	}
}

// TestIssueTokenRepoNotProvisioned: no repository — nothing provider-side
// can happen, and the caller gets the telling sentinel.
func TestIssueTokenRepoNotProvisioned(t *testing.T) {
	order := &[]string{}
	port := &fakeUserPort{order: order}
	store := &fakeAccessStore{order: order, repoRefErr: gitprovider.ErrRepoNotProvisioned}
	s := newTestUserAccess(port, store)

	_, err := s.IssueToken(t.Context(), testUserID, testProjectID, gitprovider.AccessRead)
	if !errors.Is(err, gitprovider.ErrRepoNotProvisioned) {
		t.Fatalf("IssueToken = %v, want ErrRepoNotProvisioned", err)
	}
	if len(*order) != 1 {
		t.Errorf("unprovisioned issue reached further work: %v", *order)
	}
}

// TestIssueTokenMintFailsAfterGrant: the grant happened, the mint did not
// — the error surfaces and no canonical token row is written.
func TestIssueTokenMintFailsAfterGrant(t *testing.T) {
	order := &[]string{}
	port := &fakeUserPort{order: order, mintErr: gitprovider.ErrUnavailable}
	store := &fakeAccessStore{order: order, repoRef: testRepoRef()}
	s := newTestUserAccess(port, store)

	_, err := s.IssueToken(t.Context(), testUserID, testProjectID, gitprovider.AccessRead)
	if !errors.Is(err, gitprovider.ErrUnavailable) {
		t.Fatalf("IssueToken = %v, want ErrUnavailable", err)
	}
	if len(port.grants) != 1 || len(store.upserts) != 1 {
		t.Errorf("grant/upsert = %d/%d, want 1/1 (the mapping exists even though no token came of it)", len(port.grants), len(store.upserts))
	}
}

// TestIssueTokenInsertFailsDestroysToken: the credential exists
// provider-side but cannot be recorded — the service destroys it
// best-effort so no unrecorded credential survives.
func TestIssueTokenInsertFailsDestroysToken(t *testing.T) {
	order := &[]string{}
	port := &fakeUserPort{order: order, minted: gitprovider.UserToken{ID: 42, Value: "deadbeef"}}
	store := &fakeAccessStore{order: order, repoRef: testRepoRef(), insertErr: errors.New("database is down")}
	s := newTestUserAccess(port, store)

	_, err := s.IssueToken(t.Context(), testUserID, testProjectID, gitprovider.AccessRead)
	if err == nil || !strings.Contains(err.Error(), "database is down") {
		t.Fatalf("IssueToken = %v, want the store failure", err)
	}
	if len(port.deletes) != 1 || port.deletes[0] != "u-"+testUserID+" 42" {
		t.Errorf("deletes = %v, want one best-effort delete of provider token 42", port.deletes)
	}
}

// TestIssueTokenRejectsMalformedIDs: a non-uuid reaches nothing — neither
// the store nor the provider.
func TestIssueTokenRejectsMalformedIDs(t *testing.T) {
	order := &[]string{}
	port := &fakeUserPort{order: order}
	store := &fakeAccessStore{order: order, repoRef: testRepoRef()}
	s := newTestUserAccess(port, store)

	_, err := s.IssueToken(t.Context(), "not-a-uuid", testProjectID, gitprovider.AccessRead)
	if !errors.Is(err, gitprovider.ErrConflict) {
		t.Fatalf("IssueToken = %v, want ErrConflict", err)
	}
	if len(*order) != 0 {
		t.Errorf("malformed ids reached the store/provider: %v", *order)
	}
}

// testTokenID is a uuid-shaped canonical token id (the store generates
// uuid ids; the service's shape check rejects anything else).
const testTokenID = "00000000-0000-4000-8000-000000000042"

// TestRevokeTokenProviderFirst: the provider delete precedes the canonical
// flip — a crash between them fails secure, and a retry converges.
func TestRevokeTokenProviderFirst(t *testing.T) {
	order := &[]string{}
	port := &fakeUserPort{order: order}
	store := &fakeAccessStore{order: order, token: gitprovider.TokenRecord{
		ID:            testTokenID,
		ProjectID:     testProjectID,
		UserID:        testUserID,
		GiteaUsername: "u-" + testUserID,
		GiteaTokenID:  42,
		Status:        "active",
	}}
	s := newTestUserAccess(port, store)

	if err := s.RevokeToken(t.Context(), testUserID, testProjectID, testTokenID); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	wantOrder := []string{
		"store.get:" + testTokenID,
		"port.delete:u-" + testUserID + ":42",
		"store.revokeToken:" + testTokenID,
	}
	if len(*order) != len(wantOrder) {
		t.Fatalf("order = %v, want %v", *order, wantOrder)
	}
	for i := range wantOrder {
		if (*order)[i] != wantOrder[i] {
			t.Errorf("call[%d] = %q, want %q", i, (*order)[i], wantOrder[i])
		}
	}
	if len(store.marksToken) != 1 || store.marksToken[0] != testTokenID {
		t.Errorf("marksToken = %v, want [%s]", store.marksToken, testTokenID)
	}
}

// TestRevokeTokenNotOwned: a token of another user (or another project)
// answers ErrTokenNotOwned and touches nothing.
func TestRevokeTokenNotOwned(t *testing.T) {
	order := &[]string{}
	port := &fakeUserPort{order: order}
	store := &fakeAccessStore{order: order, token: gitprovider.TokenRecord{
		ID: testTokenID, ProjectID: testProjectID, UserID: "someone-else", Status: "active",
	}}
	s := newTestUserAccess(port, store)

	err := s.RevokeToken(t.Context(), testUserID, testProjectID, testTokenID)
	if !errors.Is(err, gitprovider.ErrTokenNotOwned) {
		t.Fatalf("RevokeToken = %v, want ErrTokenNotOwned", err)
	}
	if len(*order) != 1 {
		t.Errorf("a foreign token reached further work: %v", *order)
	}
}

// TestRevokeTokenAlreadyRevoked: revocation is a no-op success — no
// provider call, no second flip.
func TestRevokeTokenAlreadyRevoked(t *testing.T) {
	order := &[]string{}
	port := &fakeUserPort{order: order}
	store := &fakeAccessStore{order: order, token: gitprovider.TokenRecord{
		ID: testTokenID, ProjectID: testProjectID, UserID: testUserID, Status: "revoked",
	}}
	s := newTestUserAccess(port, store)

	if err := s.RevokeToken(t.Context(), testUserID, testProjectID, testTokenID); err != nil {
		t.Fatalf("RevokeToken (already revoked) = %v, want nil", err)
	}
	if len(port.deletes) != 0 || len(store.marksToken) != 0 {
		t.Errorf("already-revoked token was touched again: deletes=%v marks=%v", port.deletes, store.marksToken)
	}
}

// TestRevokeTokenProviderFailsKeepsRowActive: the provider delete failed —
// the canonical row must stay active so a retry re-attempts the delete.
func TestRevokeTokenProviderFailsKeepsRowActive(t *testing.T) {
	order := &[]string{}
	port := &fakeUserPort{order: order, deleteErr: gitprovider.ErrUnavailable}
	store := &fakeAccessStore{order: order, token: gitprovider.TokenRecord{
		ID: testTokenID, ProjectID: testProjectID, UserID: testUserID,
		GiteaUsername: "u-" + testUserID, GiteaTokenID: 42, Status: "active",
	}}
	s := newTestUserAccess(port, store)

	err := s.RevokeToken(t.Context(), testUserID, testProjectID, testTokenID)
	if !errors.Is(err, gitprovider.ErrUnavailable) {
		t.Fatalf("RevokeToken = %v, want ErrUnavailable", err)
	}
	if len(store.marksToken) != 0 {
		t.Errorf("the canonical row was flipped despite the provider failure: %v", store.marksToken)
	}
}

// TestRevokeAccessCascade: the collaborator grant dies first, then every
// active token, then the access row flips revoked. The live grant is
// checked first — the returned true is what lets the caller write the
// audit entry.
func TestRevokeAccessCascade(t *testing.T) {
	order := &[]string{}
	port := &fakeUserPort{order: order}
	store := &fakeAccessStore{order: order, repoRef: testRepoRef(),
		access: gitprovider.AccessRecord{ProjectID: testProjectID, UserID: testUserID},
		list: []gitprovider.TokenRecord{
			{ID: "tok-1", GiteaUsername: "u-" + testUserID, GiteaTokenID: 41, Status: "active"},
			{ID: "tok-2", GiteaUsername: "u-" + testUserID, GiteaTokenID: 42, Status: "revoked"},
			{ID: "tok-3", GiteaUsername: "u-" + testUserID, GiteaTokenID: 43, Status: "active"},
		}}
	s := newTestUserAccess(port, store)

	revoked, err := s.RevokeAccess(t.Context(), testUserID, testProjectID)
	if err != nil {
		t.Fatalf("RevokeAccess: %v", err)
	}
	if !revoked {
		t.Fatal("RevokeAccess = false, want true (a live grant was revoked)")
	}
	if len(port.revokes) != 1 {
		t.Errorf("provider revokes = %v, want the one collaborator removal", port.revokes)
	}
	if len(port.deletes) != 2 || port.deletes[0] != "u-"+testUserID+" 41" || port.deletes[1] != "u-"+testUserID+" 43" {
		t.Errorf("deletes = %v, want provider deletes of the two ACTIVE tokens only", port.deletes)
	}
	if len(store.marksToken) != 2 || store.marksToken[0] != "tok-1" || store.marksToken[1] != "tok-3" {
		t.Errorf("marksToken = %v, want [tok-1 tok-3]", store.marksToken)
	}
	if len(store.marksAccess) != 1 {
		t.Errorf("marksAccess = %v, want one access-row flip", store.marksAccess)
	}
	// The grant lookup precedes the ref lookup, and the collaborator
	// removal precedes the token deletes: the provider enforces reach
	// through the grant, so killing the grant first is the strongest
	// ordering.
	if (*order)[0] != "store.getAccess:"+testProjectID+":"+testUserID {
		t.Errorf("call[0] = %q, want the grant lookup first", (*order)[0])
	}
	if (*order)[2] != "port.revoke:post-git-svc/p-"+testProjectID+":u-"+testUserID {
		t.Errorf("call[2] = %q, want the collaborator removal right after the ref lookup", (*order)[2])
	}
}

// TestRevokeAccessUnprovisioned: the repository row is gone, so there is
// no collaborator grant to remove — but provider tokens are account-,
// not repo-scoped, so any surviving token still dies and the canonical
// rows clear. The grant row existed (GetAccess answered it), so this is
// still a real revocation.
func TestRevokeAccessUnprovisioned(t *testing.T) {
	order := &[]string{}
	port := &fakeUserPort{order: order}
	store := &fakeAccessStore{order: order, repoRefErr: gitprovider.ErrRepoNotProvisioned,
		access: gitprovider.AccessRecord{ProjectID: testProjectID, UserID: testUserID},
		list: []gitprovider.TokenRecord{
			{ID: "tok-1", GiteaUsername: "u-" + testUserID, GiteaTokenID: 41, Status: "active"},
		}}
	s := newTestUserAccess(port, store)

	revoked, err := s.RevokeAccess(t.Context(), testUserID, testProjectID)
	if err != nil {
		t.Fatalf("RevokeAccess (unprovisioned): %v", err)
	}
	if !revoked {
		t.Fatal("RevokeAccess (unprovisioned) = false, want true (the grant row existed)")
	}
	if len(port.revokes) != 0 {
		t.Errorf("unprovisioned revoke removed a collaborator grant: %v", port.revokes)
	}
	if len(port.deletes) != 1 || port.deletes[0] != "u-"+testUserID+" 41" {
		t.Errorf("deletes = %v, want the surviving provider token dead", port.deletes)
	}
	if len(store.marksToken) != 1 || len(store.marksAccess) != 1 {
		t.Errorf("canonical rows not cleared: marksToken=%v marksAccess=%v", store.marksToken, store.marksAccess)
	}
}

// TestRevokeAccessNoGrant: no grant row exists, so the disconnect is a
// clean no-op — nothing reaches the provider or the store, and the
// caller learns NOT to write an audit entry (the review's major finding:
// the unconditional audit write let any authenticated user fabricate a
// revocation event).
func TestRevokeAccessNoGrant(t *testing.T) {
	order := &[]string{}
	port := &fakeUserPort{order: order}
	store := &fakeAccessStore{order: order, accessErr: gitprovider.ErrAccessNotFound}
	s := newTestUserAccess(port, store)

	revoked, err := s.RevokeAccess(t.Context(), testUserID, testProjectID)
	if err != nil {
		t.Fatalf("RevokeAccess (no grant): %v", err)
	}
	if revoked {
		t.Fatal("RevokeAccess (no grant) = true, want false (nothing was revoked)")
	}
	if len(*order) != 1 {
		t.Errorf("a no-grant revoke did more than the grant lookup: %v", *order)
	}
	if len(port.revokes) != 0 || len(port.deletes) != 0 || len(store.marksToken) != 0 || len(store.marksAccess) != 0 {
		t.Errorf("a no-grant revoke touched the provider or the store: revokes=%v deletes=%v marksToken=%v marksAccess=%v",
			port.revokes, port.deletes, store.marksToken, store.marksAccess)
	}
}

// TestRevokeAccessAlreadyRevoked: an already-revoked grant is also a
// no-op — the goal state holds, nothing to do, no audit.
func TestRevokeAccessAlreadyRevoked(t *testing.T) {
	order := &[]string{}
	port := &fakeUserPort{order: order}
	now := time.Now()
	store := &fakeAccessStore{order: order,
		access: gitprovider.AccessRecord{ProjectID: testProjectID, UserID: testUserID, RevokedAt: &now}}
	s := newTestUserAccess(port, store)

	revoked, err := s.RevokeAccess(t.Context(), testUserID, testProjectID)
	if err != nil {
		t.Fatalf("RevokeAccess (already revoked): %v", err)
	}
	if revoked {
		t.Fatal("RevokeAccess (already revoked) = true, want false (nothing was revoked)")
	}
	if len(*order) != 1 || len(port.revokes) != 0 || len(store.marksAccess) != 0 {
		t.Errorf("an already-revoked revoke did work: order=%v revokes=%v marks=%v",
			*order, port.revokes, store.marksAccess)
	}
}

// TestRevokeTokenValidates: a malformed token id answers ErrConflict
// before any store call (it must map to a 400, not a PostgreSQL
// uuid-cast error surfacing as a 503).
func TestRevokeTokenValidates(t *testing.T) {
	order := &[]string{}
	port := &fakeUserPort{order: order}
	store := &fakeAccessStore{order: order, token: gitprovider.TokenRecord{
		ID: "tok-1", ProjectID: testProjectID, UserID: testUserID, Status: "active",
	}}
	s := newTestUserAccess(port, store)

	err := s.RevokeToken(t.Context(), testUserID, testProjectID, "not-a-uuid")
	if !errors.Is(err, gitprovider.ErrConflict) {
		t.Fatalf("RevokeToken(malformed) = %v, want ErrConflict", err)
	}
	if len(*order) != 0 {
		t.Errorf("the malformed token id reached the store: %v", *order)
	}
}

// TestListTokensValidates: the list path applies the same id shape check.
func TestListTokensValidates(t *testing.T) {
	order := &[]string{}
	port := &fakeUserPort{order: order}
	store := &fakeAccessStore{order: order, repoRef: testRepoRef()}
	s := newTestUserAccess(port, store)

	_, err := s.ListTokens(t.Context(), "not-a-uuid", testProjectID)
	if !errors.Is(err, gitprovider.ErrConflict) {
		t.Fatalf("ListTokens = %v, want ErrConflict", err)
	}
	if len(*order) != 0 {
		t.Errorf("malformed ids reached the store: %v", *order)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
