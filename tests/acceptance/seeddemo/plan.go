// Package main is the seed demo builder (T1201): it builds the demo project
// described in docs/34_SEED_DEMO_PROJECT.md inside an empty database, driving
// every write through the product's own HTTP API, and verifies what landed by
// reading PostgreSQL directly. See ops/seed-demo.md.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Plan is examples/seed-demo/demo-plan.json: the demo's CONTENT. The
// sequence (which branch first, when a release is cut) is code — a plan file
// that also encoded the order would be a program in JSON.
type Plan struct {
	FormatVersion string `json:"format_version"`
	Synthetic     bool   `json:"synthetic"`
	Notice        string `json:"synthetic_notice"`
	Marker        struct {
		Tag         string `json:"tag"`
		MetadataKey string `json:"metadata_key"`
		Namespace   string `json:"namespace"`
		Comment     string `json:"comment"`
	} `json:"marker"`

	Organizations []PlanOrg  `json:"organizations"`
	Users         []PlanUser `json:"users"`

	Project struct {
		Slug       string `json:"slug"`
		Name       string `json:"name"`
		Purpose    string `json:"purpose"`
		Visibility string `json:"visibility"`
		Org        string `json:"org"`
	} `json:"project"`

	Branches []PlanBranch `json:"branches"`

	// ReviewRouting is the demo project's Research Owners configuration
	// (docs/04 §3): which responsibility is answerable for which changes,
	// and who holds it. Without it every pull request the demo opens is
	// unsatisfiable — internal/domain/review_routing.go treats a change no
	// rule routes as a change nobody answers for, and an unanswered change
	// never reaches merge_ready.
	ReviewRouting PlanRouting `json:"review_routing"`

	// MainObjects is the baseline line written on main BEFORE the first
	// reviewed merge: measurements and the questions they answer.
	MainObjects      []PlanObject   `json:"main_objects"`
	ProtocolVersions []PlanVersion  `json:"protocol_versions"`
	MainRelations    []PlanRelation `json:"main_relations"`
	// MainConclusions is what the project concludes from those measurements,
	// written on main AFTER the first merge. The order is not a preference:
	// this product admits an evidence assertion only against a version whose
	// lineage into main carries approved scientific + integrity reviews
	// (internal/application/rsg/evidence.go, knowledgepublish.Judge), and a
	// version written before the first merge has no such lineage. Claims are
	// conclusions — they belong after the campaign that produced them anyway.
	MainConclusions     []PlanObject   `json:"main_conclusions"`
	ConclusionRelations []PlanRelation `json:"conclusion_relations"`
	MainEvidence        []PlanEvidence `json:"main_evidence"`
	// HypothesisRevisions restate H1/H2 once the first campaign has an
	// answer. A restatement is a NEW VERSION, not an edit: the earlier
	// statement stays in the history (CLAUDE.md §9.8), and the new one is
	// what the network reads, because only a version written after the first
	// reviewed merge can be published.
	HypothesisRevisions []PlanVersion `json:"hypothesis_revisions"`

	BranchesContent map[string]PlanBranchContent `json:"branches_content"`

	PullRequests []PlanPR `json:"pull_requests"`

	Publication struct {
		Comment  string           `json:"_comment"`
		Rights   map[string]any   `json:"rights"`
		Versions []PlanPublishRef `json:"versions"`
	} `json:"publication"`

	Releases []PlanRelease `json:"releases"`
	Assets   []PlanAsset   `json:"assets"`

	External struct {
		Comment string `json:"_comment"`
		User    string `json:"user"`
		Org     string `json:"org"`
		Fork    struct {
			SourceBranch string `json:"source_branch"`
			Name         string `json:"name"`
			Purpose      string `json:"purpose"`
			Visibility   string `json:"visibility"`
			BranchName   string `json:"branch_name"`
		} `json:"fork"`
		Objects   []PlanObject   `json:"objects"`
		Relations []PlanRelation `json:"relations"`
		Evidence  []PlanEvidence `json:"evidence"`
		PR        PlanPR         `json:"pull_request"`
	} `json:"external_contribution"`

	// CollaborationDemo adds independently owned projects and accounts to the
	// primary research campaign. These records are kept beside the canonical
	// project rather than folded into its scientific story: each project is
	// created and written by its own signed-in account through the product API.
	CollaborationDemo     PlanCollaborationDemo `json:"collaboration_demo"`
	PresentationRevisions []PlanVersion         `json:"presentation_revisions"`
	PresentationRelations []PlanRelation        `json:"presentation_relations"`
}

type PlanCollaborationDemo struct {
	Organizations []PlanOrg         `json:"organizations"`
	Users         []PlanUser        `json:"users"`
	Projects      []PlanDemoProject `json:"projects"`
}

