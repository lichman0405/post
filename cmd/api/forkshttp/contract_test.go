package forkshttp

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"testing"

	yaml "go.yaml.in/yaml/v3"

	"github.com/lichman0405/post/internal/application/forks"
	"github.com/lichman0405/post/internal/domain"
)

// The contract check (T0814 acceptance criterion "contract-vs-implementation
// consistency"): read specs/api/openapi.yaml and compare it against what this
// transport actually answers, in BOTH directions.
//
//   - The path the contract declares must resolve, in this package's own
//     route table, to the pattern this package registers — a route mounted at
//     a different segment answers 404 to the client the contract was written
//     for. The lookup is shown able to say no (a mistyped path, and the same
//     path under the other verb, resolve to nothing), so the first assertion
//     is a measurement rather than a restatement.
//   - The statuses the contract declares for the route must be EXACTLY the
//     statuses this transport can answer. Not a subset in either direction: a
//     status the code answers but the contract does not declare is a client
//     that cannot branch on the response (docs/45's whole point), and a
//     status the contract promises but the code cannot produce is a promise
//     the API does not keep. The comparison is DRIVEN through the real
//     handler over the real guard — the statuses are observed responses, not
//     the handler's own constants read back.
//   - Every wire code the contract names in prose for this route is a code
//     this transport actually answered with, for the same reason.
//
// The same check for the sibling route T0814 touches (POST pull-requests)
// lives with that surface: cmd/api/pullrequestshttp/contract_test.go.

// contractForkPath is the path the contract declares for the fork route.
const contractForkPath = "/projects/{projectId}/forks"

// contractSpec is the slice of specs/api/openapi.yaml these checks read.
type contractSpec struct {
	Paths map[string]map[string]contractOperation `yaml:"paths"`
}

type contractOperation struct {
	Summary     string                      `yaml:"summary"`
	Description string                      `yaml:"description"`
	Responses   map[string]contractResponse `yaml:"responses"`
}

type contractResponse struct {
	Description string `yaml:"description"`
}

