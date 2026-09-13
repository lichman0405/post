# @post/schemas

Canonical JSON Schema copies for POST domain objects.

- **Source of truth:** `specs/schemas/*.schema.json` (repository root).
- **This copy:** `schemas/*.json` — synced artefacts, never hand-edited
  (docs/65: "generated/synced artefacts must not be hand-forked").
- **Sync:** `make sync-schemas` (or `pnpm --filter @post/schemas run sync`).
  The same command keeps `internal/rsg/schemareg/schemas/` (the copy embedded
  into the Go schema registry) in sync too.
- **Drift check:** `make check-schema-drift` fails when either copy diverges
  from `specs/schemas/`. The root `make check` runs it.

Consumers import the copied files (this package is published as plain JSON
assets, not compiled code); the Go backend validates documents against the
embedded copies via `internal/rsg/schemareg`.
