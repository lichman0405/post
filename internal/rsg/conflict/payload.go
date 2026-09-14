package conflict

import (
	"bytes"
	"encoding/json"
	"sort"
)

// divergedPayloadKeys compares the base, source and target payloads at
// top-level key granularity and returns the keys that diverged: the key
// changed on both sides (base→source and base→target) AND the two results
// differ AND the change is not a mergeable append. Sorted by name.
//
// A key is converged when both sides wrote the same resulting value —
// including both sides removing it (converging on absence) — not a
// conflict. A key whose base value is an array that both sides only
// extended is a mergeable append when mergeAppends is set (append-only
// evidence, docs/09 §7: append-only evidence is in the auto-allowed set)
// — this is what keeps "append evidence" from being misjudged as a text
// conflict (task acceptance criterion). Protocol content never merges
// appends: the caller passes mergeAppends=false because no branch
// validated the combined steps (docs/09 §7: Protocol 冲突数值折中禁止自动).
// Everything else on an overlapping key is a divergence.
//
// Payloads that are not JSON objects compare as one unit under the empty
// key: there are no top-level keys to isolate.
func divergedPayloadKeys(base, source, target json.RawMessage, mergeAppends bool) []string {
	bm, sm, tm := payloadMap(base), payloadMap(source), payloadMap(target)
	keys := make([]string, 0, len(bm)+len(sm)+len(tm))
	seen := make(map[string]bool, len(bm)+len(sm)+len(tm))
	for _, m := range []map[string]json.RawMessage{bm, sm, tm} {
		for k := range m {
			if !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
	}
	sort.Strings(keys)

	diverged := []string{}
	for _, k := range keys {
		bv, bOK := bm[k]
		sv, sOK := sm[k]
		tv, tOK := tm[k]
		if !changed(bOK, bv, sOK, sv) || !changed(bOK, bv, tOK, tv) {
			continue // only one side moved the key: non-overlapping
		}
		if !sOK && !tOK {
			continue // both sides removed the key: converged on absence
		}
		if sOK && tOK && bytes.Equal(sv, tv) {
			continue // converged on the same resulting value
		}
		if mergeAppends && appendOnly(bOK, bv, sOK, sv) && appendOnly(bOK, bv, tOK, tv) {
			continue // both sides only extended the base list: mergeable
		}
		diverged = append(diverged, k)
	}
	return diverged
}

// changed reports whether the key's value differs between the base and
// the head (presence counts as a value: a key added or removed changed).
func changed(bOK bool, bv json.RawMessage, hOK bool, hv json.RawMessage) bool {
	if bOK != hOK {
		return true
	}
	return !bytes.Equal(bv, hv)
}

// appendOnly reports whether head is base extended at the tail: both are
// arrays, head is at least as long, and base is a prefix of head. A
// missing base key is not an append — there is no anchor list to extend.
// A JSON null is not an array either: null != [], so a null base is no
// anchor and a null head is no extension. Element comparison is bytewise,
// which is exact here: the diff engine canonicalized every payload before
// the detector sees it.
func appendOnly(bOK bool, base json.RawMessage, hOK bool, head json.RawMessage) bool {
	if !bOK || !hOK {
		return false
	}
	var b, h []json.RawMessage
	if json.Unmarshal(base, &b) != nil || json.Unmarshal(head, &h) != nil {
		return false
	}
	// json.Unmarshal reports no error for a JSON null and leaves the slice
	// nil, while [] decodes to an empty (non-nil) slice — the nil check is
	// exactly the null check, and it must come before the prefix walk:
	// walking a nil base would read "no anchor list" as "empty anchor" and
	// call any replacement an append.
	if b == nil || h == nil {
		return false
	}
	if len(h) < len(b) {
		return false
	}
	for i := range b {
		if !bytes.Equal(b[i], h[i]) {
			return false
		}
	}
	return true
}

// payloadMap splits a payload into its top-level key→raw-value map. A
// payload that is not a JSON object (null, a scalar, an array) is
// represented as the single key "" holding the whole payload — there are
// no top-level keys to isolate. The raw values carry the diff engine's
// canonical encoding, so byte comparison is exact.
func payloadMap(p json.RawMessage) map[string]json.RawMessage {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(p, &obj); err != nil || obj == nil {
		return map[string]json.RawMessage{"": p}
	}
	return obj
}