type PlanDemoProject struct {
	Key       string         `json:"key"`
	Slug      string         `json:"slug"`
	Name      string         `json:"name"`
	Purpose   string         `json:"purpose"`
	Org       string         `json:"org"`
	Owner     string         `json:"owner"`
	Objects   []PlanObject   `json:"objects"`
	Relations []PlanRelation `json:"relations"`
	Assets    []PlanAsset    `json:"assets"`
}

type PlanOrg struct {
	Key         string `json:"key"`
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

type PlanUser struct {
	Key         string `json:"key"`
	Email       string `json:"email"`
	Handle      string `json:"handle"`
	DisplayName string `json:"display_name"`
	Password    string `json:"password"`
	Org         string `json:"org"`
	OrgRole     string `json:"org_role"`
}

type PlanBranch struct {
	Name       string `json:"name"`
	Visibility string `json:"visibility"`
	Base       string `json:"base"`
	Purpose    string `json:"purpose"`
}

// PlanRouting is one project's Research Owners configuration as the plan
// declares it. MatchTypes lists the object types the routing covers; the
// builder refuses a plan whose routing does not cover every type it writes,
// because the product's own answer to that is a proposal that can never be
// approved.
type PlanRouting struct {
	Comment        string   `json:"_comment"`
	Responsibility string   `json:"responsibility"`
	MatchKind      string   `json:"match_kind"`
	MatchTypes     []string `json:"match_types"`
	AssignTo       []string `json:"assign_to"`
}

type PlanObject struct {
	Key     string         `json:"key"`
	Type    string         `json:"type"`
	Branch  string         `json:"branch"`
	Payload map[string]any `json:"payload"`
	// File is the object's data file, for the types this product requires one
	// from: a dataset's blob_ids and a calculation's output_blob_ids must be
	// non-empty before the state enters main (internal/rsg/validation's
	// requiredFields at GateMain), and every blob a payload names must resolve
	// in the proposal's manifest (internal/rsg/integrity's blobRefsResolve).
	// The content declared here IS the file: the blob row's hash and size are
	// computed from it, and `seeddemo verify` recomputes them.
	File *PlanFile `json:"file"`
}

// PlanFile is one synthetic data file: a media type and its bytes, written
// out as a JSON string so the plan stays one readable document.
type PlanFile struct {
	MediaType   string `json:"media_type"`
	Content     string `json:"content"`
	ContentPath string `json:"content_path"`
}

type PlanVersion struct {
	Object string         `json:"object"`
	Branch string         `json:"branch"`
	Patch  map[string]any `json:"patch"`
}

type PlanRelation struct {
	Type   string `json:"type"`
	Source string `json:"source"`
	Target string `json:"target"`
}

type PlanEvidence struct {
	Key             string `json:"key"`
	Target          string `json:"target"`
	Evidence        string `json:"evidence"`
	Relation        string `json:"relation"`
	EvidenceType    string `json:"evidence_type"`
	Directness      string `json:"directness"`
	InferenceNature string `json:"inference_nature"`
	ReasoningNote   string `json:"reasoning_note"`
}

type PlanBranchContent struct {
	Objects         []PlanObject   `json:"objects"`
	Relations       []PlanRelation `json:"relations"`
	Evidence        []PlanEvidence `json:"evidence"`
	ProtocolVersion *PlanVersion   `json:"protocol_version"`
	MainDivergent   *PlanVersion   `json:"main_divergent_version"`
}

type PlanPR struct {
	Key    string `json:"key"`
	Branch string `json:"branch"`
	Action string `json:"action"`
	Title  string `json:"title"`
	Body   string `json:"body"`
}

type PlanRelease struct {
	Key     string `json:"key"`
	Version string `json:"version"`
	Title   string `json:"title"`
	When    string `json:"when"`
}

// PlanPublishRef is one version the demo publishes to the network.
type PlanPublishRef struct {
	Object        string `json:"object"`
	PublicVersion string `json:"public_version"`
}

type PlanAsset struct {
	Key              string         `json:"key"`
	AssetType        string         `json:"asset_type"`
	Object           string         `json:"object"`
	Title            string         `json:"title"`
	Slug             string         `json:"slug"`
	Creators         []string       `json:"creators"`
	Dependencies     []string       `json:"dependencies"`
	ManifestMetadata map[string]any `json:"manifest_metadata"`
}

// LoadPlan reads and sanity-checks the plan file. The checks are the minimum
// that keeps a typo from silently shrinking the demo: every reference form
// the resolver understands is validated by Resolve at build time, and the
// synthetic marker the verifier depends on must be declared.
func LoadPlan(path string) (*Plan, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read plan %s: %w", path, err)
	}
	var p Plan
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("parse plan %s: %w", path, err)
	}
	if !p.Synthetic {
		return nil, fmt.Errorf("plan %s does not declare synthetic:true — every demo seed must be marked synthetic", path)
	}
	if p.Marker.Tag == "" || p.Marker.MetadataKey == "" {
		return nil, fmt.Errorf("plan %s has no marker.tag/marker.metadata_key: the seed would be unselectable", path)
	}
	if p.Project.Slug == "" {
		return nil, fmt.Errorf("plan %s has no project.slug", path)
	}
	for _, objects := range p.fileBearingObjectLists() {
		for i := range objects {
			if objects[i].File == nil || objects[i].File.ContentPath == "" {
				continue
			}
			if filepath.IsAbs(objects[i].File.ContentPath) {
				return nil, fmt.Errorf("plan %s has an absolute fixture path %q", path, objects[i].File.ContentPath)
			}
			content, err := readPlanFixture(path, objects[i].File.ContentPath)
			if err != nil {
				return nil, fmt.Errorf("read fixture %q for object %s: %w", objects[i].File.ContentPath, objects[i].Key, err)
			}
			objects[i].File.Content = string(content)
		}
	}
	return &p, nil
}

