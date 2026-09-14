// Package integration — T0210 "object detail e2e": the scientific object
// detail page exercised end to end over REAL PostgreSQL, the real auth
// guard, the real RSG service and the real HTTP surface — signup, project
// create, branch create, object create/version, relation create and then
// the browser read of the detail page itself. The negative paths come
// first and they are the point: the page must be exactly as visible as
// the object it shows (existence-hiding 404, never page chrome for a
// denied read), version switching must actually switch versions, and the
// page must carry no scientific edit form of any kind.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/projectshttp"
	"github.com/lichman0405/post/cmd/api/rsghttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/application/validation"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/events"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/memstore"
	"github.com/lichman0405/post/internal/persistence/testdb"
	"github.com/lichman0405/post/internal/rsg/schemareg"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
)

const objectDetailTaskID = "T0210"

// objectDetailFixture is the full production composition (the same wiring
// cmd/api/main.go builds) plus the seeded data: alice owns a private
// project with a two-version material related to a dataset, and a public
// project with one object. bob is no member of anything.
type objectDetailFixture struct {
	ts *httptest.Server

	alice *testUserClient
	bob   *testUserClient
	anon  *testUserClient

	privateProjectID  string
	privateBranchID   string
	objectID          string // two versions: 1 then 2
	datasetID         string // one version
	versionedObjectID string // five versions: the truncation guard's control
	publicProjectID   string
	publicBranchID    string
	publicObjectID    string // one version
	hostileObjectID   string // payload name carries markup
}

