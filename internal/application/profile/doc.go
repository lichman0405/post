// Package profile orchestrates the research-profile use cases (T0102):
// reading the public profile and updating the fields the owner may edit.
// Policy lives here (docs/52: application is the only layer that may
// mutate canonical state): which fields are editable and who may change
// them. The HTTP surface in cmd/api/profilehttp only translates requests
// into Service calls; adapters live in internal/persistence.
package profile
