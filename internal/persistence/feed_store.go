package persistence

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/lichman0405/post/internal/application/feeds"
	"github.com/lichman0405/post/internal/persistence/sqlc"
	"github.com/lichman0405/post/internal/rights"
)

// FeedStore answers the public feed reads (T1004), with the canonical
// queries of internal/persistence/queries/feeds.sql (generated code under
// internal/persistence/sqlc).
//
// It answers ONE question — "what does this target look like, row by row?" —
// and the rows it answers with are the ones a public feed may RENDER: the
// canonical queries filter private asset versions out and apply the two
// audience predicates SQL can spell exactly to the knowledge halves, because
// the lists are bounded and a window read raw is a resource a writer can
// exhaust (see the header of the SQL file). That filter is a read strategy;
// the RULE that decides what may be rendered still belongs to
// internal/application/feeds (BuildFeed), which applies it to every entry
// whatever the reader returned — including the one axis the queries
// deliberately do NOT decide (a publication's rights declaration, which is
// read raw and judged in Go).
//
// Every method is a READ. Nothing in this file writes a row or takes a lock.
type FeedStore struct {
	queries *sqlc.Queries
}

// NewFeedStore wires the reader over any sqlc executor — the production
// value is the pgx pool cmd/api already builds.
func NewFeedStore(db sqlc.DBTX) *FeedStore {
	return &FeedStore{queries: sqlc.New(db)}
}

// LoadFeedState resolves one target's whole state: the entity, the
// visibility of the project that owns it, and the entry rows.
//
// A target that names no stored row is Found=false with a nil error — a
// state, not a failure (the transport answers it the same 404 a non-public
// target gets). Anything else that went wrong is an error, because "this
// project has published nothing" and "the database did not answer" are
// different statements and a feed must not report the first when it means
// the second.
//
// The identity read runs first and the entry read is skipped when the
// target does not exist: the entries of a nonexistent project are not a
// question worth asking, and answering it would be a second round trip for
// every mistyped id an anonymous client sends.
func (s *FeedStore) LoadFeedState(ctx context.Context, target feeds.Target, limit int) (feeds.State, error) {
	switch target.Kind {
	case feeds.KindAsset:
		return s.loadAssetState(ctx, target, limit)
	case feeds.KindKnowledge:
		return s.loadKnowledgeState(ctx, target, limit)
	case feeds.KindProject:
		return s.loadProjectState(ctx, target, limit)
	default:
		// Unreachable through the service (ParseTarget refuses unknown
		// kinds); refused here too rather than defaulted, so a future caller
		// that skips the service cannot get the project read by accident.
		return feeds.State{}, fmt.Errorf("%w: unknown feed kind %q", feeds.ErrValidation, target.Kind)
	}
}

func (s *FeedStore) loadProjectState(ctx context.Context, target feeds.Target, limit int) (feeds.State, error) {
	projectID, err := textUUID(target.ID)
	if err != nil {
		return feeds.State{}, fmt.Errorf("%w: project id: %v", feeds.ErrValidation, err)
	}
	row, err := s.queries.GetFeedProject(ctx, projectID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return feeds.State{Found: false}, nil
		}
		return feeds.State{}, fmt.Errorf("persistence: read feed project: %w", err)
	}
	entries, err := s.queries.ListFeedProjectEntries(ctx, sqlc.ListFeedProjectEntriesParams{
		ProjectID: projectID,
		RowLimit:  int32(limit),
	})
	if err != nil {
		return feeds.State{}, fmt.Errorf("persistence: list feed project entries: %w", err)
	}
	state := feeds.State{
		Found:             true,
		Title:             row.Name,
		Subtitle:          row.Purpose,
		ProjectVisibility: row.Visibility,
		CreatedAt:         row.CreatedAt.Time,
		Entries:           make([]feeds.EntryState, 0, len(entries)),
	}
	for _, e := range entries {
		kind, err := entryKindOf(e.EntryKind)
		if err != nil {
			return feeds.State{}, err
		}
		doc, valid := entryRights(e.RightsJson)
		state.Entries = append(state.Entries, feeds.EntryState{
			Kind:               kind,
			ID:                 e.EntryID,
			Version:            e.Version,
			AssetPID:           e.AssetPid,
			SubjectType:        e.SubjectType,
			Title:              e.Title,
			Visibility:         e.Visibility,
			VisibilityPolicyID: uuidTextPtr(e.VisibilityPolicyID),
			Rights:             doc,
			RightsValid:        valid,
			PublishedAt:        e.PublishedAt.Time,
			Publisher:          e.Publisher,
		})
	}
	return state, nil
}

