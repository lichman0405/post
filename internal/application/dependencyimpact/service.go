package dependencyimpact

import (
	"context"
	"errors"
	"fmt"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/events"
)

// Service answers the two questions this package has: what does a change
// reach (Analyze — no reader), and what may THIS caller be told about it
// (Read — a reader). Both run the same walk over the same store port; the
// only difference between them is the gate, which is why the gate is an
// argument of one method rather than a flag on the service.
type Service struct {
	store Store
	gate  ProjectGate
}

// NewService wires the service. gate may be nil only for a caller that
// never calls Read (the worker's pass, and tests of the walk): Read
// refuses a nil gate rather than treating it as "everyone may read
// everything", because that default is the leak docs/23 §5 forbids and a
// nil port is how it would arrive.
func NewService(store Store, gate ProjectGate) *Service {
	return &Service{store: store, gate: gate}
}

// Analyze walks the dependency graph downstream of subject and returns
// every affected entity, with no reader in the way. This is the answer the
// worker acts on and the one the acceptance compares against a hand-
// computed closure; Read is the same answer through a caller's access.
func (s *Service) Analyze(ctx context.Context, subject Subject) (Analysis, error) {
	return AnalyzeSubject(ctx, s.store, subject)
}

// AnalyzeSubject is the walk, against any SubjectAnalyzer: the one
// definition of "what does a change to this reach".
//
// It is a package function rather than a method so that the worker's pass
// can run it inside its own transaction — persistence's AnalyzeBatch holds
// the transaction that makes a batch atomic and calls this over a
// transaction-bound analyzer. The alternative, a second traversal inside
// the store, would be a second answer to the same question, and the alert
// a pass writes could then disagree with the closure the acceptance
// compares against.
func AnalyzeSubject(ctx context.Context, store SubjectAnalyzer, subject Subject) (Analysis, error) {
	if subject.ID == "" {
		return Analysis{}, fmt.Errorf("%w: subject id is required", ErrValidation)
	}
	switch subject.Kind {
	case SubjectObject:
		return store.ObjectClosure(ctx, subject.ID)
	case SubjectAssetVersion:
		deps, err := store.AssetDependents(ctx, subject.ID)
		if err != nil {
			return Analysis{}, err
		}
		impacts := make([]Impact, 0, len(deps))
		for _, d := range deps {
			impacts = append(impacts, Impact{
				Kind: AffectedProject, ID: d.ProjectID, ProjectID: d.ProjectID,
				// An asset dependency is always one hop: the table records
				// a project's own declaration, and there is no further
				// dependent of a project in this graph. Saying "direct" is
				// therefore not a simplification, it is the whole truth of
				// that landing point.
				Hops: 1, Directness: Direct,
			})
		}
		sortImpacts(impacts)
		return Analysis{Subject: subject, Impacts: impacts}, nil
	default:
		return Analysis{}, fmt.Errorf("%w: unknown subject kind %q", ErrValidation, subject.Kind)
	}
}

