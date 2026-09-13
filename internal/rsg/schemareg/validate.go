package schemareg

import "fmt"

// ValidationError reports a document that violated its registered schema. It
// maps to the SCHEMA_VALIDATION_FAILED API error code (docs/45). Unwrap
// exposes the underlying jsonschema error, whose message lists each failing
// instance location.
type ValidationError struct {
	Ref
	err error
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("SCHEMA_VALIDATION_FAILED: %s: %v", e.Ref, e.err)
}

// Unwrap returns the underlying schema violation so callers can inspect the
// library error via errors.As.
func (e *ValidationError) Unwrap() error { return e.err }

// Validate resolves ref in the registry and validates doc against the
// registered schema. Failures are explicit:
//
//   - ErrUnknownSchema / ErrUnknownVersion when nothing is registered there
//     — unknown schemas never pass silently;
//   - a plain error when doc is not valid JSON;
//   - *ValidationError when the document violates the schema.
func (r *Registry) Validate(ref Ref, doc []byte) error {
	s, err := r.Lookup(ref)
	if err != nil {
		return err
	}
	return s.Validate(doc)
}
