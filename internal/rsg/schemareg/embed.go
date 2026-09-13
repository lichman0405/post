package schemareg

import "embed"

// schemasFS embeds the synced copy of specs/schemas/. The canonical source of
// truth is specs/schemas/ (docs/65); this copy is kept in sync by
// packages/schemas/scripts/sync-schemas.sh and drift-checked by
// packages/schemas/scripts/check-schema-drift.sh. Never hand-edit the
// embedded copies — edit specs/schemas/ and re-run make sync-schemas.
//
//go:embed schemas/*.json
var schemasFS embed.FS

// schemasDir is the embed root holding the schema documents.
const schemasDir = "schemas"