// objectDetailPage fetches the object route with a browser Accept header
// and returns the response; the body is read by the assertions.
func (f *objectDetailFixture) page(t *testing.T, uc *testUserClient, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, f.ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/html")
	resp, err := uc.client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func (f *objectDetailFixture) objectPath(objectID string) string {
	return "/api/v1/projects/" + f.privateProjectID + "/branches/" + f.privateBranchID + "/objects/" + objectID
}

func newObjectDetailFixture(t *testing.T, ctx context.Context) *objectDetailFixture {
	t.Helper()
	pool, _ := testdb.Setup(t, ctx, adminURL(t), objectDetailTaskID)

	// --- composition (identical to cmd/api/main.go) ---
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
	projectAPI := projectshttp.New(projectshttp.Deps{
		Store: persistence.NewProjectStore(pool),
		Orgs:  orgStore,
		Authz: authz.NewMatrixEngine(),
	})
	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	stateStore := persistence.NewStateStore(pool)
	rsgSvc := rsg.NewService(rsg.Deps{
		Projects:  projectAPI.Service(),
		Branches:  branches.NewService(persistence.NewBranchStore(pool)),
		States:    states.NewService(stateStore, validation.NewGuard(rsgvalidation.NewValidator(reg), persistence.NewValidationTxProbe())),
		Latest:    stateStore,
		Objects:   persistence.NewScientificObjectStore(pool),
		Relations: persistence.NewRelationStore(pool),
		Profiles:  persistence.NewProfileStore(pool),
		Authz:     authz.NewMatrixEngine(),
		Schemas:   reg,
		Events:    events.Recorder{},
	})
	rsgAPI := rsghttp.New(rsghttp.Deps{Service: rsgSvc})
	apiMux := http.NewServeMux()
	apiMux.Handle("/api/v1/auth/", authAPI.Routes())
	apiMux.Handle("/api/v1/projects", projectAPI.Routes())
	apiMux.Handle("/api/v1/projects/", projectAPI.Routes())
	rsgAPI.Register(apiMux)
	ts := httptest.NewServer(authAPI.Guard(apiMux))
	t.Cleanup(ts.Close)

	alice, _ := signup(t, ts.URL, "detail-alice@example.com", "detail-alice")
	bob, _ := signup(t, ts.URL, "detail-bob@example.com", "detail-bob")
	anon := newTestUserClient(ts.URL)

	// --- alice creates the private project and branch over the wire ---
	resp := alice.do(t, http.MethodPost, "/api/v1/projects",
		`{"slug":"detail-lab","name":"Detail Lab","purpose":"exercise the object detail page","visibility":"private"}`)
	mustStatus(t, resp, http.StatusCreated)
	var privateProject projectResponse
	if err := json.NewDecoder(resp.Body).Decode(&privateProject); err != nil {
		t.Fatalf("create private project payload: %v", err)
	}
	privateProjectID := privateProject.Project.ID

	resp = alice.do(t, http.MethodPost, "/api/v1/projects/"+privateProjectID+"/branches",
		`{"name":"main","base_ref":"","visibility":"private"}`)
	mustStatus(t, resp, http.StatusCreated)
	var branchResp struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&branchResp); err != nil {
		t.Fatalf("create branch payload: %v", err)
	}
	privateBranchID := branchResp.ID

	// --- the object: two versions, then a related dataset ---
	resp = alice.do(t, http.MethodPost, "/api/v1/projects/"+privateProjectID+"/branches/"+privateBranchID+"/objects",
		`{"object_type":"material","payload":{"name":"MOF-5"}}`)
	mustStatus(t, resp, http.StatusCreated)
	var objectResp struct {
		ID        string `json:"id"`
		VersionID string `json:"version_id"`
		VersionNo int    `json:"version_no"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&objectResp); err != nil {
		t.Fatalf("create object payload: %v", err)
	}
	objectID := objectResp.ID

	resp = alice.do(t, http.MethodPost,
		"/api/v1/projects/"+privateProjectID+"/branches/"+privateBranchID+"/objects/"+objectID+":version",
		`{"expected_version":1,"patch":{"formula":"Zn4O(BDC)3"}}`)
	mustStatus(t, resp, http.StatusCreated)
	var v2 struct {
		VersionID string `json:"version_id"`
		VersionNo int    `json:"version_no"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v2); err != nil {
		t.Fatalf("version payload: %v", err)
	}
	if v2.VersionNo != 2 {
		t.Fatalf("second version = %d, want 2", v2.VersionNo)
	}

	resp = alice.do(t, http.MethodPost, "/api/v1/projects/"+privateProjectID+"/branches/"+privateBranchID+"/objects",
		`{"object_type":"dataset","payload":{"name":"isotherm series"}}`)
	mustStatus(t, resp, http.StatusCreated)
	var datasetResp struct {
		ID        string `json:"id"`
		VersionID string `json:"version_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&datasetResp); err != nil {
		t.Fatalf("create dataset payload: %v", err)
	}

	resp = alice.do(t, http.MethodPost, "/api/v1/projects/"+privateProjectID+"/branches/"+privateBranchID+"/relations",
		`{"relation_type":"derived_from","source_object_version_id":"`+v2.VersionID+`","target_object_version_id":"`+datasetResp.VersionID+`"}`)
	mustStatus(t, resp, http.StatusCreated)

	// --- the public project: one object anyone may read ---
	resp = alice.do(t, http.MethodPost, "/api/v1/projects",
		`{"slug":"detail-public","name":"Detail Public","purpose":"public detail fixture","visibility":"public"}`)
	mustStatus(t, resp, http.StatusCreated)
	var publicProject projectResponse
	if err := json.NewDecoder(resp.Body).Decode(&publicProject); err != nil {
		t.Fatalf("create public project payload: %v", err)
	}
	publicProjectID := publicProject.Project.ID

	resp = alice.do(t, http.MethodPost, "/api/v1/projects/"+publicProjectID+"/branches",
		`{"name":"main","base_ref":"","visibility":"private"}`)
	mustStatus(t, resp, http.StatusCreated)
	if err := json.NewDecoder(resp.Body).Decode(&branchResp); err != nil {
		t.Fatalf("create public branch payload: %v", err)
	}
	publicBranchID := branchResp.ID

	resp = alice.do(t, http.MethodPost, "/api/v1/projects/"+publicProjectID+"/branches/"+publicBranchID+"/objects",
		`{"object_type":"material","payload":{"name":"Public MOF"}}`)
	mustStatus(t, resp, http.StatusCreated)
	var publicObject struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&publicObject); err != nil {
		t.Fatalf("create public object payload: %v", err)
	}

	// A second public object whose payload name carries markup: the page
	// must escape it, never render it.
	resp = alice.do(t, http.MethodPost, "/api/v1/projects/"+publicProjectID+"/branches/"+publicBranchID+"/objects",
		`{"object_type":"material","payload":{"name":"<script>alert(1)</script>"}}`)
	mustStatus(t, resp, http.StatusCreated)
	var hostileObject struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&hostileObject); err != nil {
		t.Fatalf("create hostile object payload: %v", err)
	}

	// A five-version object: the truncation guard's control. ?version=5
	// must render version 5, and ?version=4294967301 (2^32+5, which an
	// int32 truncation would turn into 5) must answer not-found instead
	// of silently rendering version 5.
	resp = alice.do(t, http.MethodPost, "/api/v1/projects/"+privateProjectID+"/branches/"+privateBranchID+"/objects",
		`{"object_type":"material","payload":{"name":"Versioned MOF"}}`)
	mustStatus(t, resp, http.StatusCreated)
	var versionedObject struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&versionedObject); err != nil {
		t.Fatalf("create versioned object payload: %v", err)
	}
	for i := 2; i <= 5; i++ {
		resp = alice.do(t, http.MethodPost,
			"/api/v1/projects/"+privateProjectID+"/branches/"+privateBranchID+"/objects/"+versionedObject.ID+":version",
			fmt.Sprintf(`{"expected_version":%d,"patch":{"note":"v%d"}}`, i-1, i))
		mustStatus(t, resp, http.StatusCreated)
	}

	return &objectDetailFixture{
		ts:                ts,
		alice:             alice,
		bob:               bob,
		anon:              anon,
		privateProjectID:  privateProjectID,
		privateBranchID:   privateBranchID,
		objectID:          objectID,
		datasetID:         datasetResp.ID,
		versionedObjectID: versionedObject.ID,
		publicProjectID:   publicProjectID,
		publicBranchID:    publicBranchID,
		publicObjectID:    publicObject.ID,
		hostileObjectID:   hostileObject.ID,
	}
}

// TestObjectDetailE2E is the required "object detail e2e" test.
func TestObjectDetailE2E(t *testing.T) {
	ctx := testCtx(t)
	f := newObjectDetailFixture(t, ctx)

	// --- denial first: the page is exactly as visible as the object ---
	t.Run("private page hidden from anonymous", func(t *testing.T) {
		resp := f.page(t, f.anon, f.objectPath(f.objectID))
		mustStatus(t, resp, http.StatusNotFound)
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("Content-Type = %q, want text/html", ct)
		}
		body := readAll(t, resp)
		if !strings.Contains(body, "Not found") {
			t.Errorf("denied page lacks the neutral not-found title")
		}
		for _, chrome := range []string{"version-menu", "Work with Agent", "MOF-5"} {
			if strings.Contains(body, chrome) {
				t.Errorf("denied read renders page chrome (%q) — existence must stay hidden", chrome)
			}
		}
	})

	t.Run("private page hidden from non-member", func(t *testing.T) {
		resp := f.page(t, f.bob, f.objectPath(f.objectID))
		mustStatus(t, resp, http.StatusNotFound)
		body := readAll(t, resp)
		if strings.Contains(body, "MOF-5") {
			t.Errorf("non-member page discloses the object")
		}
	})

	t.Run("denied JSON contract unchanged", func(t *testing.T) {
		resp := f.bob.do(t, http.MethodGet, f.objectPath(f.objectID), "")
		mustStatus(t, resp, http.StatusNotFound)
		mustEnvelope(t, resp, projects.CodeProjectNotFound)
	})

	// --- the page itself, anonymously on the public project ---
	t.Run("public page renders header facts and tabs", func(t *testing.T) {
		publicPath := "/api/v1/projects/" + f.publicProjectID + "/branches/" + f.publicBranchID + "/objects/" + f.publicObjectID
		resp := f.page(t, f.anon, publicPath)
		mustStatus(t, resp, http.StatusOK)
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("Content-Type = %q, want text/html", ct)
		}
		if vary := resp.Header.Get("Vary"); !strings.Contains(vary, "Accept") {
			t.Errorf("Vary = %q, want Accept listed", vary)
		}
		body := readAll(t, resp)
		for _, want := range []string{
			// object header: id, type, version, state (the task's facts)
			`data-object-id="` + f.publicObjectID + `"`,
			`<span class="type-badge">material</span>`,
			"Public MOF",
			`Version <strong>1</strong> of 1`,
			`data-lifecycle-state="active"`,
			// breadcrumb carries the project slug and branch
			"detail-public", "main",
			// all five tabs
			"Metadata", "Relations", "History", "Files", "Evidence",
			// Work with Agent CTA pre-filled with this object's coordinates
			"Work with Agent", "object.get", "object.create_version", f.publicObjectID,
			// creator resolved to the profile handle, not the raw id
			"detail-alice",
			// metadata facts
			"Object type", "Integrity hash", "Lifecycle state",
			// the read-only footer note
			"there is no scientific edit form on the web surface",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("public page lacks %q", want)
			}
		}
	})

	// --- acceptance: 无 web scientific edit form — nothing to submit ---
	t.Run("page has no edit form", func(t *testing.T) {
		publicPath := "/api/v1/projects/" + f.publicProjectID + "/branches/" + f.publicBranchID + "/objects/" + f.publicObjectID
		resp := f.page(t, f.anon, publicPath)
		mustStatus(t, resp, http.StatusOK)
		body := strings.ToLower(readAll(t, resp))
		for _, forbidden := range []string{"<form", "<input", "<textarea", "<button"} {
			if strings.Contains(body, forbidden) {
				t.Errorf("page contains %q — the detail page must have no edit form", forbidden)
			}
		}
	})

	// --- acceptance: 对象版本可切换 ---
	t.Run("version switching", func(t *testing.T) {
		// default: latest (2), merged payload
		resp := f.page(t, f.alice, f.objectPath(f.objectID))
		mustStatus(t, resp, http.StatusOK)
		body := readAll(t, resp)
		if !strings.Contains(body, `Version <strong>2</strong> of 2`) {
			t.Errorf("default page does not show latest version 2")
		}
		if !strings.Contains(body, "Zn4O(BDC)3") {
			t.Errorf("latest payload lacks the merged formula")
		}

		// ?version=1: the first version, without the patch key
		resp = f.page(t, f.alice, f.objectPath(f.objectID)+"?version=1")
		mustStatus(t, resp, http.StatusOK)
		body = readAll(t, resp)
		if !strings.Contains(body, `Version <strong>1</strong> of 2`) {
			t.Errorf("version=1 page does not show version 1")
		}
		if strings.Contains(body, "Zn4O(BDC)3") {
			t.Errorf("version=1 payload carries version 2's patch")
		}
		if !strings.Contains(body, "MOF-5") {
			t.Errorf("version=1 page lacks the object title")
		}

		// ?version=2: explicit latest
		resp = f.page(t, f.alice, f.objectPath(f.objectID)+"?version=2")
		mustStatus(t, resp, http.StatusOK)
		if body = readAll(t, resp); !strings.Contains(body, `Version <strong>2</strong> of 2`) {
			t.Errorf("version=2 page does not show version 2")
		}
	})

	t.Run("unknown version is a not-found page", func(t *testing.T) {
		resp := f.page(t, f.alice, f.objectPath(f.objectID)+"?version=99")
		mustStatus(t, resp, http.StatusNotFound)
		if body := readAll(t, resp); !strings.Contains(body, "object version not found") {
			t.Errorf("unknown version page: %s", body)
		}
		// A version beyond the one-version public object answers the same.
		publicPath := "/api/v1/projects/" + f.publicProjectID + "/branches/" + f.publicBranchID + "/objects/" + f.publicObjectID
		resp = f.page(t, f.anon, publicPath+"?version=2")
		mustStatus(t, resp, http.StatusNotFound)
	})

	t.Run("absurd version number is not silently truncated", func(t *testing.T) {
		// Control: version 5 really exists and renders.
		resp := f.page(t, f.alice, f.objectPath(f.versionedObjectID)+"?version=5")
		mustStatus(t, resp, http.StatusOK)
		if body := readAll(t, resp); !strings.Contains(body, `Version <strong>5</strong> of 5`) {
			t.Errorf("version=5 page does not show version 5")
		}
		// 2^32+5 fits in an int but not in the int4 version column: it
		// must answer not-found, never silently render version 5.
		resp = f.page(t, f.alice, f.objectPath(f.versionedObjectID)+"?version=4294967301")
		mustStatus(t, resp, http.StatusNotFound)
		if body := readAll(t, resp); !strings.Contains(body, "object version not found") {
			t.Errorf("absurd version page: %s", body)
		}
	})

	t.Run("malformed version parameter is answered in place", func(t *testing.T) {
		for _, param := range []string{"version=abc", "version=0"} {
			resp := f.page(t, f.alice, f.objectPath(f.objectID)+"?"+param)
			mustStatus(t, resp, http.StatusBadRequest)
			if body := readAll(t, resp); !strings.Contains(body, "version must be a positive integer") {
				t.Errorf("%s page: %s", param, body)
			}
		}
	})

	t.Run("relations tab shows the typed edge", func(t *testing.T) {
		resp := f.page(t, f.alice, f.objectPath(f.objectID)+"?tab=relations")
		mustStatus(t, resp, http.StatusOK)
		body := readAll(t, resp)
		for _, want := range []string{
			`data-tab-panel="relations"`,
			"derived_from",            // the relation type
			"isotherm series",         // the other endpoint's title
			"→",                       // direction: this object is the source
			"dataset",                 // the other endpoint's object type
			"/objects/" + f.datasetID, // link to the other object's page
			"detail-alice",            // creator resolved through the profile surface
		} {
			if !strings.Contains(body, want) {
				t.Errorf("relations tab lacks %q", want)
			}
		}
		// The metadata section (payload block) is not rendered on this tab
		// — "payload-pre" matches the stylesheet on every page, so probe the
		// section heading instead.
		if strings.Contains(body, `<h2 class="section-title">Payload</h2>`) {
			t.Errorf("relations tab renders the metadata payload block")
		}
	})

	t.Run("placeholder tabs answer honestly", func(t *testing.T) {
		tabText := map[string]string{
			"history":  "version history timeline arrives",
			"files":    "Files attached to this object are read-only",
			"evidence": "Evidence assertions arrive",
		}
		for tab, want := range tabText {
			resp := f.page(t, f.alice, f.objectPath(f.objectID)+"?tab="+tab)
			mustStatus(t, resp, http.StatusOK)
			if body := readAll(t, resp); !strings.Contains(body, want) {
				t.Errorf("%s tab lacks %q", tab, want)
			}
		}
	})

	t.Run("payload markup is escaped never rendered", func(t *testing.T) {
		publicPath := "/api/v1/projects/" + f.publicProjectID + "/branches/" + f.publicBranchID + "/objects/" + f.hostileObjectID
		resp := f.page(t, f.anon, publicPath)
		mustStatus(t, resp, http.StatusOK)
		body := readAll(t, resp)
		if strings.Contains(body, "<script>alert(1)</script>") {
			t.Errorf("page renders raw payload markup")
		}
		if !strings.Contains(body, "&lt;script&gt;alert") {
			t.Errorf("page lacks the escaped payload text")
		}
	})

	t.Run("JSON client still gets the envelope", func(t *testing.T) {
		resp := f.alice.do(t, http.MethodGet, f.objectPath(f.objectID), "")
		mustStatus(t, resp, http.StatusOK)
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		var payload map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if payload["id"] != f.objectID || payload["version_no"] != float64(2) {
			t.Errorf("payload = %v", payload)
		}
	})
}
