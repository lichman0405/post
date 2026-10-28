// The WEB-05 browse fixture (T1306): a synthetic owner, a PRIVATE project
// and the research state the browse journey reads.
//
// WHY THIS IS SEEDED, AND WHAT IT DOES NOT PROVE. Every other WEB-2 journey
// starts from an account the browser created and state the browser wrote,
// because the product has a surface for writing it (signup, a project form,
// an organization form). WEB-05 is the BROWSE half of G06: the research map,
// the outline list and the object page READ the RSG; the creation surface
// that would let a reader author a question, a version and a relation is a
// later work package and does not exist on the web yet. A browse journey
// therefore needs research state to read, and docs/70 §6.3 allows exactly
// this: a synthetic seed for a journey whose subject is the read. It is the
// seed demo the plan names, and it must not — and does not — stand in for
// any creation step: nothing in this journey claims an object was authored,
// versioned or related through the UI.
//
// The account it also seeds exists so the seed has an owner. It is created
// with a real password hash and signed in through the product's own /login
// form (docs/70 §6.3: "产品支援的真实登入流程"); no session is planted.
//
// The project is PRIVATE on purpose. A public fixture would appear in every
// other journey's /projects listing — journey-web04 asserts a new account's
// directory is the answered empty state — so a public seed would silently
// rewrite another journey's premise. Private keeps this fixture exactly as
// visible as its one member.
package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence"
)

// The fixture's fixed identity. The journey recognises the project by its
// name and address the way a reader would (a directory row, a research tab),
// never by pasting an id.
const (
	web05ProjectSlug    = "isotherm-browse-lab"
	web05ProjectName    = "Isotherm Browse Lab"
	web05ProjectPurpose = "Synthetic browse fixture: a question tree, a claim, a finding and an experiment with their version logs."
	web05OwnerHandle    = "web05-owner"
	web05OwnerName      = "WEB-05 fixture owner"

	// The statements each object starts with and the patch its second
	// version applies. They differ so "the old version" is observable
	// content, not only a version number: selecting version 1 must render
	// version 1's words.
	web05QuestionV1 = "Does the material adsorb CO2 at 298 K?"
	web05QuestionV2 = "Does the material adsorb CO2 above 1 bar at 298 K?"
	web05ClaimV1    = "Capacity is 2 mmol/g at 1 bar"
	web05ClaimV2    = "Capacity is 2.4 mmol/g at 1 bar"
	web05FindingV1  = "Uptake peaks at 30 bar"
	web05FindingV2  = "Uptake peaks at 32 bar"
	web05ExperV1    = "Measure the CO2 isotherm at 298 K"
	web05ExperV2    = "Measure the CO2 isotherm at 298 K between 1 and 40 bar"
)

// web05Fixture is what the seed produced. Only the coordinates the READY
// line may carry live here; the journey reaches every one of them by
// clicking, so they are evidence, not a shortcut.
type web05Fixture struct {
	Email        string
	ProjectID    string
	ProjectSlug  string
	BranchID     string
	QuestionID   string
	ExperimentID string
	FindingID    string
}

