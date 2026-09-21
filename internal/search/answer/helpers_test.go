package answer_test

import (
	"github.com/lichman0405/post/internal/search/ranking"
	"github.com/lichman0405/post/internal/search/retrieval"
)

// The identities the tests in this package rank and cite. They are the two
// shapes the projection writes: a pid behind an entity ref ("asset:AST-0001")
// and a uuid (an object version, a project state).
const (
	assetPID    = "AST-0001"
	assetRef    = "asset:AST-0001@2"
	knownPID    = "KNW-0007"
	knownRef    = "knowledge:KNW-0007@1"
	stateUUID   = "6b1f9c2e-3a4d-4e5f-8a9b-0c1d2e3f4a5b"
	stateRef    = "state:6b1f9c2e-3a4d-4e5f-8a9b-0c1d2e3f4a5b"
	objectUUID  = "1d2c3b4a-5e6f-4071-8293-a4b5c6d7e8f9"
	versionID   = "9f8e7d6c-5b4a-4392-8170-6f5e4d3c2b1a"
	projectID   = "3f2a5b7c-1d4e-4a6b-8c9d-0e1f2a3b4c5d"
	releaseRef  = "release:" + releaseUUID
	releaseUUID = "7a6b5c4d-3e2f-4109-8b7a-6c5d4e3f2a1b"
)

// assessment builds one factor outcome. Tests spell factors out rather than
// going through the ranking so that a change to the ranking's rules cannot
// silently rewrite what the answer layer is tested against.
func assessment(factor, level, reason string) ranking.Assessment {
	return ranking.Assessment{Factor: factor, Level: level, Reason: reason}
}

// cleanFactors is six factors that match NO limitation rule and no conflict
// rule: the state the "the platform derived no limitation" and "nothing is
// contested" sentences are for.
//
// Every level here is the benign one on its ladder — the newest live version,
// reviewed evidence, an approved state, an independent reproduction, nothing
// contradicting it, and a match on the question's own words.
func cleanFactors() []ranking.Assessment {
	return []ranking.Assessment{
		assessment(ranking.FactorQueryScopeMatch, ranking.LevelQuestion, "recalled by the question's own content (full_text)"),
		assessment(ranking.FactorEvidence, ranking.LevelReviewed, "2 evidence assertions target this version, 1 reviewed"),
		assessment(ranking.FactorReview, ranking.LevelApproved, "1 scientific review on the state this version was created in, 1 approved"),
		assessment(ranking.FactorReproduction, ranking.LevelIndependent, "1 reproduction recorded, 1 from another project"),
		assessment(ranking.FactorConflict, ranking.LevelUncontested, "nothing on the platform contradicts this version (no contradictory evidence assertion, no contradicts relation)"),
		assessment(ranking.FactorVersion, ranking.LevelCurrent, "version 2 is the newest version of its object and is active"),
	}
}

// factorsWith returns cleanFactors with one factor's level and reason
// replaced, so a test states the single fact it is about.
func factorsWith(factor, level, reason string) []ranking.Assessment {
	out := cleanFactors()
	for i := range out {
		if out[i].Factor == factor {
			out[i].Level = level
			out[i].Reason = reason
		}
	}
	return out
}

// rankedAsset builds the ranked candidate the tests cite: a published asset
// document, pinned at version 2.
func rankedAsset(ref, identity string, factors []ranking.Assessment) ranking.Ranked {
	return ranking.Ranked{
		Rank:       1,
		Ref:        ref,
		Kind:       retrieval.KindDocument,
		EntityType: "asset",
		ObjectType: "material",
		Title:      "Mg-MOF-74 CO2 uptake at 298 K",
		Version:    "2",
		Labels:     []string{ranking.LabelIndependentlyReproduced},
		Factors:    factors,
		Candidate: retrieval.Candidate{
			Ref:        ref,
			Kind:       retrieval.KindDocument,
			EntityType: "asset",
			Identity:   identity,
			Version:    "2",
			Title:      "Mg-MOF-74 CO2 uptake at 298 K",
			Visibility: "public",
			ProjectID:  projectID,
			ObjectType: "material",
			ObjectID:   objectUUID,
			Signals:    []retrieval.SignalHit{{Signal: retrieval.SignalFullText, Rank: 1, Score: 0.31}},
		},
	}
}

