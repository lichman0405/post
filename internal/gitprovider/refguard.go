package gitprovider

import (
	"errors"
	"fmt"
	"strings"
)

// The platform layer of main's double protection (T0302, docs/16 §3):
// RefGuard is the platform's own policy over Git ref updates, applied
// wherever the platform observes or performs them — the push ingestion
// path (T0305) validates every delivery through it, and any future
// platform-side Git write (the merge service) runs through it before the
// write. It exists because the Gitea rule can be removed by an operator;
// when it is, the platform layer is what still refuses to treat a direct
// main push as legitimate (and the protection sweep in sweeper.go
// re-applies the Gitea rule).
//
// Scope: RefGuard restricts main and main only. Research branches are the
// product domain's concern (T0205 guards their lifecycle); force pushes on
// research branches are ordinary Git semantics and out of scope here.
//
// Force pushes to main need no separate rule: the only identity that may
// advance main is the merge service, and the merge service writes main
// exclusively through provider-side PR merges — it never pushes. Any
// force push to main therefore fails the actor rule below by construction.

// MainRef is the only ref this guard restricts (full ref name).
const MainRef = "refs/heads/main"

// Sentinel errors RefGuard returns. Callers decide policy per error (the
// ingestion path records a violation; the merge service aborts the write).
var (
	// ErrMainDirectPush: a ref update moved main without the merge
	// service's identity (a direct push, or a write through a bypass).
	ErrMainDirectPush = errors.New("gitprovider: direct push to protected main")
	// ErrMainDelete: an update deleted main.
	ErrMainDelete = errors.New("gitprovider: deletion of protected main")
)

// CodeMainFrozenDirectWrite is docs/45's stable code for a write to a
// frozen main (MAIN_FROZEN_DIRECT_WRITE_FORBIDDEN). The Git-side refusal
// carries it too, so an operator reading the two paths' refusals — the
// semantic write the states adapter refuses and the push the ingestion
// refuses — sees one rule, not two (T0601).
const CodeMainFrozenDirectWrite = "MAIN_FROZEN_DIRECT_WRITE_FORBIDDEN"

// MainFrozenRefusalError reports a Git-side write to main the platform
// refuses because the project's main is frozen (T0601): the push delivery
// reached the platform, but the project it belongs to has its main frozen,
// so the delivery is refused instead of being recorded as the branch's new
// head.
//
// It is a distinct type from ErrMainDirectPush and not a second spelling
// of it, because the two refusals answer different questions. ErrMainDirectPush
// is the identity rule ("this actor is not the merge service") and applies
// to every project, frozen or not; this error is the freeze rule ("this
// project's main may only advance through a Research PR merge") and applies
// to whoever pushed.
type MainFrozenRefusalError struct {
	// Ref is the ref the delivery moved (always refs/heads/main).
	Ref string
	// RepositoryID is the provider's numeric repository id, which is what
	// the provision row is keyed by.
	RepositoryID int64
}

// Error implements error.
func (e *MainFrozenRefusalError) Error() string {
	return fmt.Sprintf("gitprovider: main of the project provisioned for repository %d is frozen — a direct push to %s is refused (docs/09 §3: main advances only through a Research PR merge)",
		e.RepositoryID, e.Ref)
}

// Code is the stable wire code of this outcome (docs/45).
func (e *MainFrozenRefusalError) Code() string { return CodeMainFrozenDirectWrite }

// RefGuard is the platform ref guard.
type RefGuard struct {
	// MergeService is the provider login of the controlled identity that
	// may advance main (the platform merge service). An empty MergeService
	// fails closed: no identity may advance main.
	MergeService string
}

// RefUpdate is one ref transition the platform observed or is about to
// perform.
type RefUpdate struct {
	// Ref is the full ref name ("refs/heads/main").
	Ref string
	// OldSHA is the ref before the update; the zero sha when the ref is
	// being created.
	OldSHA string
	// NewSHA is the ref after the update; the zero sha when the ref is
	// being deleted.
	NewSHA string
	// Actor is the provider login performing the update (the push
	// payload's pusher login, or the identity the platform writes with).
	Actor string
	// ViaMerge marks an update the merge path produced (a provider-side
	// PR merge). Callers that know the write went through a merge set it;
	// the webhook receiver cannot tell and relies on Actor alone. The
	// guard does not require it — the merge service is identified by
	// Actor, and the Gitea rule guarantees that identity cannot push.
	ViaMerge bool
}

// zeroSHA is the all-zeroes sha Git uses for "no object".
const zeroSHA = "0000000000000000000000000000000000000000"

// Check validates one ref update against the guard's policy. A nil error
// means the update is allowed; the sentinel errors name the violation.
// Anything that is not main passes: the guard restricts main and main
// only, so tags and research-branch updates are not its concern.
func (g RefGuard) Check(u RefUpdate) error {
	ref := strings.TrimPrefix(u.Ref, "refs/heads/")
	if ref != "main" {
		return nil
	}
	if u.NewSHA == "" || u.NewSHA == zeroSHA {
		return ErrMainDelete
	}
	if u.Actor != g.MergeService {
		return ErrMainDirectPush
	}
	return nil
}