// loadContract reads the contract from the repository. An empty parse is a
// hard failure: a check that passes on anything proves nothing.
func loadContract(t *testing.T) contractSpec {
	t.Helper()
	// The test's working directory is this package: cmd/api/forkshttp.
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

// operation returns the contract's entry for one path and method.
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

// codeTokenRe matches a wire code named in the contract's prose: "code X",
// "wire code X", "(00042, code X)". It reads the contract's own words rather
// than a list kept here, so renaming a code on one side alone fails the check.
var codeTokenRe = regexp.MustCompile(`\bcode\s+([A-Z][A-Z0-9_]+)\b`)

// namedCodes is every wire code the operation's prose names.
func namedCodes(op contractOperation) map[string]bool {
	out := map[string]bool{}
	texts := append([]string{op.Description}, responseDescriptions(op)...)
	for _, text := range texts {
		for _, m := range codeTokenRe.FindAllStringSubmatch(text, -1) {
			out[m[1]] = true
		}
	}
	return out
}

func responseDescriptions(op contractOperation) []string {
	out := make([]string, 0, len(op.Responses))
	for _, r := range op.Responses {
		out = append(out, r.Description)
	}
	sort.Strings(out)
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

// drivenOutcomes runs every outcome the fork route can report through the
// REAL handler — real guard, real ServeMux, fake command — and returns the
// statuses and the wire codes it answered with. What comes back is observed
// from responses; nothing here is read off the handler's own tables.
func drivenOutcomes(t *testing.T) (statuses map[int]bool, codes map[string]bool) {
	t.Helper()
	statuses, codes = map[int]bool{}, map[string]bool{}
	for _, tc := range []struct {
		name  string
		actor *fakeForks
		gate  *fakeProjects
		anon  bool
		body  string
	}{
		{name: "created", actor: &fakeForks{out: forkResult()}, gate: &fakeProjects{visibility: domain.VisibilityPrivate}, body: `{}`},
		{name: "already forked", actor: &fakeForks{out: alreadyForked()}, gate: &fakeProjects{visibility: domain.VisibilityPrivate}, body: `{}`},
		{name: "validation", actor: &fakeForks{err: forks.ErrValidation}, gate: &fakeProjects{visibility: domain.VisibilityPublic}, body: `{}`},
		{name: "unreadable body", actor: &fakeForks{}, gate: &fakeProjects{visibility: domain.VisibilityPublic}, body: `{"name":`},
		{name: "anonymous", actor: &fakeForks{}, gate: &fakeProjects{visibility: domain.VisibilityPublic}, anon: true, body: `{}`},
		{name: "forbidden", actor: &fakeForks{err: forks.ErrForbidden}, gate: &fakeProjects{visibility: domain.VisibilityPublic}, body: `{}`},
		{name: "public fork of a non-public parent", actor: &fakeForks{}, gate: &fakeProjects{visibility: domain.VisibilityPrivate}, body: `{"visibility":"public"}`},
		{name: "project not found", actor: &fakeForks{err: forks.ErrProjectNotFound}, gate: &fakeProjects{visibility: domain.VisibilityPublic}, body: `{}`},
		{name: "branch not found", actor: &fakeForks{err: forks.ErrBranchNotFound}, gate: &fakeProjects{visibility: domain.VisibilityPublic}, body: `{}`},
		{name: "fork name taken", actor: &fakeForks{err: forks.ErrForkSlugTaken}, gate: &fakeProjects{visibility: domain.VisibilityPublic}, body: `{}`},
		{name: "store outage", actor: &fakeForks{err: forks.ErrStore}, gate: &fakeProjects{visibility: domain.VisibilityPublic}, body: `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts, client, _, csrf := newForkServer(t, Deps{Forks: tc.actor, Projects: tc.gate})
			var resp *http.Response
			if tc.anon {
				resp = anonymousPost(t, ts, tc.body)
			} else {
				resp = forkPost(t, client, forkURL(ts, testProjectID), csrf, tc.body)
			}
			defer resp.Body.Close()
			statuses[resp.StatusCode] = true
			if resp.StatusCode >= 400 {
				env := decodePayload[errorEnvelope](t, resp)
				if env.Code == "" {
					t.Errorf("status %d carried no wire code: a client cannot branch on it", resp.StatusCode)
				}
				codes[env.Code] = true
			}
		})
	}
	return statuses, codes
}

// TestForkRouteTheContractDeclaresIsTheRouteThisPackageServes: the path the
// contract declares resolves, in this package's route table, to the pattern
// this package registers — and the lookup can say no, which is what makes the
// first assertion a measurement.
func TestForkRouteTheContractDeclaresIsTheRouteThisPackageServes(t *testing.T) {
	contract := loadContract(t)
	contract.operation(t, contractForkPath, "post") // the method is declared

	mux := http.NewServeMux()
	New(Deps{Forks: &fakeForks{}, Projects: &fakeProjects{}}).Register(mux)

	const want = "POST /api/v1/projects/{projectId}/forks"
	req, err := http.NewRequest(http.MethodPost, "http://api.test/api/v1/projects/pid/forks", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, pattern := mux.Handler(req); pattern != want {
		t.Fatalf("the contract's path resolves to pattern %q, want %q", pattern, want)
	}

	// The controls: a path the contract does not declare, and the declared
	// path under the read verb. Neither may resolve to a pattern.
	miss, err := http.NewRequest(http.MethodPost, "http://api.test/api/v1/projects/pid/fork", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, pattern := mux.Handler(miss); pattern != "" {
		t.Fatalf("a path the contract does not declare resolved to pattern %q", pattern)
	}
	get, err := http.NewRequest(http.MethodGet, "http://api.test/api/v1/projects/pid/forks", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, pattern := mux.Handler(get); pattern != "" {
		t.Fatalf("a GET resolved to pattern %q; the fork route is a write", pattern)
	}
}

// TestForkRouteAnswersExactlyTheStatusesTheContractDeclares compares the
// observed statuses with the declared set in both directions.
func TestForkRouteAnswersExactlyTheStatusesTheContractDeclares(t *testing.T) {
	contract := loadContract(t)
	declared := declaredStatuses(t, contract.operation(t, contractForkPath, "post"))
	emitted, _ := drivenOutcomes(t)

	for status := range emitted {
		if !declared[status] {
			t.Errorf("the transport answered %d, which %s post does not declare (%v)",
				status, contractForkPath, sortedStatuses(declared))
		}
	}
	// A contract entry nothing can answer is a promise this API does not
	// keep, and it must fail here rather than be discovered by a client.
	for status := range declared {
		if !emitted[status] {
			t.Errorf("the contract declares %d for %s post, and no outcome produces it (%v)",
				status, contractForkPath, sortedStatuses(emitted))
		}
	}
}

// TestForkRouteEmitsTheWireCodesTheContractNames: the codes the contract's
// prose names for the route are codes this transport answered with.
func TestForkRouteEmitsTheWireCodesTheContractNames(t *testing.T) {
	contract := loadContract(t)
	named := namedCodes(contract.operation(t, contractForkPath, "post"))
	if len(named) == 0 {
		t.Fatal("the contract names no wire code for the fork route — this check would pass on anything")
	}
	_, emitted := drivenOutcomes(t)
	for code := range named {
		if !emitted[code] {
			t.Errorf("the contract names code %q for %s post, and no driven outcome answered it (%v)",
				code, contractForkPath, sortedCodes(emitted))
		}
	}
	// The control: the scan reads this route's prose and not a neighbour's.
	// BRANCH_UNSTRUCTURED_CHANGES is named for the pull-request route only;
	// if it showed up here the token scan would be reading text it should
	// not, and the check above would be measuring the wrong thing.
	if named["BRANCH_UNSTRUCTURED_CHANGES"] {
		t.Fatal("the fork route's prose names BRANCH_UNSTRUCTURED_CHANGES; the token scan is reading text it should not")
	}
}

func sortedCodes(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// TestForkRouteContractStatusesAreTheRecordedSet records which statuses the
// contract declares, so a contract change that quietly drops one fails here
// with the set named rather than only as the first difference above.
func TestForkRouteContractStatusesAreTheRecordedSet(t *testing.T) {
	contract := loadContract(t)
	got := sortedStatuses(declaredStatuses(t, contract.operation(t, contractForkPath, "post")))
	want := []int{200, 201, 400, 401, 403, 404, 409, 503}
	if len(got) != len(want) {
		t.Fatalf("the contract declares %v; this test records %v — change it only together with the contract", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("the contract declares %v; this test records %v", got, want)
		}
	}
}

// alreadyForked is the command's answer for a repeated request.
func alreadyForked() forks.ForkResult {
	res := forkResult()
	res.AlreadyForked = true
	res.Imported = false
	return res
}
