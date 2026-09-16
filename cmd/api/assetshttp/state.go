package assetshttp

import (
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/sqlc"
)

// PostgresStateStore is the production StateReader: the canonical resolver
// of the things a publish candidate names (internal/persistence.
// AssetStateStore), reached through the transport's own name for it.
//
// # Where the adapter lives, and why it moved (T0705)
//
// T0704 put this adapter in the transport, because internal/persistence was
// outside that task's scope and this package was its only caller — the same
// choice cmd/api/provenancehttp made for its projection store (T0505). The
// publish command then needed the SAME resolution inside its write
// transaction (docs/22 §7: a command re-runs its gate server-side, never
// trusting a precheck), and internal/persistence is where a pgx.Tx-scoped
// read adapter belongs. The definition therefore moved to
// internal/persistence.AssetStateStore and this type is the alias the
// preview route names it by: one resolver, two callers, no second copy of
// "does this pin resolve" to drift from the first. The SQL never moved — it
// was always canonical, under internal/persistence/queries/asset_preview.sql.
//
// The alias is deliberate rather than a re-exported constructor result: a
// caller that wants the preview's reader and a caller that wants the
// publish's reader are asking for the same object, and two names for one
// type is cheaper than two types for one answer.
type PostgresStateStore = persistence.AssetStateStore

// NewPostgresStateStore wires the resolver over any sqlc executor (the
// production value is the pgx pool cmd/api already builds). Returning the
// interface the surface declares keeps the constructor honest about what
// its callers may do with it: resolve a state, nothing else.
func NewPostgresStateStore(db sqlc.DBTX) StateReader {
	return persistence.NewAssetStateStore(db)
}
