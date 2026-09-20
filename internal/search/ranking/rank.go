package ranking

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/lichman0405/post/internal/search"
	"github.com/lichman0405/post/internal/search/retrieval"
)

// The ranking's sentinels.
var (
	// ErrNoScope: the caller has no resolved scope. It wraps
	// search.ErrNoActor, so a caller that already handles the scope layer's
	// refusal handles this one too.
	ErrNoScope = fmt.Errorf("ranking: %w", search.ErrNoActor)
	// ErrStore: a factor read failed. The ranking fails rather than
	// continuing with the candidates it happens to have facts for, because
	// "we could not read this version's evidence" and "this version has no
	// evidence" are different answers and the second one is what a missing
	// fact looks like in the output.
	ErrStore = errors.New("ranking: store")
)

// Ranker orders retrieval candidates by the six scientific factors.
//
// It holds a FactorStore and nothing else: no configuration, no weights, no
// tunable. There is deliberately nothing here to tune — a knob that changed
// the order would be a value the platform set without a human deciding it,
// which is what docs/10 §4 rules out for V1 and what a golden fixture could
// not then pin.
type Ranker struct {
	store FactorStore
	log   *slog.Logger
}

// Option tunes a Ranker.
type Option func(*Ranker)

// WithLogger sets the ranker's logger (default slog.Default()).
func WithLogger(log *slog.Logger) Option {
	return func(r *Ranker) { r.log = log }
}

// NewRanker builds a ranker over store.
//
// A nil store is REFUSED, and the contrast with NewRetriever's optional
// embedder is deliberate. A missing embedder is a supported state — the
// vector signal does not run and the result says so — but a missing factor
// store is not: every one of the six factors except the query match is read
// from it, so a ranker without one would still produce a confidently ordered
// list whose order was decided by the one factor it could compute. There is
// no honest way to return a "scientific ranking" that read no science, so the
// constructor refuses rather than degrading.
func NewRanker(store FactorStore, opts ...Option) (*Ranker, error) {
	if store == nil {
		return nil, errors.New("ranking: a ranker needs a factor store")
	}
	r := &Ranker{store: store, log: slog.Default()}
	for _, opt := range opts {
		opt(r)
	}
	return r, nil
}

