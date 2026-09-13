// Package audit owns the Activity page use cases (T0110): reading the
// append-only audit log per project / per organization. Authorization is
// delegated, never re-implemented: the project and organization read gates
// are the same services the project/org surfaces use, so a non-member gets
// the same existence-hiding 404 everywhere. Writing the log is the
// persistence layer's job (transactional appendAudit) and the authn
// service's (best-effort auth events); this package exposes no write path.
package audit