func (s *FeedStore) loadAssetState(ctx context.Context, target feeds.Target, limit int) (feeds.State, error) {
	row, err := s.queries.GetFeedAsset(ctx, target.ID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return feeds.State{Found: false}, nil
		}
		return feeds.State{}, fmt.Errorf("persistence: read feed asset: %w", err)
	}
	entries, err := s.queries.ListFeedAssetEntries(ctx, sqlc.ListFeedAssetEntriesParams{
		Pid:      target.ID,
		RowLimit: int32(limit),
	})
	if err != nil {
		return feeds.State{}, fmt.Errorf("persistence: list feed asset entries: %w", err)
	}
	state := feeds.State{
		Found:             true,
		Title:             row.Title,
		ProjectVisibility: row.ProjectVisibility,
		CreatedAt:         row.CreatedAt.Time,
		Entries:           make([]feeds.EntryState, 0, len(entries)),
	}
	for _, e := range entries {
		state.Entries = append(state.Entries, feeds.EntryState{
			Kind:        feeds.EntryAssetVersion,
			ID:          e.EntryID,
			Version:     e.Version,
			AssetPID:    e.AssetPid,
			SubjectType: e.SubjectType,
			Title:       e.Title,
			Visibility:  e.Visibility,
			PublishedAt: e.PublishedAt.Time,
			Publisher:   e.Publisher,
		})
	}
	return state, nil
}

func (s *FeedStore) loadKnowledgeState(ctx context.Context, target feeds.Target, limit int) (feeds.State, error) {
	objectID, err := textUUID(target.ID)
	if err != nil {
		return feeds.State{}, fmt.Errorf("%w: knowledge object id: %v", feeds.ErrValidation, err)
	}
	row, err := s.queries.GetFeedKnowledgeObject(ctx, objectID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return feeds.State{Found: false}, nil
		}
		return feeds.State{}, fmt.Errorf("persistence: read feed knowledge object: %w", err)
	}
	entries, err := s.queries.ListFeedKnowledgeEntries(ctx, sqlc.ListFeedKnowledgeEntriesParams{
		ObjectID: objectID,
		RowLimit: int32(limit),
	})
	if err != nil {
		return feeds.State{}, fmt.Errorf("persistence: list feed knowledge entries: %w", err)
	}
	state := feeds.State{
		Found: true,
		// No title: a knowledge object has no name of its own, and the title
		// a public feed may carry is the newest publication it RENDERS —
		// which rows those are is the model's decision, not this read's
		// (feeds.BuildFeed names a knowledge feed). Reading "the newest
		// publication's title" here would answer a second, weaker question.
		ProjectVisibility: row.ProjectVisibility,
		CreatedAt:         row.CreatedAt.Time,
		Entries:           make([]feeds.EntryState, 0, len(entries)),
	}
	for _, e := range entries {
		doc, valid := entryRights(e.RightsJson)
		state.Entries = append(state.Entries, feeds.EntryState{
			Kind:               feeds.EntryKnowledgePublication,
			ID:                 e.EntryID,
			Version:            e.Version,
			SubjectType:        e.SubjectType,
			Title:              e.Title,
			Visibility:         e.Visibility,
			VisibilityPolicyID: uuidTextPtr(e.VisibilityPolicyID),
			Rights:             doc,
			RightsValid:        valid,
			PublishedAt:        e.PublishedAt.Time,
			Publisher:          e.Publisher,
		})
	}
	return state, nil
}

// entryRights parses a row's stored rights declaration and reports whether
// this build could read it.
//
// The queries hand the document back RAW for exactly this: the audience rule
// is "the metadata token is the rights model's own project_policy token, and
// a document that does not parse states no token", and that is a Go parse
// (internal/rights refuses unknown fields, so the same bytes can mean
// different things to a different build), not a JSON predicate. An
// unreadable declaration comes back as the zero document with valid=false,
// and feeds.rowRenderable refuses it — fail-closed, never "assume public".
//
// A NULL column (every asset row; see the queries' union) is unreadable the
// same way: an asset version has no declaration of its own, and the model
// does not read one for it.
func entryRights(raw []byte) (rights.Document, bool) {
	doc, err := rights.Parse(raw)
	if err != nil {
		return rights.Document{}, false
	}
	return doc, true
}

// entryKindOf maps the union query's kind discriminator to the entry kind.
// The column is a literal written by the query itself, so an unknown value
// means the query grew a half this reader does not model — refused rather
// than defaulted, because the default would render some other entity's rows
// as asset versions and hand them an asset URL.
func entryKindOf(kind string) (feeds.EntryKind, error) {
	switch feeds.EntryKind(kind) {
	case feeds.EntryAssetVersion, feeds.EntryKnowledgePublication:
		return feeds.EntryKind(kind), nil
	default:
		return "", fmt.Errorf("persistence: unknown feed entry kind %q", kind)
	}
}