// Rank orders res.Candidates for req under scope.
//
// req is the retrieval request the candidates came from — the same value
// T0904 was given — because the query/scope factor is a property of what was
// ASKED (which narrowing was in force) and cannot be recovered from the
// candidates alone: a candidate set with no narrowing and one narrowed to an
// entity the caller happened to recall only that of look identical
// afterwards, and the explanation has to say which it was.
//
// The result contains exactly the candidates it was given: re-ranking never
// adds, drops or rewrites one, and never consults anything but the six
// factors. Authorization is entirely upstream (retrieval.Retrieve), and this
// layer has no way to widen it — the only database read it issues is scoped
// and returns facts, not rows.
func (r *Ranker) Rank(ctx context.Context, scope search.Scope, req retrieval.Request, res retrieval.Result) (Result, error) {
	if !scope.Authenticated() {
		return Result{}, ErrNoScope
	}
	cands := res.Candidates
	if len(cands) == 0 {
		return Result{Query: res.Query, Factors: Factors(), Ranked: []Ranked{}}, nil
	}

	// ------------------------------------------------------------------
	// 1. Resolve every candidate to the object version it is about.
	//
	// A retrieval candidate carries a version id only when the traversal's
	// seed step happened to resolve it, and that step is bounded by
	// Limits.Seeds — a candidate budget, not a correctness boundary. The
	// ranking must not inherit it: a document whose pid is not resolved
	// because it fell outside the seed budget would silently rank as "no
	// evidence", which is a different claim from "not read". So the
	// resolution is redone here for every candidate that needs it, and the
	// candidate's own id is preferred when it already has one.
	versionOf, refused, err := r.resolveVersions(ctx, scope, cands)
	if err != nil {
		return Result{}, err
	}

	// ------------------------------------------------------------------
	// 2. Read the facts, once, for every distinct version in play. A
	// version already known to be scope-refused is NOT asked about: the
	// store would refuse it, and not asking is what keeps the refusal this
	// layer's own decision rather than a side effect of the SQL.
	ids := make([]string, 0, len(cands))
	seen := make(map[string]bool, len(cands))
	for _, c := range cands {
		id := versionOf[c.Ref]
		if id == "" || seen[id] || refused[c.Ref] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	sort.Strings(ids)
	facts, err := r.readFacts(ctx, scope, ids)
	if err != nil {
		return Result{}, err
	}

	// ------------------------------------------------------------------
	// 3. Assess and order.
	ranked := make([]Ranked, 0, len(cands))
	for _, c := range cands {
		id := versionOf[c.Ref]
		if id != "" && facts[id].ObjectVersionID == "" && !refused[c.Ref] {
			// A version we could not read facts for: either the caller's
			// scope does not cover it or it is not a row any more. Both are
			// reported to the caller as `unknown` (unreadAssessment) — the
			// log is for the operator who has to tell those two apart, which
			// the ranking deliberately does not guess at. A refused ref is
			// not logged: its absence is the known, named case.
			r.log.Debug("search: no ranking facts read for a candidate's version",
				"ref", c.Ref, "object_version_id", id)
		}
		ranked = append(ranked, assess(c, id, facts[id], req))
	}
	// The comparison is the factors and nothing else. SliceStable — not
	// Slice — because a full tie (two candidates identical on all six
	// factors) then keeps the order the retrieval returned them in, which is
	// the fusion's own total order: relevance the ranking cannot replace is
	// not discarded, and the sort adds no map-iteration nondeterminism
	// because it never compares equal elements.
	sort.SliceStable(ranked, func(i, j int) bool {
		return betterFactors(ranked[i].Factors, ranked[j].Factors)
	})
	for i := range ranked {
		ranked[i].Rank = i + 1
	}
	return Result{Query: res.Query, Factors: Factors(), Ranked: ranked}, nil
}

// resolveVersions maps each candidate's ref to the object version it is
// about ("" when it is about none), and marks the refs whose resolved
// version the caller's scope refuses to read.
//
// Two sources, in order of trust: the candidate's own ObjectVersionID (the
// traversal already pinned it, and re-deriving it would be a second answer to
// a question already settled), then the publication pid for a knowledge
// document. Nothing else resolves: an asset, a release and a state are not
// version-pinned objects (search.PinnedByIdentity, retrieval.seeds), and
// guessing a version for them would be ranking a candidate by facts about
// something else.
//
// The refused half is what keeps the two unresolved-looking cases apart. A
// pid that names no publication resolves to nothing: the candidate is
// genuinely `unversioned`. A pid whose publication pins a version OUTSIDE
// the caller's scope resolves to that version — which exists — and the ref
// is marked refused, so every database-backed factor reports `unknown` with
// the scope as the reason instead of claiming there is no version to read.
// The refused version's facts are never requested: the store would refuse
// them (fail-closed), and not asking is what makes the refusal this layer's
// own decision rather than the SQL's side effect.
func (r *Ranker) resolveVersions(ctx context.Context, scope search.Scope, cands []retrieval.Candidate) (map[string]string, map[string]bool, error) {
	out := make(map[string]string, len(cands))
	refused := make(map[string]bool, len(cands))
	pids := make([]string, 0, len(cands))
	byPID := make(map[string][]string, len(cands))
	seenPID := make(map[string]bool, len(cands))
	for _, c := range cands {
		if c.ObjectVersionID != "" {
			out[c.Ref] = c.ObjectVersionID
			continue
		}
		if c.EntityType != search.EntityKnowledge || c.Identity == "" {
			continue
		}
		if !seenPID[c.Identity] {
			seenPID[c.Identity] = true
			pids = append(pids, c.Identity)
		}
		byPID[c.Identity] = append(byPID[c.Identity], c.Ref)
	}
	if len(pids) == 0 {
		return out, refused, nil
	}
	sort.Strings(pids)
	admitted, refusedPids, err := r.store.VersionsForPids(ctx, scope, pids)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: publication versions: %w", ErrStore, err)
	}
	for pid, versionID := range admitted {
		for _, ref := range byPID[pid] {
			out[ref] = versionID
		}
	}
	for pid, versionID := range refusedPids {
		for _, ref := range byPID[pid] {
			out[ref] = versionID
			refused[ref] = true
		}
	}
	return out, refused, nil
}

