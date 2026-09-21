package dependencyimpact

import "errors"

var (
	// ErrValidation marks a malformed request to this package: a subject
	// with no id, a hop count that names no entity, a batch limit of zero.
	// It never means "the data was odd" — data outcomes have their own
	// names below.
	ErrValidation = errors.New("dependencyimpact: invalid input")
	// ErrStore marks a failure to read or write through the store port.
	// It is a 503, never a 404: a database that could not be reached must
	// not be reported as an entity that does not exist.
	ErrStore = errors.New("dependencyimpact: store failure")
	// ErrSubjectNotFound is the existence-hiding answer to a read of a
	// subject the caller may not read (docs/45: a denied read looks
	// exactly like an unknown one). It is returned for a subject that does
	// not exist AND for one whose project the reader's gate refuses,
	// deliberately: distinguishing them would tell the caller that
	// something is there.
	ErrSubjectNotFound = errors.New("dependencyimpact: subject not found")
)