// rankedKnowledge builds a published knowledge document whose pid resolves to
// an object version — the case the ranking reads facts for.
func rankedKnowledge(rank int, ref, identity string, factors []ranking.Assessment) ranking.Ranked {
	return ranking.Ranked{
		Rank:            rank,
		Ref:             ref,
		Kind:            retrieval.KindDocument,
		EntityType:      "knowledge",
		ObjectType:      "claim",
		Title:           "The uptake is reversible over ten cycles",
		Version:         "1",
		ObjectVersionID: versionID,
		Labels:          nil,
		Factors:         factors,
		Candidate: retrieval.Candidate{
			Ref:             ref,
			Kind:            retrieval.KindDocument,
			EntityType:      "knowledge",
			Identity:        identity,
			Version:         "1",
			Title:           "The uptake is reversible over ten cycles",
			Visibility:      "public",
			ProjectID:       projectID,
			ObjectType:      "claim",
			ObjectID:        objectUUID,
			ObjectVersionID: versionID,
			VersionNo:       1,
			Signals:         []retrieval.SignalHit{{Signal: retrieval.SignalFullText, Rank: 2, Score: 0.22}},
		},
	}
}

// rankedRelease builds a release document: an entity the platform addresses
// under its project and nowhere else (locator.go).
func rankedRelease(rank int) ranking.Ranked {
	return ranking.Ranked{
		Rank:       rank,
		Ref:        releaseRef,
		Kind:       retrieval.KindDocument,
		EntityType: "release",
		Title:      "Release 1: accepted baseline",
		Version:    "1",
		Factors:    cleanFactors(),
		Candidate: retrieval.Candidate{
			Ref:        releaseRef,
			Kind:       retrieval.KindDocument,
			EntityType: "release",
			Identity:   releaseUUID,
			Version:    "1",
			Title:      "Release 1: accepted baseline",
			Visibility: "public",
			ProjectID:  projectID,
			Signals:    []retrieval.SignalHit{{Signal: retrieval.SignalFullText, Rank: rank, Score: 0.11}},
		},
	}
}

// rankedState builds a project-state candidate: an entity that pins no
// version and has no address (locator.go).
func rankedState(rank int) ranking.Ranked {
	return ranking.Ranked{
		Rank:       rank,
		Ref:        stateRef,
		Kind:       retrieval.KindDocument,
		EntityType: "state",
		Title:      "Accepted the CO2 uptake baseline",
		Factors: factorsWith(ranking.FactorVersion, ranking.LevelUnversioned,
			"the candidate does not resolve to a scientific object version, so no freshness is recorded"),
		Candidate: retrieval.Candidate{
			Ref:        stateRef,
			Kind:       retrieval.KindDocument,
			EntityType: "state",
			Identity:   stateUUID,
			Title:      "Accepted the CO2 uptake baseline",
			Visibility: "public",
			ProjectID:  projectID,
		},
	}
}

// rankedObjectVersion builds a version reached by traversal: a real entity
// with no route of its own (locator.go).
func rankedObjectVersion(rank int) ranking.Ranked {
	ref := "object_version:" + objectUUID + "@1"
	return ranking.Ranked{
		Rank:            rank,
		Ref:             ref,
		Kind:            retrieval.KindObjectVersion,
		ObjectType:      "finding",
		Title:           "Uptake falls by 4% after ten cycles",
		Version:         "1",
		ObjectVersionID: versionID,
		Factors:         cleanFactors(),
		Candidate: retrieval.Candidate{
			Ref:             ref,
			Kind:            retrieval.KindObjectVersion,
			Identity:        objectUUID,
			Version:         "1",
			Title:           "Uptake falls by 4% after ten cycles",
			ProjectID:       projectID,
			ObjectType:      "finding",
			ObjectID:        objectUUID,
			ObjectVersionID: versionID,
			VersionNo:       1,
			Hops:            []retrieval.Hop{{RelationType: "derived_from", Direction: retrieval.DirectionOut, FromRef: assetRef, Depth: 1}},
		},
	}
}
