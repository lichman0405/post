package schemareg

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"sync"
)

// CanonicalNamespace is the $id URI prefix of the official POST schemas. The
// namespace is reserved: only schemas shipped with the code may live under
// it, so runtime registrations can never squat on an official schema id.
const CanonicalNamespace = "https://open-rd.example/schemas/"

// CanonicalV1 is the registry version of the canonical V1 schema set. The
// embedded schemas are fixed at compile time, so their content is immutable
// by construction (docs/21 §8).
const CanonicalV1 = "1"

// Ref addresses one registered schema version. It is the machine shape of
// the schema_ref object persisted on scientific_object_versions and pinned by
// release manifests.
type Ref struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

func (r Ref) String() string {
	return fmt.Sprintf("%s v%s", r.ID, r.Version)
}

// Sentinel errors for the registry's explicit failure modes. errors.Is
// matches both the sentinel and its wrapped causes.
var (
	// ErrReservedID reports a Register call whose id lies under
	// CanonicalNamespace. Official schema ids are fixed at build time.
	ErrReservedID = errors.New("schemareg: schema id is reserved for official schemas")
	// ErrAlreadyRegistered reports a Register call whose id+version is
	// already registered with different content. Old versions are never
	// overwritten; register the new content under a new version.
	ErrAlreadyRegistered = errors.New("schemareg: schema id+version is already registered with different content")
	// ErrUnknownSchema reports a lookup for an id with no registered
	// versions at all.
	ErrUnknownSchema = errors.New("schemareg: schema is not registered")
	// ErrUnknownVersion reports a lookup for a known id with an unknown
	// version.
	ErrUnknownVersion = errors.New("schemareg: schema version is not registered")
)

// Registry maps (schema id, version) pairs to their compiled, immutable
// schema content. It is safe for concurrent use: validation reads may run
// while extensions register.
type Registry struct {
	mu      sync.RWMutex
	entries map[Ref]*Schema
}

// New loads and registers every embedded canonical V1 schema
// (schemas/*.json) under version "1", keyed by each document's $id. It fails
// if any canonical schema is unreadable, lacks an $id inside
// CanonicalNamespace, or does not compile as a JSON Schema — a broken
// catalog is caught at startup, not at first validation.
func New() (*Registry, error) {
	r := &Registry{entries: make(map[Ref]*Schema)}
	entries, err := fs.ReadDir(schemasFS, schemasDir)
	if err != nil {
		return nil, fmt.Errorf("schemareg: read embedded schema dir: %w", err)
	}
	// First pass: read and identify every document so cross-references
	// between canonical schemas resolve regardless of file order.
	docs := make(map[string][]byte, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := schemasDir + "/" + e.Name()
		doc, err := fs.ReadFile(schemasFS, path)
		if err != nil {
			return nil, fmt.Errorf("schemareg: read %s: %w", path, err)
		}
		var probe any
		if err := json.Unmarshal(doc, &probe); err != nil {
			return nil, fmt.Errorf("schemareg: %s: not valid JSON: %w", path, err)
		}
		id := docID(doc)
		if id == "" {
			return nil, fmt.Errorf("schemareg: %s: canonical schemas must declare an $id", path)
		}
		if !inCanonicalNamespace(id) {
			return nil, fmt.Errorf("schemareg: %s: $id %q lies outside the canonical namespace %q", path, id, CanonicalNamespace)
		}
		if _, dup := docs[id]; dup {
			return nil, fmt.Errorf("schemareg: duplicate canonical $id %q", id)
		}
		docs[id] = doc
	}
	// Second pass: register, with every other document visible as a $ref
	// target.
	for id, doc := range docs {
		if _, err := r.register(Ref{ID: id, Version: CanonicalV1}, doc, docs); err != nil {
			return nil, fmt.Errorf("schemareg: %s: %w", id, err)
		}
	}
	return r, nil
}