// Read answers what a CALLER may be told about what a change reaches.
//
// Two rules, and they are different rules for different questions:
//
//  1. THE SUBJECT must be readable by the caller, or the whole call
//     answers ErrSubjectNotFound. A caller may not ask "what does this
//     change reach" about a thing they cannot open: the answer would tell
//     them the thing exists, and for a private subject that is the leak.
//     The rule is fail-closed across the whole call rather than
//     per-subject, so a mixed list cannot be used to probe which of its
//     entries are real.
//
//  2. EVERY AFFECTED ENTITY is shown only if the caller may read the
//     project that owns it; the rest are DROPPED with no trace. Not
//     blanked, not counted, not "1 result withheld": the count is the
//     disclosure (docs/23 §5, and the rule T0707 wrote for the mirror
//     direction of the same table — internal/assets/project_dependency.go:
//     "a dropped row leaves no entry, no placeholder and no number, so a
//     reader cannot infer how many were dropped"). This is what makes a
//     private project's use of a public asset invisible to everyone
//     outside it: the asset's page may be public, its dependency list may
//     be public, and the private dependent is simply not in the answer.
//
// A third rule applies to the asset landing point alone: a declaration
// whose visibility_of_usage is private is the project's own business and is
// shown to a member of that project only (docs/23 §5's second axis, the
// one assets.ProjectDependencyViewer documents as "a non-member reading a
// public project ... sees the dependencies the project declared PUBLIC and
// nothing else"). It is the same table the asset page reads and the rule
// is borrowed rather than re-decided.
func (s *Service) Read(ctx context.Context, reader projects.Reader, subjects Subjects) (Report, error) {
	if s.gate == nil {
		return Report{}, fmt.Errorf("%w: this service was built without a project read gate", ErrStore)
	}
	if len(subjects) == 0 {
		return Report{}, fmt.Errorf("%w: at least one subject is required", ErrValidation)
	}
	for _, subject := range subjects {
		if subject.ID == "" {
			return Report{}, fmt.Errorf("%w: subject id is required", ErrValidation)
		}
		// The subject's own project is the gate this whole call hangs on;
		// the resolved visibility SubjectInfo carries is the emitter's
		// (BuildAlerts), not the reader's.
		info, err := s.store.SubjectInfo(ctx, subject)
		if err != nil {
			return Report{}, err
		}
		access, err := s.gate.Access(ctx, reader, info.ProjectID)
		if err != nil {
			return Report{}, err
		}
		if !access.Visible {
			// The gate answered "no" — which for a project the reader may
			// not read is the same answer as "no such project", and the
			// caller gets the same outcome (existence hiding, docs/45).
			return Report{}, fmt.Errorf("%w: %s", ErrSubjectNotFound, subject)
		}
	}

	groups := make([]SubjectImpacts, 0, len(subjects))
	for _, subject := range subjects {
		visible, err := s.visibleImpacts(ctx, reader, subject)
		if err != nil {
			return Report{}, err
		}
		impacts := make([]Impact, 0, len(visible))
		for _, v := range visible {
			access, err := s.gate.Access(ctx, reader, v.impact.ProjectID)
			if err != nil {
				return Report{}, err
			}
			if !access.Visible {
				// Dropped, with nothing said about it anywhere.
				continue
			}
			if v.privateUsage && !access.Member {
				// Readable project, declaration that is the project's own
				// business: still dropped, still uncounted.
				continue
			}
			impacts = append(impacts, v.impact)
		}
		sortImpacts(impacts)
		groups = append(groups, SubjectImpacts{Subject: subject, Impacts: impacts})
	}
	return Report{Groups: groups}, nil
}

// visibleImpact is one impact plus the one extra fact the asset landing
// point's second axis needs. It is unexported because it is a step in an
// authorization decision, not part of any answer.
type visibleImpact struct {
	impact Impact
	// privateUsage is true for an asset dependency declared with
	// visibility_of_usage = private. It is always false for an object
	// dependent: relation_versions has no usage-visibility column, and
	// inventing one here would be inventing a product rule.
	privateUsage bool
}

// visibleImpacts runs the walk for one subject and attaches the
// declaration-visibility fact. It does NOT authorize — Read does that, on
// the project gate, once per row.
func (s *Service) visibleImpacts(ctx context.Context, reader projects.Reader, subject Subject) ([]visibleImpact, error) {
	switch subject.Kind {
	case SubjectObject:
		analysis, err := s.store.ObjectClosure(ctx, subject.ID)
		if err != nil {
			return nil, err
		}
		out := make([]visibleImpact, 0, len(analysis.Impacts))
		for _, imp := range analysis.Impacts {
			out = append(out, visibleImpact{impact: imp})
		}
		return out, nil
	case SubjectAssetVersion:
		deps, err := s.store.AssetDependents(ctx, subject.ID)
		if err != nil {
			return nil, err
		}
		out := make([]visibleImpact, 0, len(deps))
		for _, d := range deps {
			out = append(out, visibleImpact{
				impact: Impact{
					Kind: AffectedProject, ID: d.ProjectID, ProjectID: d.ProjectID,
					Hops: 1, Directness: Direct,
				},
				privateUsage: d.VisibilityOfUsage == events.VisibilityPrivate,
			})
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%w: unknown subject kind %q", ErrValidation, subject.Kind)
	}
}

// BuildAlerts renders one alert event per impact of one trigger.
//
// It is a method on the service rather than a free function because it
// resolves the upstream project (one query per trigger, not per impact),
// and because it is the step a caller and a test both drive — the same
// call, so what a test asserts on cannot diverge from what a caller
// would emit.
//
// The review marker is AlertEvent's own constant (ReviewRequired): this
// analysis never changes a scientific conclusion, so "a human should look"
// is the only thing it can be saying (docs/18 §5).
func (s *Service) BuildAlerts(ctx context.Context, t Trigger, analysis Analysis) ([]events.Event, error) {
	info, err := s.store.SubjectInfo(ctx, t.Subject)
	if err != nil {
		return nil, err
	}
	out := make([]events.Event, 0, len(analysis.Impacts))
	for _, imp := range analysis.Impacts {
		ev, err := AlertEvent(t, info.ProjectID, imp)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, nil
}

// IsSubjectNotFound reports whether err is the existence-hiding answer of
// Read, so a transport can map it to the 404 shape every other
// visibility-gated read in this tree uses.
func IsSubjectNotFound(err error) bool { return errors.Is(err, ErrSubjectNotFound) }
