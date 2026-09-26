# Seed demo project (synthetic)

`demo-plan.json` is the declaration of the synthetic research network this
repository builds. Its source campaign is the project `docs/34_SEED_DEMO_PROJECT.md`
describes (slug `demo-mof-humidity-separation`), with an independent replication
project, a computational transferability project, and the external fork.

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
| `collaboration_demo` | six additional logins, two universities/labs, and two independently owned projects |
| `main_objects`, `main_conclusions` | the objects main starts with (research question, hypotheses, materials, protocol, …) |
| `main_relations`, `conclusion_relations`, `main_evidence` | the edges and assertions between them |
| `branches` | the four branches, their visibility and purpose |
| `branches_content` | each branch's own objects, relations and evidence |
| `protocol_versions`, `hypothesis_revisions` | the versions that make a branch's proposal a scientific one |
| `pull_requests` | the three proposal kinds (conflict-free merge, protocol conflict, selective publication) |
| `publication`, `releases`, `assets` | publishing, R0.1/R1.0, all four V1 asset types, multiple dataset releases and examples with exact dependency pins |
| `external_contribution` | the second user/org, the fork, and the contribution made from it |
| `marker` | the marker every payload carries (below) |

Every object declares a `key` that is unique across the whole plan. The key is
not a database id — it is the plan's name for the item, and the builder resolves
it to whatever id the product returned.

## Demo accounts and collaboration projects

The full seed creates eight separate accounts using the regular signup and
login routes. After seeding, open `/demo` in the web app to see the role list,
passwords, and login links. The addresses use `example.invalid`; these are
synthetic demo accounts, not real people. Running with `--external 0` omits the
external contributor and fork, so that reduced build has seven accounts.

The two extra projects are authored through their owners' sessions. The
Westhaven University project records an independent MOF-X replication, its
protocol, measurements, and a result that differs from the source campaign.
The Northbridge project records a computational MOF-Y transferability study.
Organization affiliations are synthetic and unverified; the projects and
scientific claims are marked synthetic as well.

The adsorption chart reads the CSV fixtures in
`apps/web/public/demo-data/`. The seed builder reads those same files through
the plan's `content_path` entries and stores their hashes as dataset blobs.
Each plotted series is the mean of three replicate values with sample-standard-
deviation error bars. All values are fabricated for the interface and are not
experimental evidence.

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

## Other demo files

The CSV fixtures are shared by the UI and seed builder at
`apps/web/public/demo-data/`. The plan is the input; generated records live in
PostgreSQL (and in Gitea repositories the product provisions).
