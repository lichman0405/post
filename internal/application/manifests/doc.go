// Package manifests is the manifest export use case (T0206): the read that
// renders one RSG state as its canonical open manifest — the complete
// versioned research-state graph as of that state, with the state hash
// content-addressing it (internal/rsg/manifest, docs/07 §4).
//
// Export is a system-facing read, not an API command: there is no HTTP
// route in this task and the service performs no authorization of its own.
// The consumers that authorize are T0309 (git ↔ RSG reconciliation),
// T0605 (release manifest building) and any future transport, each of
// which resolves visibility before calling it. The read surface it needs —
// the state row and the lineage snapshot — is the same visibility-unaware
// surface those system flows read through anyway.
//
// The export reads nothing mutable: a state's hash is a pure function of
// the state's own recorded content (the state row plus its lineage's
// append-only member rows), so the service has no project read and Build
// takes no project input (internal/rsg/manifest's purity invariant).
package manifests