// Register compiles doc and registers it under id+version — the namespaced
// extension path for schemas that are not part of the canonical set (e.g.
// project schema profiles, T0213). Rules:
//
//   - id must be namespaced ("<namespace>:<name>"; URLs qualify) so
//     extensions cannot collide with each other or with official ids;
//   - id must not lie under CanonicalNamespace (case-insensitively: URI
//     hosts are case-insensitive): official ids ship with the code and
//     cannot be registered at runtime;
//   - doc must be valid JSON and a valid JSON Schema; a $id inside doc, when
//     present, must equal id;
//   - an id+version already registered with identical content is a no-op;
//     different content is rejected with ErrAlreadyRegistered — old versions
//     are never overwritten (docs/21 §8).
//
// $refs in doc resolve against the registered catalog locally (never over
// the network): a $ref to the id being registered resolves to this
// registration, a $ref to any other id resolves to that id's lowest
// registered version (deterministic — the oldest version stays the
// resolution target as new versions appear).
func (r *Registry) Register(id, version string, doc []byte) (*Schema, error) {
	if id == "" || version == "" {
		return nil, errors.New("schemareg: Register: id and version must be non-empty")
	}
	if !strings.Contains(id, ":") {
		return nil, fmt.Errorf("schemareg: Register: id %q is not namespaced; extension schema ids must use the %q form", id, "<namespace>:<name>")
	}
	if inCanonicalNamespace(id) {
		return nil, fmt.Errorf("%w: %q: official schema ids are fixed at build time and cannot be registered at runtime", ErrReservedID, id)
	}
	return r.register(Ref{ID: id, Version: version}, doc, r.snapshot(id))
}

// register compiles doc and stores it under ref, enforcing the immutability
// rule. Compilation happens before the lock; the check-and-set is atomic.
func (r *Registry) register(ref Ref, doc []byte, resources map[string][]byte) (*Schema, error) {
	s, err := compileSchema(ref, doc, resources)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.entries[ref]; ok {
		if existing.ContentHash == s.ContentHash {
			return existing, nil // identical re-registration is a no-op
		}
		return nil, fmt.Errorf("%w: %s: registered content differs; schema versions are immutable — register the new content under a new version (docs/21 §8)", ErrAlreadyRegistered, ref)
	}
	r.entries[ref] = s
	return s, nil
}

// snapshot copies the registered schema documents keyed by id, excluding
// every version of excludeID. It is the $ref resolution context for a new
// registration. When several versions of a target id are registered, the
// lowest version's document wins deterministically (JSON Schema refs cannot
// address registry versions, and the oldest version is the stable target
// per docs/21 §8).
func (r *Registry) snapshot(excludeID string) map[string][]byte {
	r.mu.RLock()
	defer r.mu.RUnlock()
	byID := make(map[string][]Ref, len(r.entries))
	for ref := range r.entries {
		if ref.ID != excludeID {
			byID[ref.ID] = append(byID[ref.ID], ref)
		}
	}
	resources := make(map[string][]byte, len(byID))
	for id, refs := range byID {
		sort.Slice(refs, func(i, j int) bool { return refs[i].Version < refs[j].Version })
		resources[id] = r.entries[refs[0]].raw
	}
	return resources
}

// inCanonicalNamespace reports whether id lies under the reserved canonical
// namespace. URI hosts are case-insensitive, so the comparison folds case.
func inCanonicalNamespace(id string) bool {
	return strings.HasPrefix(strings.ToLower(id), strings.ToLower(CanonicalNamespace))
}

// Get returns the schema registered at ref, or (nil, false) when nothing is
// registered there. Use Lookup when the reason for a miss matters.
func (r *Registry) Get(ref Ref) (*Schema, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.entries[ref]
	return s, ok
}

// Lookup resolves ref and reports exactly why a miss failed: ErrUnknownSchema
// when the id has no registered versions at all, ErrUnknownVersion when the
// id exists but the version does not (the error lists the registered
// versions). Unknown schemas always fail here — there is no silent fallback.
func (r *Registry) Lookup(ref Ref) (*Schema, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if s, ok := r.entries[ref]; ok {
		return s, nil
	}
	versions := r.versionsLocked(ref.ID)
	if len(versions) == 0 {
		return nil, fmt.Errorf("%w: schema %q is unknown; register a namespaced extension via Register before validating against it", ErrUnknownSchema, ref.ID)
	}
	return nil, fmt.Errorf("%w: schema %q has no version %q; registered versions: %s", ErrUnknownVersion, ref.ID, ref.Version, strings.Join(versions, ", "))
}

// List returns every registered schema ref, sorted by id then version.
func (r *Registry) List() []Ref {
	r.mu.RLock()
	defer r.mu.RUnlock()
	refs := make([]Ref, 0, len(r.entries))
	for ref := range r.entries {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].ID != refs[j].ID {
			return refs[i].ID < refs[j].ID
		}
		return refs[i].Version < refs[j].Version
	})
	return refs
}

// Versions returns the versions registered under id, sorted ascending, or
// nil when the id is unknown.
func (r *Registry) Versions(id string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.versionsLocked(id)
}

// versionsLocked collects the registered versions of id. Callers hold at
// least the read lock.
func (r *Registry) versionsLocked(id string) []string {
	var versions []string
	for ref := range r.entries {
		if ref.ID == id {
			versions = append(versions, ref.Version)
		}
	}
	sort.Strings(versions)
	return versions
}