// seedWeb05 creates the owner account, the private project, the research
// state and the outline's relations. Every step is a call into the SAME
// application services the production API calls — the seed writes exactly
// what a member's own writes would write, through the same validation gate,
// and a refusal here fails the harness start loudly (there is no partially
// seeded run).
func seedWeb05(
	ctx context.Context,
	pool *pgxpool.Pool,
	projectSvc *projects.Service,
	rsgSvc *rsg.Service,
	email, password string,
) (web05Fixture, error) {
	fixture := web05Fixture{Email: email, ProjectSlug: web05ProjectSlug}

	hash, err := authn.HashPassword(password)
	if err != nil {
		return fixture, fmt.Errorf("hash the fixture password: %w", err)
	}
	owner, err := persistence.NewCredentialStore(pool).CreateWithPassword(
		ctx, email, hash, web05OwnerHandle, web05OwnerName)
	if err != nil {
		return fixture, fmt.Errorf("create the fixture owner %s: %w", email, err)
	}

	project, _, err := projectSvc.Create(ctx, owner, projects.CreateProjectInput{
		Slug:       web05ProjectSlug,
		Name:       web05ProjectName,
		Purpose:    web05ProjectPurpose,
		Visibility: domain.VisibilityPrivate,
	})
	if err != nil {
		return fixture, fmt.Errorf("create the fixture project: %w", err)
	}
	fixture.ProjectID = project.ID

	branch, err := rsgSvc.CreateBranch(ctx, owner, project.ID, rsg.CreateBranchInput{
		Name:       "main",
		BaseRef:    "",
		Visibility: domain.BranchVisibilityPrivate,
	})
	if err != nil {
		return fixture, fmt.Errorf("create the fixture main branch: %w", err)
	}
	fixture.BranchID = branch.ID

	create := func(objectType, payload string) (rsg.ObjectResult, error) {
		return rsgSvc.CreateObject(ctx, owner, project.ID, branch.ID, rsg.CreateObjectInput{
			ObjectType: objectType,
			Payload:    json.RawMessage(payload),
		})
	}
	revise := func(objectID string, patch string) (rsg.ObjectVersionResult, error) {
		return rsgSvc.CreateObjectVersion(ctx, owner, project.ID, branch.ID, objectID,
			rsg.CreateObjectVersionInput{ExpectedVersion: 1, Patch: json.RawMessage(patch)})
	}

	question, err := create("research_question",
		`{"statement":`+quote(web05QuestionV1)+`,"question_state":"open"}`)
	if err != nil {
		return fixture, fmt.Errorf("create the fixture question: %w", err)
	}
	fixture.QuestionID = question.Object.ID

	claim, err := create("claim",
		`{"statement":`+quote(web05ClaimV1)+`,"claim_type":"quantitative"}`)
	if err != nil {
		return fixture, fmt.Errorf("create the fixture claim: %w", err)
	}

	finding, err := create("finding",
		`{"statement":`+quote(web05FindingV1)+`,"finding_type":"trend","assessment":"accepted","claim_version_refs":[`+quote(claim.Version.ID)+`]}`)
	if err != nil {
		return fixture, fmt.Errorf("create the fixture finding: %w", err)
	}
	fixture.FindingID = finding.Object.ID

	experiment, err := create("experiment",
		`{"objective":`+quote(web05ExperV1)+`}`)
	if err != nil {
		return fixture, fmt.Errorf("create the fixture experiment: %w", err)
	}
	fixture.ExperimentID = experiment.Object.ID

	// The outline's edges, pinned to the objects' FIRST versions on purpose:
	// the version log below supersedes them immediately, so the seed also
	// exercises the outline's documented rule that an edge pinned to a
	// superseded version still counts (internal/application/rsg/outline.go).
	for _, edge := range []struct {
		what   string
		source string
	}{
		{"the finding", finding.Version.ID},
		{"the experiment", experiment.Version.ID},
	} {
		if _, err := rsgSvc.CreateRelation(ctx, owner, project.ID, branch.ID, rsg.CreateRelationInput{
			RelationType:          "addresses_question",
			SourceObjectVersionID: edge.source,
			TargetObjectVersionID: question.Version.ID,
		}); err != nil {
			return fixture, fmt.Errorf("link %s to the question: %w", edge.what, err)
		}
	}

	// Second versions: every object in the journey gets an old version to
	// select, and the objects the journey browses keep their current title
	// one version ahead of it.
	if _, err := revise(question.Object.ID, `{"statement":`+quote(web05QuestionV2)+`}`); err != nil {
		return fixture, fmt.Errorf("revise the fixture question: %w", err)
	}
	if _, err := revise(claim.Object.ID, `{"statement":`+quote(web05ClaimV2)+`}`); err != nil {
		return fixture, fmt.Errorf("revise the fixture claim: %w", err)
	}
	if _, err := revise(finding.Object.ID, `{"statement":`+quote(web05FindingV2)+`}`); err != nil {
		return fixture, fmt.Errorf("revise the fixture finding: %w", err)
	}
	if _, err := revise(experiment.Object.ID, `{"objective":`+quote(web05ExperV2)+`}`); err != nil {
		return fixture, fmt.Errorf("revise the fixture experiment: %w", err)
	}

	return fixture, nil
}

// quote encodes one string as a JSON literal, so a fixture sentence may
// contain quotes or backslashes without making the payload text the test's
// escaping problem.
func quote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		// Marshalling a string cannot fail; keep the compiler honest.
		panic(err)
	}
	return string(b)
}
