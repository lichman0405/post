package pullrequestshttp

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"testing"

	yaml "go.yaml.in/yaml/v3"

	"github.com/lichman0405/post/internal/application/forks"
	"github.com/lichman0405/post/internal/application/pullrequests"
	"github.com/lichman0405/post/internal/domain"
)

// The contract check for the open-pull-request route, the sibling of the one
// T0814 adds in cmd/api/forkshttp/contract_test.go. It answers the questions
// this surface can answer about specs/api/openapi.yaml without policing prose
// it does not own:
//
//   - the path the contract declares resolves, in this package's route table,
//     to the pattern this package registers (and the lookup can say no);
//   - the statuses the contract declares for the route are EXACTLY the statuses
//     this transport can answer. Not a subset in either direction: a status the
//     code answers but the contract does not declare is a client that cannot
//     branch on the response, and a status the contract promises but the code
//     cannot produce is a promise the API does not keep. The comparison is
//     DRIVEN through the real handler over the real guard — the statuses are
//     observed responses, not the handler's own constants read back.
//   - the semantic-gate outcome the contract's prose names — a source branch
//     whose recorded state is unstructured_changes — is answered with exactly
//     the wire code the contract spells, and with a status the route declares.
//     The token is read off the contract, so renaming the code on one side
//     alone fails here.
//
// What is deliberately not checked here: every code the contract's prose
// names. Its 404 sentence names BRANCH_NOT_FOUND while this surface answers
// an unreadable project with PROJECT_NOT_FOUND (the code every other project
// read uses) — a looseness in the contract's wording, not a behaviour of this
// task, and one this test must not paper over by asserting a set that hides
// it.

// contractPRPath is the path the contract declares for opening a proposal.
const contractPRPath = "/projects/{projectId}/pull-requests"

type contractSpec struct {
	Paths map[string]map[string]contractOperation `yaml:"paths"`
}

type contractOperation struct {
	Description string                      `yaml:"description"`
	Responses   map[string]contractResponse `yaml:"responses"`
}

type contractResponse struct {
	Description string `yaml:"description"`
}

func loadContract(t *testing.T) contractSpec {
	t.Helper()
	// The test's working directory is this package:
	// cmd/api/pullrequestshttp.
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "specs", "api", "openapi.yaml"))
	if err != nil {
		t.Fatalf("read the contract: %v", err)
	}
	var spec contractSpec
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("parse the contract: %v", err)
	}
	if len(spec.Paths) == 0 {
		t.Fatal("the contract parsed empty — every check below would pass on anything")
	}
	return spec
}

func (c contractSpec) operation(t *testing.T, path, method string) contractOperation {
	t.Helper()
	methods, ok := c.Paths[path]
	if !ok {
		t.Fatalf("the contract declares no path %q", path)
	}
	op, ok := methods[method]
	if !ok {
		t.Fatalf("the contract declares no %s on %q", method, path)
	}
	if len(op.Responses) == 0 {
		t.Fatalf("the contract declares no responses for %s %s", method, path)
	}
	return op
}

func (op contractOperation) declares(status int) bool {
	for raw := range op.Responses {
		if n, err := strconv.Atoi(raw); err == nil && n == status {
			return true
		}
	}
	return false
}

// codeTokenRe matches a wire code named in the contract's prose: "code X",
// "wire code X", "(code X)".
var codeTokenRe = regexp.MustCompile(`\bcode\s+([A-Z][A-Z0-9_]+)\b`)

func namedCodes(op contractOperation) map[string]bool {
	out := map[string]bool{}
	texts := []string{op.Description}
	for _, r := range op.Responses {
		texts = append(texts, r.Description)
	}
	for _, text := range texts {
		for _, m := range codeTokenRe.FindAllStringSubmatch(text, -1) {
			out[m[1]] = true
		}
	}
	return out
}

