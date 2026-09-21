# Seed demo project (synthetic)

`demo-plan.json` is the declaration of the demo project this repository builds
from an empty database — the project `docs/34_SEED_DEMO_PROJECT.md` describes:
MOF-X/MOF-Y humidity separation, slug `demo-mof-humidity-separation`.

**Everything in it is fabricated.** The materials, the isotherms, the DFT
numbers, the citations, the people: written for the demo, verifiable against
nothing. No number here is a measurement, no reference here is to a real
paper's data, and the demo says so in its own payloads.

## Build it

```bash
ops/seed-demo.sh --reset            # empty database → the whole demo
ops/seed-demo.sh                    # again, onto what the first run left
ops/seed-demo.sh --external 0       # without the fork-dependent part
```

`ops/seed-demo.sh` starts the product API against the target database, drives
the plan through it, then measures the result by querying PostgreSQL. The last
line of its stdout is a machine-readable JSON summary; **`ops/seed-demo.md`** is
the operator's document (stages, exit codes, path labels, known limitations).

## The plan file

| key | what it holds |
| --- | --- |
| `project`, `users`, `organizations` | the demo's identity: slug, owner, and the external contributor's account and org |
| `main_objects`, `main_conclusions` | the objects main starts with (research question, hypotheses, materials, protocol, …) |
| `main_relations`, `conclusion_relations`, `main_evidence` | the edges and assertions between them |
| `branches` | the four branches, their visibility and purpose |
| `branches_content` | each branch's own objects, relations and evidence |
| `protocol_versions`, `hypothesis_revisions` | the versions that make a branch's proposal a scientific one |
| `pull_requests` | the three proposal kinds (conflict-free merge, protocol conflict, selective publication) |
| `publication`, `releases`, `assets` | publishing, R0.1/R1.0, the four asset types |
| `external_contribution` | the second user/org, the fork, and the contribution made from it |
| `marker` | the marker every payload carries (below) |

Every object declares a `key` that is unique across the whole plan. The key is
not a database id — it is the plan's name for the item, and the builder resolves
it to whatever id the product returned.

## The synthetic marker

Every seeded object's payload carries the marker twice: the tag `synthetic` in
`payload.tags`, and its plan key in `payload.metadata.seed_key`. One query
selects the whole seed:

```sql
select count(*)
from scientific_objects o
join scientific_object_versions v
  on v.object_id = o.id and v.version_no = o.current_version_no
where o.project_id = (select id from projects where slug = 'demo-mof-humidity-separation')
  and v.payload -> 'tags' ? 'synthetic'
  and v.payload -> 'metadata' ->> 'seed_key' is not null;
```

The verifier runs exactly this query and fails if the count is short of the
project's object count — so a demo object that lost its marker is a red, not a
silent exception. The marker is also the builder's re-run key: a second run
finds its objects by `seed_key` (`tests/acceptance/seeddemo/discover.go`)
instead of creating a second copy of the demo.

## This directory's other files

Nothing else. The plan is the input; everything the demo produces lives in
PostgreSQL (and in the Gitea repositories the product provisions), not here.
