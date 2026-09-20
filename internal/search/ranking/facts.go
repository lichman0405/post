package ranking

import (
	"context"

	"github.com/lichman0405/post/internal/search"
)

// VersionFacts is what the platform knows about one scientific object
// version, in the shape the six factors read it: counts and flags, one row
// per version, exactly as
// internal/persistence/queries/search.sql's SearchRankingFacts returns them.
//
// Every field is a count of rows somebody else wrote. None is derived from
// another field of this struct, and none is a score: the levels are decided
// in rank.go from these facts and from nothing else, which is what makes a
// position in a result reproducible from the database alone.
type VersionFacts struct {
	// ObjectVersionID is scientific_object_versions.id, the key every other
	// field is about.
	ObjectVersionID string
	// ObjectID is scientific_objects.id.
	ObjectID string
	// VersionNo is the version's ordinal within its object, and
	// NewestVersionNo is the object's highest — the two together are the
	// freshness claim, and both are reported so a reason can name the
	// newest version rather than only asserting that this is not it.
	VersionNo       int
	NewestVersionNo int
	// LifecycleState is 'active', 'aborted', 'reopened' or 'superseded'
	// (00005's CHECK).
	LifecycleState string
	// IsNewest reports whether this is the object's highest version_no.
	IsNewest bool

	// EvidenceAssertions is how many evidence assertions target this
	// version, and the three below split them by the axes docs/10 §3
	// records. They are counts of the assertions the CALLER may see: the
	// read admits an assertion whose project is in the caller's scope or
	// whose visibility is 'public' (00091).
	EvidenceAssertions    int
	EvidenceReviewed      int
	EvidenceRejected      int
	EvidenceDirect        int
	EvidenceSupporting    int
	EvidenceContradicting int

	// Reproduces is how many 'reproduces' assertions target this version;
	// ReproducesIndependent is how many of them were asserted by a project
	// other than the version's own. FailsToReproduce is the same relation
	// type's negative form (docs/10 §4).
	Reproduces            int
	ReproducesIndependent int
	FailsToReproduce      int

	// ReviewsScientific is how many scientific reviews were recorded on the
	// project state this version was created in; ReviewsApproved and
	// ReviewsChangesRequested split them by decision.
	ReviewsScientific       int
	ReviewsApproved         int
	ReviewsChangesRequested int

	// ContradictingRelations is how many distinct relations of type
	// 'contradicts' touch this version, under the traversal's project rule.
	ContradictingRelations int
}

// FactorStore reads the facts a ranking orders by.
//
// It is a port and not a *SQLStore for the reason retrieval.Store is one: the
// unit suite drives a fake, so everything those tests prove is about the
// ranking's own rules, and what only a real PostgreSQL can settle is pinned
// by tests/integration/ranking_test.go.
//
// The scope travels WITH the request and the ids are already-authorized
// candidates' versions. An implementation must apply the scope to the ids it
// was given — the same shape as retrieval.Store.SeedObjectVersions and for
// the same reason: the input is not a scope but a list of ids, so the scope
// has to be enforced on the output. A version outside the scope is simply
// ABSENT from the result (never a zero row, which would be indistinguishable
// from a version with no facts), and the ranking reports the factor as
// unknown for it.
type FactorStore interface {
	// VersionsForPids maps recalled publication pids to the object version
	// each pins (knowledge_publications.object_version_id — a publication
	// and its version are 1:1, 00083). The answer is split in two because
	// the two ways a pid can fail to resolve are different facts:
	//
	//   - admitted: the pinned version's project is one the caller's scope
	//     covers, so the version may be read;
	//   - refused: the publication pins a version whose project the scope
	//     does NOT cover. The version EXISTS — it is only unreadable — and
	//     the map carries its id so the ranking can say exactly that.
	//
	// A pid that names no publication at all is in neither map. The ranking
	// reports the three cases as three different outcomes (a resolved
	// version; `unknown` with the scope refusal as the reason; `unversioned`),
	// so a store must not collapse them: reporting a refused version as
	// unresolved would claim "there is no version" about one the caller is
	// simply not allowed to read.
	//
	// The scope is applied to the OUTPUT for the same reason it is on
	// Factors: the input is a list of pids the caller has already been
	// authorized to see (they came out of the document read), not a scope.
	VersionsForPids(ctx context.Context, scope search.Scope, pids []string) (admitted map[string]string, refused map[string]string, err error)

	// Factors returns one VersionFacts per requested id the caller's scope
	// admits. Ids it does not admit, and ids that name no version, are
	// absent.
	Factors(ctx context.Context, scope search.Scope, versionIDs []string) ([]VersionFacts, error)
}