// TestOpenPullRequestRouteIsTheRouteTheContractDeclares: the declared path
// resolves to the pattern this package registers, and the lookup can say no.
func TestOpenPullRequestRouteIsTheRouteTheContractDeclares(t *testing.T) {
	contract := loadContract(t)
	contract.operation(t, contractPRPath, "post")

	mux := http.NewServeMux()
	New(Deps{PullRequests: &fakePRs{}, Create: &fakeCreator{}, Projects: &fakeProjectGate{}}).Register(mux)

	const want = "POST /api/v1/projects/{projectId}/pull-requests"
	req, err := http.NewRequest(http.MethodPost, "http://api.test/api/v1/projects/pid/pull-requests", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, pattern := mux.Handler(req); pattern != want {
		t.Fatalf("the contract's path resolves to pattern %q, want %q", pattern, want)
	}
	miss, err := http.NewRequest(http.MethodPost, "http://api.test/api/v1/projects/pid/pull-request", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, pattern := mux.Handler(miss); pattern != "" {
		t.Fatalf("a path the contract does not declare resolved to pattern %q", pattern)
	}
}

// TestOpenPullRequestSemanticGateIsWhatTheContractSpells: the contract names
// BRANCH_UNSTRUCTURED_CHANGES for the unstructured semantic state (00042's
// pull_request_semantic_gate) and declares 409. The transport must answer
// exactly that code, with a declared status, and the message must not leak
// the Go error.
func TestOpenPullRequestSemanticGateIsWhatTheContractSpells(t *testing.T) {
	contract := loadContract(t)
	op := contract.operation(t, contractPRPath, "post")
	named := namedCodes(op)
	if !named["BRANCH_UNSTRUCTURED_CHANGES"] {
		t.Fatal("the contract no longer names BRANCH_UNSTRUCTURED_CHANGES for the open route — this check would pass on anything")
	}

	ts, client, _, csrf := newCreateTestServer(t, &fakeCreator{err: pullrequests.ErrBranchUnstructuredChanges})
	resp := createPost(t, client, createURL(ts, createTestProjectID), csrf, createKey, createBody)
	defer resp.Body.Close()

	if !op.declares(resp.StatusCode) {
		t.Fatalf("status = %d, which the contract does not declare for %s post", resp.StatusCode, contractPRPath)
	}
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (the contract's status for the unstructured semantic state)", resp.StatusCode)
	}
	got := decode[errorEnvelope](t, resp)
	if got.Code != pullrequests.CodeBranchUnstructuredChanges {
		t.Fatalf("code = %q, want %q — the code the contract spells",
			got.Code, pullrequests.CodeBranchUnstructuredChanges)
	}
	// The constant and the contract's token are the same string: this is what
	// makes the mapping above a claim about the contract rather than about
	// this package's own spelling.
	if pullrequests.CodeBranchUnstructuredChanges != "BRANCH_UNSTRUCTURED_CHANGES" {
		t.Fatalf("CodeBranchUnstructuredChanges = %q, want the contract's BRANCH_UNSTRUCTURED_CHANGES",
			pullrequests.CodeBranchUnstructuredChanges)
	}
}

// declaredStatuses is the set of statuses the contract declares for one
// operation.
func declaredStatuses(t *testing.T, op contractOperation) map[int]bool {
	t.Helper()
	out := map[int]bool{}
	for raw := range op.Responses {
		status, err := strconv.Atoi(raw)
		if err != nil {
			t.Fatalf("contract response key %q is not a status", raw)
		}
		out[status] = true
	}
	return out
}

func sortedStatuses(set map[int]bool) []int {
	out := make([]int, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Ints(out)
	return out
}

// drivenCreateOutcomes runs every outcome the open-pull-request route can
// report through the REAL handler — real guard, real ServeMux, fake command —
// and returns the statuses it answered with. What comes back is observed from
// responses; nothing here is read off the handler's own tables.
func drivenCreateOutcomes(t *testing.T) map[int]bool {
	t.Helper()
	statuses := map[int]bool{}
	for _, tc := range []struct {
		name   string
		create *fakeCreator
		anon   bool
		key    string
		body   string
	}{
		{name: "created", create: &fakeCreator{out: domainPR(1)}, key: createKey, body: createBody},
		{name: "anonymous", create: &fakeCreator{}, anon: true, key: createKey, body: createBody},
		{name: "missing idempotency key", create: &fakeCreator{}, key: "", body: createBody},
		{name: "short idempotency key", create: &fakeCreator{}, key: "short", body: createBody},
		{name: "malformed body", create: &fakeCreator{}, key: createKey, body: `{"source_branch_id":`},
		{name: "forbidden", create: &fakeCreator{err: forks.ErrForbidden}, key: createKey, body: createBody},
		{name: "project not found", create: &fakeCreator{err: forks.ErrProjectNotFound}, key: createKey, body: createBody},
		{name: "branch not found", create: &fakeCreator{err: pullrequests.ErrBranchNotFound}, key: createKey, body: createBody},
		{name: "branch not active", create: &fakeCreator{err: &pullrequests.BranchNotActiveError{BranchID: "branch-src", Lifecycle: "merged"}}, key: createKey, body: createBody},
		{name: "branch head missing", create: &fakeCreator{err: pullrequests.ErrBranchHeadMissing}, key: createKey, body: createBody},
		{name: "unstructured branch changes", create: &fakeCreator{err: pullrequests.ErrBranchUnstructuredChanges}, key: createKey, body: createBody},
		{name: "validation", create: &fakeCreator{err: pullrequests.ErrValidation}, key: createKey, body: createBody},
		{name: "store outage", create: &fakeCreator{err: pullrequests.ErrStore}, key: createKey, body: createBody},
		{name: "unknown", create: &fakeCreator{err: errors.New("boom")}, key: createKey, body: createBody},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts, client, _, csrf := newCreateTestServer(t, tc.create)
			var resp *http.Response
			if tc.anon {
				resp = createPost(t, http.DefaultClient, createURL(ts, createTestProjectID), "", tc.key, tc.body)
			} else {
				resp = createPost(t, client, createURL(ts, createTestProjectID), csrf, tc.key, tc.body)
			}
			defer resp.Body.Close()
			statuses[resp.StatusCode] = true
		})
	}
	return statuses
}

// domainPR returns a minimal domain pull request with the given number.
func domainPR(number int64) domain.PullRequest {
	return domain.PullRequest{
		ID:              "pr-1",
		ProjectID:       createTestProjectID,
		Number:          number,
		SourceBranchID:  "branch-src",
		TargetBranchID:  "branch-main",
		BaseStateID:     "state-main",
		ProposedStateID: "state-src",
		Title:           "annealing protocol",
		Body:            "the evidence is on the branch",
		State:           domain.PullRequestStateOpen,
		CreatedBy:       "someone",
	}
}

// TestOpenPullRequestAnswersExactlyTheStatusesTheContractDeclares compares the
// observed statuses with the declared set in both directions.
func TestOpenPullRequestAnswersExactlyTheStatusesTheContractDeclares(t *testing.T) {
	contract := loadContract(t)
	declared := declaredStatuses(t, contract.operation(t, contractPRPath, "post"))
	emitted := drivenCreateOutcomes(t)

	for status := range emitted {
		if !declared[status] {
			t.Errorf("the transport answered %d, which %s post does not declare (%v)",
				status, contractPRPath, sortedStatuses(declared))
		}
	}
	for status := range declared {
		if !emitted[status] {
			t.Errorf("the contract declares %d for %s post, and no outcome produces it (%v)",
				status, contractPRPath, sortedStatuses(emitted))
		}
	}
}

// TestOpenPullRequestContractStatusesAreTheRecordedSet records which statuses
// the contract declares, so a contract change that quietly drops one fails here
// with the set named rather than only as the first difference above.
func TestOpenPullRequestContractStatusesAreTheRecordedSet(t *testing.T) {
	contract := loadContract(t)
	got := sortedStatuses(declaredStatuses(t, contract.operation(t, contractPRPath, "post")))
	want := []int{201, 400, 401, 403, 404, 409, 503}
	if len(got) != len(want) {
		t.Fatalf("the contract declares %v; this test records %v — change it only together with the contract", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("the contract declares %v; this test records %v", got, want)
		}
	}
}
