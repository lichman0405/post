# @post/domain-fixtures

Shared, versioned test fixtures for POST domain objects (projects, RSG
objects, evidence assertions, relations, …).

- **Conventions:** fixtures are keyed by domain object type and must validate
  against the canonical schema in `specs/schemas/` (see `@post/schemas`).
  The `examples/` seed documents in the repo root are the reference style.
- **Status:** T0002 scaffolds this package only; the fixture corpus arrives
  with the domain and test tasks (docs/24_TEST_STRATEGY.md, docs/67).