// readFacts reads the fact sheet for every version in ids.
//
// A version the store does not return is NOT a version with no facts: the
// port's contract is that a version outside the caller's scope is absent, and
// assess() reports that as an unread factor rather than as an empty one.
func (r *Ranker) readFacts(ctx context.Context, scope search.Scope, ids []string) (map[string]VersionFacts, error) {
	if len(ids) == 0 {
		return map[string]VersionFacts{}, nil
	}
	rows, err := r.store.Factors(ctx, scope, ids)
	if err != nil {
		return nil, fmt.Errorf("%w: ranking facts: %w", ErrStore, err)
	}
	out := make(map[string]VersionFacts, len(rows))
	for _, row := range rows {
		out[row.ObjectVersionID] = row
	}
	return out, nil
}

// betterFactors reports whether a's factor levels beat b's, comparing the
// two lists position by position in priority order. The first factor whose
// levels differ decides; a prefix of equal levels is not a win.
func betterFactors(a, b []Assessment) bool {
	for i := range a {
		if i >= len(b) {
			break
		}
		if a[i].LevelRank != b[i].LevelRank {
			return a[i].LevelRank < b[i].LevelRank
		}
	}
	return false
}

// assess computes a candidate's six Assessments and its labels.
//
// It is the ONLY place a level is decided, and every level is decided from
// the candidate, the request and the fact sheet — never from a table of
// weights, and never from anything about who wrote the candidate (there is no
// author, organization, follower or contribution count in scope here, which
// is docs/14 §3's "不得主要按 popularity/star/organization prestige" made
// structural rather than promised).
func assess(cand retrieval.Candidate, versionID string, facts VersionFacts, req retrieval.Request) Ranked {
	haveFacts := versionID != "" && facts.ObjectVersionID != ""
	out := Ranked{
		Ref:             cand.Ref,
		Kind:            cand.Kind,
		EntityType:      cand.EntityType,
		ObjectType:      cand.ObjectType,
		Title:           cand.Title,
		Version:         cand.Version,
		ObjectVersionID: versionID,
		Candidate:       cand,
		Factors: []Assessment{
			assessQueryScope(cand, req),
			assessEvidence(versionID, facts, haveFacts),
			assessReview(versionID, facts, haveFacts),
			assessReproduction(versionID, facts, haveFacts),
			assessConflict(versionID, facts, haveFacts),
			assessVersion(versionID, facts, haveFacts),
		},
	}
	out.Labels = labelsFor(facts, haveFacts)
	return out
}

// assessment builds one Assessment, looking the level's position up rather
// than taking it as an argument: a level with no position is a table bug, and
// a panic here is better than an arbitrary rank.
func assessment(factor, level, reason string, facts map[string]int) Assessment {
	rank, ok := levelRank(factor, level)
	if !ok {
		panic("ranking: level " + level + " has no position on factor " + factor + "'s ladder")
	}
	return Assessment{Factor: factor, Level: level, LevelRank: rank, Reason: reason, Facts: facts}
}

