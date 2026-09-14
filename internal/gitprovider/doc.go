// Package gitprovider defines the port over the internal Gitea transport
// (ADR-003, ADR-019, docs/16_GIT_COMPATIBILITY.md) and the repository
// provisioning that runs on top of it (T0301).
//
// Product domain objects never use Gitea identity as primary identity:
// the platform-side mapping lives in projects.git_repository_external_id
// and the git_repository_provisions table, and the adapter talks to the
// Gitea REST API only — the domain layer never references Gitea's
// database (acceptance criterion: domain 不引用 Gitea DB).
//
// Layout:
//
//   - port.go: the GitPort interface (the ADR-003 exit hatch) and the
//     provider value types. Future Git-layer tasks extend the port
//     (refs T0303, user tokens T0304).
//   - gitea.go: the Gitea REST API v1 adapter, authenticating as the
//     service account (post-git-svc bot user).
//   - provisioning.go: the application service — one project → one
//     repository ("p-"+uuid, created private in the service account's
//     namespace) → one push webhook with a fresh HMAC secret → main
//     protected before the project is recorded provisioned.
//   - mainprotection.go: the Gitea layer of main's double protection
//     (T0302) — the canonical branch-protection rule, created and
//     converged on every provisioned repository.
//   - refguard.go: the platform layer's ref guard (T0302) — the
//     platform's own policy over ref updates, applied wherever the
//     platform observes or performs Git writes.
//   - sweeper.go: the platform layer's enforcement loop (T0302) — a
//     scheduled pass that re-applies the rule when an operator removed
//     or drifted it.
//   - store.go: the canonical-store side — the only writer of
//     projects.provision_status / projects.git_repository_external_id and
//     the only reader/writer of git_repository_provisions (including the
//     webhook secret).
//   - config.go: the validated adapter configuration (POST_GITEA_*).
package gitprovider
