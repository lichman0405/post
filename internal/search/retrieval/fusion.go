package retrieval

import "sort"

// Fusion, and why it is rank-based.
//
// The three signals do not produce comparable numbers. ts_rank returns a
// small positive weight whose scale depends on the document length and the
// query's lexemes; the vector signal returns a cosine similarity in [-1, 1];
// the graph signal has no score at all, only a discovery order. A weighted
// sum over those three would be a formula whose coefficients mean nothing and
// whose behaviour changes when the corpus changes shape.
//
// Reciprocal Rank Fusion (Cormack et al., 2009) needs none of that: each
// signal contributes sum(1 / (k + rank)) over the positions it placed a
// candidate at, so only the ORDER within a signal matters. k = 60 is the
// constant from the paper and the one the literature uses; it flattens the
// head of each list, which is the property that matters here — a candidate
// ranked 1st by one signal does not automatically outrank a candidate ranked
// 2nd by two signals. That is the behaviour a hybrid search wants: agreeing
// signals count for more than one confident signal.
//
// What fusion is NOT is a relevance or quality measure. It is a recall
// ordering device: it decides what to spend the candidate budget on. docs/14
// §3's scientific ranking is a separate step (T0905), and CLAUDE.md §9.13
// forbids a research score outright. Nothing downstream may present
// Candidate.Score as an assessment of the underlying science.

// rrf is one signal's contribution for a candidate it placed at rank (1-based).
func rrf(rank int) float64 {
	if rank < 1 {
		return 0
	}
	return 1 / float64(rrfK+rank)
}

// accumulator collects candidates from every signal and keeps each one's
// fused score and signal list in step.
//
// It is an accumulator rather than a map-then-sort because a candidate can be
// contributed to more than once — by two signals, and again by a hop that
// arrives at a document already in the set — and every one of those
// contributions has to be folded into the SAME entry. The map is keyed by the
// candidate's ref, which is the citation identity: two candidates with the
// same ref are the same thing to a citation.
type accumulator struct {
	byRef map[string]*Candidate
	order []string // insertion order, for a stable tie-break before ranking
}

// newAccumulator builds an empty accumulator.
func newAccumulator() *accumulator {
	return &accumulator{byRef: make(map[string]*Candidate)}
}

// addHits folds one signal's result list in. The signal's own order IS its
// ranking: the queries all ORDER BY their score then their key, so the caller
// does not have to sort them and must not (a re-sort by the raw score would
// silently discard the query's total order for equal rows).
func (a *accumulator) addHits(signal string, hits []DocumentHit) {
	for i, hit := range hits {
		cand := documentCandidate(hit)
		a.add(cand, SignalHit{Signal: signal, Rank: i + 1, Score: hit.Score})
	}
}

// add folds one signal hit into the candidate with the same ref, creating it
// if it is new.
func (a *accumulator) add(cand Candidate, hit SignalHit) {
	if existing, ok := a.byRef[cand.Ref]; ok {
		existing.Signals = append(existing.Signals, hit)
		existing.Score += rrf(hit.Rank)
		return
	}
	cand.Signals = []SignalHit{hit}
	cand.Score = rrf(hit.Rank)
	a.byRef[cand.Ref] = &cand
	a.order = append(a.order, cand.Ref)
}

// put stores a candidate built outside the signal loop (a graph node), adding
// its score once.
func (a *accumulator) put(cand Candidate) {
	if _, ok := a.byRef[cand.Ref]; ok {
		// The caller checks byObjectVersion first; reaching here with a ref
		// already present would double-count the node. Refusing to overwrite
		// keeps the first, richer entry (a document keeps its visibility and
		// its text score) rather than replacing it with the bare node.
		return
	}
	a.byRef[cand.Ref] = &cand
	a.order = append(a.order, cand.Ref)
}

// byObjectVersion finds the candidate that already resolves to an object
// version id, if any. This is how a hop attaches to a document that was
// recalled by text — the document and the graph node are the same thing, and
// the result must say so once, with both of its reasons.
func (a *accumulator) byObjectVersion(versionID string) *Candidate {
	if versionID == "" {
		return nil
	}
	for _, ref := range a.order {
		if c := a.byRef[ref]; c != nil && c.ObjectVersionID == versionID {
			return c
		}
	}
	return nil
}

// countSignal counts the candidates reached by one signal. It is what the
// graph signal's report carries: the graph contributes hops to candidates
// other signals created as well, so "how many candidates does this signal
// appear on" is the honest number.
func (a *accumulator) countSignal(signal string) int {
	n := 0
	for _, c := range a.byRef {
		for _, hit := range c.Signals {
			if hit.Signal == signal {
				n++
				break
			}
		}
	}
	return n
}

// ranked returns the candidates, best-fused first.
//
// The order is total, and it has to be: fusion scores collide constantly
// (every candidate that exactly one signal placed at rank 3 has the same
// score), so a tie-break that depended on map iteration would make the same
// query return a different order run to run and the ranking fixture (T0905)
// unreproducible. Ties therefore break by (signal count DESC, ref ASC):
// more agreement first, then the citation identity — which is unique, so
// the order is total.
func (a *accumulator) ranked() []Candidate {
	pointers := a.rankedPointers()
	out := make([]Candidate, 0, len(pointers))
	for _, c := range pointers {
		out = append(out, *c)
	}
	return out
}

// rankedPointers is ranked's mutable view: the traversal enriches a document
// candidate with the object version its publication pins, and it has to
// enrich the accumulator's own entry rather than a copy of it.
func (a *accumulator) rankedPointers() []*Candidate {
	out := make([]*Candidate, 0, len(a.order))
	for _, ref := range a.order {
		out = append(out, a.byRef[ref])
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		if len(out[i].Signals) != len(out[j].Signals) {
			return len(out[i].Signals) > len(out[j].Signals)
		}
		return out[i].Ref < out[j].Ref
	})
	return out
}

// signalReports collects the per-signal report in a fixed order.
//
// The order is the pipeline's own (full text, vector, facets, graph) rather
// than the order the steps ran, so a reader of a result compares two runs by
// position and never has to wonder whether a missing entry means "did not
// run". Every signal is reported exactly once: a signal is either done or
// skipped with a reason, and a report that simply omitted the vector entry
// would be indistinguishable from an older revision that had no vector
// signal at all.
type signalReports struct {
	byName map[string]SignalReport
}

// done records that a signal ran and contributed n hits.
func (s *signalReports) done(name string, n int) {
	s.set(SignalReport{Signal: name, Ran: true, Hits: n})
}

// skipped records that a signal did not run, and why.
func (s *signalReports) skipped(name, reason string) {
	s.set(SignalReport{Signal: name, Ran: false, Skipped: reason})
}

func (s *signalReports) set(r SignalReport) {
	if s.byName == nil {
		s.byName = make(map[string]SignalReport, 4)
	}
	s.byName[r.Signal] = r
}

// list returns the reports in pipeline order, filling in any signal the
// caller forgot to report as an explicit "did not run" rather than leaving
// it absent.
func (s *signalReports) list() []SignalReport {
	order := []string{SignalFullText, SignalVector, SignalFacets, SignalGraph}
	out := make([]SignalReport, 0, len(order))
	for _, name := range order {
		if r, ok := s.byName[name]; ok {
			out = append(out, r)
			continue
		}
		out = append(out, SignalReport{Signal: name, Ran: false, Skipped: skippedUnreported})
	}
	return out
}

// skippedUnreported is the reason given for a signal no step accounted for.
// It is a bug indicator, not a supported state: every signal in the pipeline
// is either issued or explicitly skipped.
const skippedUnreported = "unreported"