// assessQueryScope is the one factor that reads no database row: it is a
// property of how the retrieval went, which the candidate's own signal list
// records.
//
// The narrowing is part of the reason and not part of the level, deliberately.
// The level says how the candidate MATCHED; the narrowing is the same for
// every candidate in one result (it comes from the request) and putting it in
// the level would make two results' levels incomparable for no gain.
func assessQueryScope(cand retrieval.Candidate, req retrieval.Request) Assessment {
	var text, facets, graph bool
	for _, hit := range cand.Signals {
		switch hit.Signal {
		case retrieval.SignalFullText, retrieval.SignalVector:
			text = true
		case retrieval.SignalFacets:
			facets = true
		case retrieval.SignalGraph:
			graph = true
		}
	}
	var level, reason string
	switch {
	case text:
		level = LevelQuestion
		reason = "recalled by the question's own content (" + strings.Join(signalNames(cand, retrieval.SignalFullText, retrieval.SignalVector), ", ") + ")"
	case facets:
		level = LevelConditions
		reason = "recalled by the structured filter the question carried, not by its wording"
	case graph:
		level = LevelRelated
		reason = "reached by relation traversal from a recalled document, not recalled itself"
	default:
		level = LevelUnattributed
		reason = "no recall signal is recorded on this candidate"
	}
	if narrowing := narrowingText(req); narrowing != "" {
		reason += "; " + narrowing
	}
	return assessment(FactorQueryScopeMatch, level, reason, nil)
}

// signalNames lists which of names the candidate was recalled by, in the
// order given, so the reason names its evidence rather than asserting it.
func signalNames(cand retrieval.Candidate, names ...string) []string {
	out := make([]string, 0, len(names))
	for _, want := range names {
		for _, hit := range cand.Signals {
			if hit.Signal == want {
				out = append(out, want)
				break
			}
		}
	}
	return out
}

// narrowingText describes the narrowing the question imposed, or "" when it
// imposed none.
func narrowingText(req retrieval.Request) string {
	parts := make([]string, 0, 3)
	if len(req.EntityTypes) > 0 {
		parts = append(parts, "narrowed to entity types "+strings.Join(req.EntityTypes, ", "))
	}
	if req.PublicOnly {
		parts = append(parts, "public rows only")
	}
	if len(req.Facets) > 0 {
		parts = append(parts, "a structured filter was applied")
	}
	if len(parts) == 0 {
		return "the question imposed no narrowing"
	}
	return "the question " + strings.Join(parts, "; ")
}

// assessEvidence reads docs/10 §3's review state over the assertions that
// target the version.
func assessEvidence(versionID string, facts VersionFacts, have bool) Assessment {
	if !have {
		return unreadAssessment(FactorEvidence, versionID, facts)
	}
	f := map[string]int{
		"assertions": facts.EvidenceAssertions,
		"reviewed":   facts.EvidenceReviewed,
		"rejected":   facts.EvidenceRejected,
		"direct":     facts.EvidenceDirect,
	}
	switch {
	case facts.EvidenceAssertions == 0:
		return assessment(FactorEvidence, LevelNoEvidence,
			"no evidence assertion targets this version", f)
	case facts.EvidenceReviewed > 0:
		return assessment(FactorEvidence, LevelReviewed, assertionsClause(facts)+
			" this version, "+itoa(facts.EvidenceReviewed)+" reviewed"+directClause(facts), f)
	case facts.EvidenceRejected > 0:
		return assessment(FactorEvidence, LevelRejected, assertionsClause(facts)+
			" this version, none reviewed and "+itoa(facts.EvidenceRejected)+" rejected"+directClause(facts), f)
	default:
		return assessment(FactorEvidence, LevelAsserted, assertionsClause(facts)+
			" this version, none reviewed"+directClause(facts), f)
	}
}

// assertionsClause renders the count with the verb it agrees with: "1 evidence
// assertion/targets", "2 evidence assertions/target". The reason is read by
// people, and a sentence whose verb does not agree with its subject is the
// kind of detail that makes a reader trust the rest of it less.
func assertionsClause(facts VersionFacts) string {
	if facts.EvidenceAssertions == 1 {
		return "1 evidence assertion targets"
	}
	return itoa(facts.EvidenceAssertions) + " evidence assertions target"
}

// directClause reports how many of the assertions are recorded as direct
// evidence, when any is — an omitted clause is "none", never "unknown".
func directClause(facts VersionFacts) string {
	if facts.EvidenceDirect == 0 {
		return ""
	}
	return ", " + itoa(facts.EvidenceDirect) + " direct"
}

