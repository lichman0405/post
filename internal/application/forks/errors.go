package forks

import "errors"

// The outcomes the fork service answers with. They are the service's own
// vocabulary (the transport layer maps them to codes); a store failure is
// never one of the refuse-everything sentinels, because "the store is
// down" and "you may not do this" are different answers.
var (
	// ErrValidation: the request's shape is wrong (a missing identity, an
	// unusable slug, a title or body outside the domain bounds).
	ErrValidation = errors.New("forks: invalid request")
	// ErrForbidden: the caller may not fork this project, may not propose
	// to it, or may not propose from that branch. It is also the answer
	// for an unauthenticated caller: docs/04 §2 forks are for
	// authenticated users, and the matrix's public_anonymous column denies
	// all three of the cells this package implements.
	ErrForbidden = errors.New("forks: forbidden")
	// ErrProjectNotFound: the project is unknown, or the caller may not
	// read it — the existence-hiding answer a private project gives a
	// non-member (docs/45). A non-member forking a private project is
	// refused here, before any condition is resolved.
	ErrProjectNotFound = errors.New("forks: project not found")
	// ErrNoSourceBranch: the project being forked has no main branch to
	// fork. A project's canonical line is main (domain.MainBranchName);
	// without it there is no content to copy and no branch to record as
	// the lineage's source.
	ErrNoSourceBranch = errors.New("forks: the project being forked has no main branch")
	// ErrBranchNotFound: the source branch named by the request does not
	// exist in the project being forked — the same answer for an unknown
	// branch and for a branch of another project, because the read is
	// project-scoped (docs/45: no foreign existence leak).
	ErrBranchNotFound = errors.New("forks: the source branch is not in the project being forked")
	// ErrForkSlugTaken: the name the fork's project must have is held by a
	// project that is not this (parent, actor) pair's recorded fork. Two
	// situations collapse into this one answer, and neither is claimed over
	// the other because the record cannot tell them apart: the holder is one
	// of the actor's OWN projects — either a fork request of theirs between
	// the project insert and the lineage insert, or one of their own projects
	// that is not this fork and merely holds the name — or both the derived
	// name and the pair's reserved name were taken by others before the pair
	// ever got there. The answer states that; it does not report a fork being
	// created when no row shows one.
	//
	// It is not a transient by construction: a fork whose derived name is held
	// by ANOTHER actor moves to the reserved name and succeeds (see
	// forkProjectOverTakenName), so this is answered only where moving on
	// would mean a second fork project for one pair — which the slug's unique
	// index, not this service, is what actually forbids. A caller whose own
	// in-flight request settles will find the fork on a retry (the lineage row
	// then exists and the request is answered with it).
	ErrForkSlugTaken = errors.New("forks: the name the fork's project must have is already taken")
	// ErrStore: the backing store failed, so the outcome is unknown and
	// nothing is claimed about it.
	ErrStore = errors.New("forks: store failure")
)
