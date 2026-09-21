package main

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/lichman0405/post/cmd/api/projectshttp"
	"github.com/lichman0405/post/internal/application/diffs"
	"github.com/lichman0405/post/internal/application/policy"
	"github.com/lichman0405/post/internal/application/prdiff"
	"github.com/lichman0405/post/internal/application/responsibilities"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence"
)

// pathServiceInProcess is the third path label. It exists because this
// product has a write the HTTP API does not serve: the Research Owners
// routing rules and responsibility assignments (docs/04 §3, migration 00084)
// are written by internal/application/responsibilities, which cmd/api
// constructs (cmd/api/main.go:789) and hands to the review service — but
// nothing registers an HTTP route for it. So a human with a browser cannot
// configure the routing of their own project today, and a pull request whose
// changes no rule routes can never reach merge_ready
// (internal/domain/review_routing.go: "missing routing is never an
// approval").
//
// The seed has to configure it or the demo's PRs stall at review_required.
// It configures it by calling the PRODUCT'S OWN SERVICE over the PRODUCT'S
// OWN PERSISTENCE ADAPTERS, in this process — the same composition
// cmd/api/main.go performs. That is not a direct database write (no INSERT
// appears in this file, and the rows carry the service's audit entries), and
// it is not the product API either. The report says so item by item, and
// RESULT.json carries the missing route as a follow-up issue.
const pathServiceInProcess = "product application service, in-process (the product exposes no HTTP route for review routing in this build)"

// reviewRouting wires the routing service exactly as the production
// composition root does, and returns it with the actor to write as.
func (b *builder) reviewRouting(ctx context.Context) (*responsibilities.Service, domain.User, error) {
	pool := b.db.pool
	projectStore := persistence.NewProjectStore(pool)
	projectAPI := projectshttp.New(projectshttp.Deps{
		Store: projectStore,
		Orgs:  persistence.NewOrgStore(pool),
		Authz: authz.NewMatrixEngine(),
	})
	prStore := persistence.NewPullRequestStore(pool)
	branchStore := persistence.NewBranchStore(pool)
	svc := responsibilities.NewService(responsibilities.Deps{
		Rules:    persistence.NewResponsibilityStore(pool),
		Projects: projectStore,
		Members:  projectAPI.Service(),
		PRs:      prStore,
		Branches: branchStore,
		// The same diff use case the PR page's routing runs on: what a
		// proposal changes is what the routing applies to.
		Diffs: prdiff.NewService(prStore, branchStore,
			diffs.NewService(persistence.NewStateStore(pool), persistence.NewManifestStore(pool), prStore)),
		Policies:  persistence.NewPolicyStore(pool),
		Evaluator: policy.NewRuleEvaluator(),
	})
	owner := b.planUser("owner")
	if owner == nil {
		return nil, domain.User{}, fmt.Errorf("plan has no user with key owner")
	}
	actor := domain.User{ID: b.refs.users["owner"], Email: owner.Email, Handle: owner.Handle}
	if actor.ID == "" {
		return nil, domain.User{}, fmt.Errorf("the owner user id is not known yet")
	}
	return svc, actor, nil
}