// assessReview reads the scientific review dimension on the project state the
// version was created in (00061; docs/09 §5).
func assessReview(versionID string, facts VersionFacts, have bool) Assessment {
	if !have {
		return unreadAssessment(FactorReview, versionID, facts)
	}
	f := map[string]int{
		"scientific":        facts.ReviewsScientific,
		"approved":          facts.ReviewsApproved,
		"changes_requested": facts.ReviewsChangesRequested,
	}
	switch {
	case facts.ReviewsScientific == 0:
		return assessment(FactorReview, LevelUnreviewed,
			"no scientific review is recorded on the state this version was created in", f)
	case facts.ReviewsApproved > 0:
		return assessment(FactorReview, LevelApproved, plural(facts.ReviewsScientific, "scientific review")+
			" on the state this version was created in, "+itoa(facts.ReviewsApproved)+" approved"+changesClause(facts), f)
	default:
		return assessment(FactorReview, LevelReviewedState, plural(facts.ReviewsScientific, "scientific review")+
			" on the state this version was created in, none approved"+changesClause(facts), f)
	}
}

// changesClause reports the change requests among the reviews, when any is.
func changesClause(facts VersionFacts) string {
	if facts.ReviewsChangesRequested == 0 {
		return ""
	}
	return ", " + itoa(facts.ReviewsChangesRequested) + " requesting changes"
}

// assessReproduction reads docs/10 §4's `reproduces` / `fails_to_reproduce`
// and splits them by whether the asserting project is the version's own.
func assessReproduction(versionID string, facts VersionFacts, have bool) Assessment {
	if !have {
		return unreadAssessment(FactorReproduction, versionID, facts)
	}
	f := map[string]int{
		"reproduces":  facts.Reproduces,
		"independent": facts.ReproducesIndependent,
		"failed":      facts.FailsToReproduce,
	}
	failClause := ""
	if facts.FailsToReproduce > 0 {
		failClause = ", and " + plural(facts.FailsToReproduce, "recorded failure") + " to reproduce"
	}
	switch {
	case facts.Reproduces == 0:
		return assessment(FactorReproduction, LevelNoReproduction, "no reproduction is recorded"+failClause, f)
	case facts.ReproducesIndependent > 0 && facts.FailsToReproduce > 0:
		return assessment(FactorReproduction, LevelDisputedReproduction,
			plural(facts.Reproduces, "reproduction")+" recorded, "+itoa(facts.ReproducesIndependent)+
				" from another project"+failClause, f)
	case facts.ReproducesIndependent > 0:
		return assessment(FactorReproduction, LevelIndependent,
			plural(facts.Reproduces, "reproduction")+" recorded, "+itoa(facts.ReproducesIndependent)+" from another project", f)
	default:
		return assessment(FactorReproduction, LevelSelfReproduced,
			plural(facts.Reproduces, "reproduction")+" recorded, all from the version's own project"+failClause, f)
	}
}

// assessConflict reads contradictory evidence: assertions that contradict the
// version, and relation edges of type 'contradicts' touching it.
func assessConflict(versionID string, facts VersionFacts, have bool) Assessment {
	if !have {
		return unreadAssessment(FactorConflict, versionID, facts)
	}
	f := map[string]int{
		"evidence_assertions": facts.EvidenceContradicting,
		"relations":           facts.ContradictingRelations,
	}
	if facts.EvidenceContradicting == 0 && facts.ContradictingRelations == 0 {
		return assessment(FactorConflict, LevelUncontested,
			"nothing on the platform contradicts this version (no contradictory evidence assertion, no contradicts relation)", f)
	}
	// Each half is named only when it is non-zero, and the verb agrees with
	// the total: "1 contradictory evidence assertion touches" vs "2
	// contradictory evidence assertions and 1 contradicts relation touch".
	// A sentence that says "and 0 contradicts relations" reads as a
	// template, not as a finding.
	clauses := make([]string, 0, 2)
	if facts.EvidenceContradicting > 0 {
		clauses = append(clauses, plural(facts.EvidenceContradicting, "contradictory evidence assertion"))
	}
	if facts.ContradictingRelations > 0 {
		clauses = append(clauses, plural(facts.ContradictingRelations, "contradicts relation"))
	}
	verb := "touch"
	if facts.EvidenceContradicting+facts.ContradictingRelations == 1 {
		verb = "touches"
	}
	return assessment(FactorConflict, LevelContested,
		strings.Join(clauses, " and ")+" "+verb+" this version", f)
}