// readPlanFixture resolves fixture paths relative to the plan when the plan
// lives in the repository, and relative to the current checkout when the
// plan has been copied to a temporary directory (as mutation-check does).
func readPlanFixture(planPath, relative string) ([]byte, error) {
	planDir, _ := filepath.Abs(filepath.Dir(planPath))
	workingDir, _ := os.Getwd()
	starts := []string{planDir, workingDir}
	seen := map[string]bool{}
	var candidates []string
	add := func(path string) {
		path = filepath.Clean(path)
		if !seen[path] {
			seen[path] = true
			candidates = append(candidates, path)
		}
	}
	for _, start := range starts {
		if start == "" {
			continue
		}
		add(filepath.Join(start, relative))
		for dir := start; ; dir = filepath.Dir(dir) {
			if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
				add(filepath.Join(dir, relative))
				break
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
		}
	}
	var lastErr error
	for _, candidate := range candidates {
		content, err := os.ReadFile(candidate)
		if err == nil {
			return content, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("tried %s: %w", strings.Join(candidates, ", "), lastErr)
}

// fileBearingObjectLists returns slices backed by the parsed plan, so fixture
// contents are hydrated in place for the existing builder and verifier.
func (p *Plan) fileBearingObjectLists() [][]PlanObject {
	lists := [][]PlanObject{p.MainObjects, p.MainConclusions, p.External.Objects}
	for _, content := range p.BranchesContent {
		lists = append(lists, content.Objects)
	}
	for i := range p.CollaborationDemo.Projects {
		lists = append(lists, p.CollaborationDemo.Projects[i].Objects)
	}
	return lists
}

// publishRef returns the publication the plan declares for an object key.
func (p *Plan) publishRef(key string) (PlanPublishRef, bool) {
	for _, v := range p.Publication.Versions {
		if v.Object == key {
			return v, true
		}
	}
	return PlanPublishRef{}, false
}

// ObjectKeys returns every object key the plan declares, across the main
// project, branch sections, external contribution and collaboration projects.
func (p *Plan) ObjectKeys() []string {
	seen := map[string]bool{}
	var out []string
	add := func(o PlanObject) {
		if o.Key != "" && !seen[o.Key] {
			seen[o.Key] = true
			out = append(out, o.Key)
		}
	}
	for _, o := range p.MainObjects {
		add(o)
	}
	for _, o := range p.MainConclusions {
		add(o)
	}
	for _, name := range p.branchContentNames() {
		for _, o := range p.BranchesContent[name].Objects {
			add(o)
		}
	}
	for _, o := range p.External.Objects {
		add(o)
	}
	for _, project := range p.CollaborationDemo.Projects {
		for _, o := range project.Objects {
			add(o)
		}
	}
	return out
}

// ObjectByKey returns the plan's object declaration for a key, whichever
// section declares it. The builder needs it after a merge: the plan is where
// the object's file content lives, and a merged version needs its blob
// re-attached (see builder.refreshRefsAfterMerge).
func (p *Plan) ObjectByKey(key string) (PlanObject, bool) {
	search := func(list []PlanObject) (PlanObject, bool) {
		for _, o := range list {
			if o.Key == key {
				return o, true
			}
		}
		return PlanObject{}, false
	}
	if o, ok := search(p.MainObjects); ok {
		return o, true
	}
	if o, ok := search(p.MainConclusions); ok {
		return o, true
	}
	for _, name := range p.branchContentNames() {
		if o, ok := search(p.BranchesContent[name].Objects); ok {
			return o, true
		}
	}
	if o, ok := search(p.External.Objects); ok {
		return o, true
	}
	for _, project := range p.CollaborationDemo.Projects {
		if o, ok := search(project.Objects); ok {
			return o, true
		}
	}
	return PlanObject{}, false
}

// branchContentNames lists the plan's branch sections in a stable order, so
// every walk over them is deterministic.
func (p *Plan) branchContentNames() []string {
	names := make([]string, 0, len(p.BranchesContent))
	for name := range p.BranchesContent {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// refTable resolves the plan's symbolic references to the ids the API
// returned. A plan says "@mat-mof-x"; the builder knows the uuid.
type refTable struct {
	objects  map[string]string // key -> object id
	versions map[string]string // key -> current version id
	users    map[string]string // key -> user id
	blobs    map[string]string // key -> the object's file blob id
}

func newRefTable() *refTable {
	return &refTable{
		objects:  map[string]string{},
		versions: map[string]string{},
		users:    map[string]string{},
		blobs:    map[string]string{},
	}
}

func (r *refTable) set(key, objectID, versionID string) {
	r.objects[key] = objectID
	r.versions[key] = versionID
}

func (r *refTable) setBlob(key, blobID string) {
	r.blobs[key] = blobID
}

func (r *refTable) object(key string) (string, error) {
	id, ok := r.objects[key]
	if !ok {
		return "", fmt.Errorf("no object recorded for reference %q", key)
	}
	return id, nil
}

func (r *refTable) version(key string) (string, error) {
	id, ok := r.versions[key]
	if !ok {
		return "", fmt.Errorf("no version recorded for reference %q", key)
	}
	return id, nil
}

// Resolve walks a payload and replaces every "@..." reference with the id it
// names. Reference forms:
//
//	@key             the object's id
//	@v:key           the object's current version id
//	@vlist:a,b       the version ids of a,b
//	@idlist:a,b      the object ids of a,b
//	@user:key        the user's id
//	@blob:key        the blob id of the file written for the object
//
// An unknown form is an error, never a pass-through: a payload that kept the
// literal string "@v:proto-activation" would be written into the demo and
// only noticed by a reader.
func (r *refTable) Resolve(v any) (any, error) {
	switch t := v.(type) {
	case string:
		return r.resolveString(t)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			rv, err := r.Resolve(val)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			out[k] = rv
		}
		return out, nil
	case []any:
		out := make([]any, 0, len(t))
		for i, val := range t {
			rv, err := r.Resolve(val)
			if err != nil {
				return nil, fmt.Errorf("[%d]: %w", i, err)
			}
			// A reference that names several ids expands into this list
			// rather than becoming a list inside it: a plan writes
			// "claim_version_refs": ["@vlist:c-1,c-2"] and means the two
			// ids, not an array of one array.
			if inner, ok := rv.([]any); ok {
				out = append(out, inner...)
				continue
			}
			out = append(out, rv)
		}
		return out, nil
	default:
		return v, nil
	}
}

func (r *refTable) resolveString(s string) (any, error) {
	if !strings.HasPrefix(s, "@") {
		return s, nil
	}
	body := strings.TrimPrefix(s, "@")
	switch {
	case strings.HasPrefix(body, "vlist:"):
		keys := splitKeys(strings.TrimPrefix(body, "vlist:"))
		out := make([]any, 0, len(keys))
		for _, k := range keys {
			id, err := r.version(k)
			if err != nil {
				return nil, err
			}
			out = append(out, id)
		}
		return out, nil
	case strings.HasPrefix(body, "idlist:"):
		keys := splitKeys(strings.TrimPrefix(body, "idlist:"))
		out := make([]any, 0, len(keys))
		for _, k := range keys {
			id, err := r.object(k)
			if err != nil {
				return nil, err
			}
			out = append(out, id)
		}
		return out, nil
	case strings.HasPrefix(body, "v:"):
		return r.version(strings.TrimPrefix(body, "v:"))
	case strings.HasPrefix(body, "blob:"):
		key := strings.TrimPrefix(body, "blob:")
		id, ok := r.blobs[key]
		if !ok {
			return nil, fmt.Errorf("no file blob recorded for reference %q", key)
		}
		return id, nil
	case strings.HasPrefix(body, "user:"):
		key := strings.TrimPrefix(body, "user:")
		id, ok := r.users[key]
		if !ok {
			return nil, fmt.Errorf("no user recorded for reference %q", key)
		}
		return id, nil
	default:
		return r.object(body)
	}
}

func splitKeys(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
