package authz

import "context"

// Engine evaluates authorization requests (docs/50: every public action
// passes an explicit authorization check). The contract is default deny:
// an unknown action, an unknown actor class, or any request an
// implementation cannot resolve MUST answer a denying Decision — silence
// is never permission, and a missing engine is never "everyone allowed".
//
// The V1 implementation is MatrixEngine (the static CSV matrix); the
// interface exists so later engines (T0603 organization/project policy,
// conditional resolvers) can replace it without touching the enforcement
// sites in the application services.
type Engine interface {
	// Authorize answers one authorization question. An error means the
	// engine could not decide (state unknowable); enforcement sites must
	// fail closed on it.
	Authorize(ctx context.Context, req Request) (Decision, error)
}

// Request is one authorization question.
type Request struct {
	// Action is the matrix row being asked about.
	Action Action
	// Class is the actor's matrix column (resolve with ClassOf).
	Class ActorClass
}

// MatrixEngine evaluates requests against the static permission matrix
// (matrix.go, mirroring specs/policies/permissions-matrix.csv — the
// CSV-consistency test keeps the two identical). It never fails: the
// matrix is complete, and anything not in it is denied.
type MatrixEngine struct{}

// NewMatrixEngine returns the V1 matrix engine.
func NewMatrixEngine() *MatrixEngine { return &MatrixEngine{} }

// Authorize implements Engine. Unknown actions and unknown actor classes
// answer VerdictDeny with a reason (default deny, docs/12).
func (e *MatrixEngine) Authorize(_ context.Context, req Request) (Decision, error) {
	row, ok := matrixTable[req.Action]
	if !ok {
		return Decision{
			Verdict: VerdictDeny,
			Reason:  "action is not in the permission matrix",
		}, nil
	}
	verdict, ok := row[req.Class]
	if !ok {
		return Decision{
			Verdict: VerdictDeny,
			Reason:  "actor class is not in the permission matrix",
		}, nil
	}
	return Decision{Verdict: verdict}, nil
}