// ensureReviewRouting writes the demo's Research Owners configuration: one
// rule per object type the plan writes (so no change of the demo's is
// unrouted), and the responsibility the rules name held by the user who
// signs the demo's reviews. Both writes are idempotent by inspection first —
// the store's CreateRule answers ErrRuleExists for a mapping that already
// exists, and Assign is documented idempotent — so a second run reuses.
func (b *builder) ensureReviewRouting(ctx context.Context) error {
	cfg := b.plan.ReviewRouting
	if cfg.Responsibility == "" || len(cfg.MatchTypes) == 0 {
		return fmt.Errorf("the plan declares no review_routing.responsibility/match_types: no PR of the demo could reach merge_ready (docs/04 §3)")
	}
	// Fail fast on a type the plan uses but the routing does not cover: the
	// product answers that with a PR stuck at review_required, which is a
	// confusing way to learn about a typo.
	declared := map[string]bool{}
	for _, t := range cfg.MatchTypes {
		declared[t] = true
	}
	var missing []string
	for _, t := range b.plan.ObjectTypes() {
		if !declared[t] {
			missing = append(missing, t)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("plan review_routing.match_types does not cover object type(s) %v: every changed object needs a rule or the proposal is unsatisfiable", missing)
	}

	svc, actor, err := b.reviewRouting(ctx)
	if err != nil {
		return err
	}

	// The responsibility the rules name must be held by whoever signs the
	// reviews, or the recorded review carries no routed label and no
	// requirement is ever met (review_routing.pickResponsibility).
	for _, key := range cfg.AssignTo {
		user := b.planUser(key)
		if user == nil {
			return fmt.Errorf("review_routing.assign_to names user %q, which the plan does not declare", key)
		}
		userID := b.refs.users[key]
		if userID == "" {
			return fmt.Errorf("review_routing.assign_to names user %q, whose id is not known yet", key)
		}
		existing, err := svc.ListAssignments(ctx, actor, b.projectID)
		if err != nil {
			return fmt.Errorf("list responsibility assignments: %w", err)
		}
		if hasAssignment(existing, userID, cfg.Responsibility) {
			b.item("responsibility "+cfg.Responsibility+" for "+key, "reused", pathServiceInProcess,
				"already assigned to this user in this project")
			continue
		}
		assignment, err := svc.Assign(ctx, actor, b.projectID, userID, cfg.Responsibility)
		if err != nil {
			return fmt.Errorf("assign %s to %s: %w", cfg.Responsibility, key, err)
		}
		b.item("responsibility "+cfg.Responsibility+" for "+key, "created", pathServiceInProcess,
			fmt.Sprintf("user_id=%s responsibility=%q since=%s", assignment.UserID, assignment.Responsibility, assignment.CreatedAt.Format("2006-01-02")))
	}

	rules, err := svc.ListRules(ctx, actor, b.projectID)
	if err != nil {
		return fmt.Errorf("list research-owner rules: %w", err)
	}
	for _, t := range cfg.MatchTypes {
		if hasRule(rules, domain.ResearchOwnerMatchObjectType, t, cfg.Responsibility) {
			b.item("review routing rule object_type="+t, "reused", pathServiceInProcess,
				"already routes "+t+" changes to "+cfg.Responsibility)
			continue
		}
		rule, err := svc.AddRule(ctx, actor, b.projectID, responsibilities.AddRuleInput{
			MatchKind:      domain.ResearchOwnerMatchObjectType,
			MatchValue:     t,
			Responsibility: cfg.Responsibility,
		})
		if err != nil {
			if errors.Is(err, responsibilities.ErrRuleExists) {
				b.item("review routing rule object_type="+t, "reused", pathServiceInProcess,
					"the store already holds this mapping")
				continue
			}
			return fmt.Errorf("route object type %s: %w", t, err)
		}
		b.item("review routing rule object_type="+t, "created", pathServiceInProcess,
			"rule_id="+rule.ID+" responsibility="+cfg.Responsibility)
	}
	return nil
}

func hasAssignment(list []domain.ResponsibilityAssignment, userID, responsibility string) bool {
	for _, a := range list {
		if a.UserID == userID && a.Responsibility == responsibility {
			return true
		}
	}
	return false
}

func hasRule(list []domain.ResearchOwnerRule, kind domain.ResearchOwnerMatchKind, value, responsibility string) bool {
	for _, r := range list {
		if r.MatchKind == kind && r.MatchValue == value && r.Responsibility == responsibility {
			return true
		}
	}
	return false
}

// ObjectTypes returns the distinct scientific object types the plan writes,
// sorted. It is what the routing rules must cover.
func (p *Plan) ObjectTypes() []string {
	seen := map[string]bool{}
	add := func(o PlanObject) {
		if o.Type != "" {
			seen[o.Type] = true
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
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}