// assessVersion reads the candidate's freshness: the version ordinal against
// its object's newest, and the version's own lifecycle state.
func assessVersion(versionID string, facts VersionFacts, have bool) Assessment {
	if versionID == "" {
		return assessment(FactorVersion, LevelUnversioned,
			"the candidate does not resolve to a scientific object version, so no freshness is recorded", nil)
	}
	if !have {
		return unreadAssessment(FactorVersion, versionID, facts)
	}
	f := map[string]int{"version_no": facts.VersionNo, "newest_version_no": facts.NewestVersionNo}
	switch {
	case facts.LifecycleState == "aborted":
		return assessment(FactorVersion, LevelAborted,
			"version "+itoa(facts.VersionNo)+" was aborted; it is still returned, and ranked last", f)
	case facts.IsNewest && facts.LifecycleState == "active":
		return assessment(FactorVersion, LevelCurrent,
			"version "+itoa(facts.VersionNo)+" is the newest version of its object and is active", f)
	case !facts.IsNewest:
		return assessment(FactorVersion, LevelHistorical,
			"version "+itoa(facts.VersionNo)+" is not the newest version of its object (the newest is "+
				itoa(facts.NewestVersionNo)+")", f)
	default:
		return assessment(FactorVersion, LevelHistorical,
			"version "+itoa(facts.VersionNo)+" is the object's newest but its lifecycle state is \""+
				facts.LifecycleState+"\"", f)
	}
}

// unreadAssessment is the factor of a candidate whose version the caller's
// scope does not cover — or which no version could be resolved for.
//
// The level is `unknown` and never a benign one, and the reason says which of
// the two situations it is. This is the fail-closed direction: the ranking
// reads a project's evidence graph only when the caller's scope covers it
// (store.go), so the honest report of a version outside that scope is that
// nothing was read — "no evidence" would be a claim about another project's
// corpus made without looking at it.
func unreadAssessment(factor, versionID string, facts VersionFacts) Assessment {
	reason := "the version this candidate resolves to is not in the caller's project scope, so no facts about it were read"
	if versionID == "" {
		reason = "the candidate resolves to no scientific object version, so there are no facts to read"
	}
	return assessment(factor, LevelUnknown, reason, nil)
}

// labelsFor derives docs/10 §8's descriptive labels from the fact sheet.
//
// Each label restates a fact the assessments already state — none is a new
// judgement, and none is computed from a threshold nobody can see. An empty
// label set is honest and common: a version with reviewed, uncontested
// evidence carries none of the four.
func labelsFor(facts VersionFacts, have bool) []string {
	labels := make([]string, 0, 4)
	if !have {
		return labels
	}
	if facts.EvidenceAssertions == 0 {
		labels = append(labels, LabelLimitedEvidence)
	}
	if facts.EvidenceSupporting > 0 && facts.EvidenceContradicting > 0 {
		labels = append(labels, LabelMixedEvidence)
	}
	if facts.EvidenceContradicting > 0 || facts.ContradictingRelations > 0 {
		labels = append(labels, LabelActivelyContested)
	}
	if facts.ReproducesIndependent > 0 {
		labels = append(labels, LabelIndependentlyReproduced)
	}
	return sortLabels(labels)
}

// plural renders "1 assertion" / "2 assertions" — the reason is read by
// people, and "1 assertions" is the kind of detail that makes a reader trust
// the rest of the sentence less.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return itoa(n) + " " + noun + "s"
}

// itoa is strconv.Itoa without the import churn in a file that already has
// four helpers of this size.
func itoa(n int) string { return fmt.Sprintf("%d", n) }
