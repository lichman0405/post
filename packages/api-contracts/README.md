# @post/api-contracts

OpenAPI contract and the generated TypeScript client for the POST API.

- **Canonical source:** `specs/api/openapi.yaml` (OpenAPI 3.1 contract seed).
- **Generated client:** lands here via the generation task once the API
  surface stabilizes (T0002 scaffolds the package only). Generated code is a
  build artefact and must never be hand-edited (docs/65); frontend code
  consumes it instead of hand-writing a second DTO set (docs/50).
- **Go side:** the Go API implements the same contract — OpenAPI-first
  (docs/52). Contract changes are cross-language decisions, not local edits.
