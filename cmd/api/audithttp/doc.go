// Package audithttp is the Activity page HTTP surface (T0110): read-only
// audit queries per project and per organization. There are deliberately
// no write routes here — the log is append-only, written by the stores
// inside their own transactions and by the auth service; no endpoint can
// update or delete an audit row, and none is registered.
package audithttp
