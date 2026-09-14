// Package gittokenshttp is the HTTP surface of the scoped git-token
// feature (T0304): issue, list and revoke the credentials platform users
// use with real git clients against the internal Gitea transport
// (docs/16 §2 identity mapping).
//
// The authorization boundary lives here, before the service runs: the
// actor's project membership and role come from the projects service
// (the same instance that serves /api/v1/projects — visibility and
// membership rules can never drift between the two surfaces), and the
// role decides the token scope through the permission matrix facts:
// viewer → read, contributor/maintainer/owner → write. The service then
// trusts the level it is given — the handler is the gate.
//
// Credentials are shown exactly once: the issue response carries the
// token value and a ready-to-use clone URL, and nothing stores them.
package gittokenshttp
