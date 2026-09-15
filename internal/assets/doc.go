// Package assets owns the research asset domain core (T0701): the
// persistent identity every published research asset carries (its PID),
// the immutable published versions, the closed V1 type set, the origin
// refs that pin a version's provenance, and the persistent URL scheme.
//
// Identity: an asset's public identity is its pid (research_assets.pid,
// migration 00064) — a random 26-character Crockford base32 token,
// never derived from the slug, the owning organization, or any other
// mutable metadata. Renaming the slug or transferring the asset between
// organizations changes nothing about the pid, and therefore nothing
// about the URLs built from it (docs/11 §2; acceptance: asset ID 不随
// slug/org 变化). The internal row id remains a uuid for foreign keys;
// the pid is the identifier the network sees.
//
// Versions: published versions are immutable (migration 00014 guards the
// rows; CLAUDE.md invariant 5) and identified by the pair (asset pid,
// version label) — the label is per-asset unique (UNIQUE(asset_id,
// version)) and its shape is ValidVersionLabel.
//
// Types: the V1 asset type set is closed — dataset, protocol,
// material_collection, benchmark (docs/11 §2; the database CHECK and
// specs/schemas/research-asset-version.schema.json carry the same four
// values).
//
// Origin: a version's origin_refs (research_asset_versions.origin_refs,
// migration 00064) are canonical kind:value provenance pins — which
// project, release, state or object versions this published version came
// from. The column is NOT NULL and non-empty at the storage layer, the
// same minimum the version schema declares (minItems 1).
//
// URLs: the persistent URL scheme is /assets/{pid} for the asset page
// and /assets/{pid}/{version} for one immutable published version
// (AssetURL / AssetVersionURL). Nothing mutable ever appears in them.
//
// The publish use case itself is T0705; this package carries the
// identity machinery it will build on. Blob storage sits behind this
// package as a port (docs/17); asset metadata may be revised
// independently of the scientific versions (docs/11 §4).
package assets
