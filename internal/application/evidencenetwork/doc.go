// Package evidencenetwork is the network evidence read model (T0806,
// docs/10 §7): the assertions a Published Knowledge Object carries,
// presented under the three classes the specification names — Origin
// Evidence, Reviewed External Evidence, Unreviewed External Evidence.
//
// # What this package is, and what it is not
//
// It is the READ side of the network evidence feature: it takes the
// assertions one published object version carries (as the store returns
// them) plus the project that owns that version, and produces the section
// the public knowledge document renders. It writes nothing, and it owns no
// table: the assertions themselves live in evidence_assertions, written by
// the RSG service's evidence command (internal/application/rsg, on the same
// state-commit machinery every scientific-state write uses).
//
// # The classes are computed, never stored
//
// The two axes exist already — the asserting project versus the project
// that owns the published version, and evidence_assertions.review_state —
// so the classes are derived here with domain.ClassifyEvidenceNetwork.
// A stored three-valued column was ruled out (task scope narrowing): it
// would be a second source of truth for the review axis and would drift
// from review_state the first time an assertion was reviewed.
//
// # Fail closed
//
// Every rule in here answers "invisible" rather than "visible" when the
// facts are unclear: an unknown owner is external, an unknown review state
// never earns the reviewed class, and a row whose asserting project is not
// public (and is not the published object's own project) is dropped even if
// the stored assertion claims to be public. Nothing here scores, ranks,
// merges or adjudicates: two contradictory assertions on one pair stay two
// rows, each in its own class, with its own review state (CLAUDE.md §9.12
// and §9.13 — no net position, no Truth Score). docs/10 §8's descriptive
// labels (limited evidence, mixed evidence, actively contested,
// independently reproduced) are deliberately NOT produced: their rules are
// a scientific-semantics decision no specification fixes yet, so this build
// produces none of them rather than inventing thresholds.
package evidencenetwork
