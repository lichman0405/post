// Package projects orchestrates the project-creation shell (T0104): create
// a project with a Public/Private visibility preset, a required purpose and
// an optional program, making the creator its owner. Every new project is
// provision-pending — the GitProvider repository is provisioned later
// (T0301). The permission matrix (T0105) and public/private read isolation
// (T0106) build on this shell: reads are visibility-aware — public project
// shells are readable by anyone (anonymous included), private projects
// answer the existence-hiding 404 for everyone but members, and the list
// only ever contains projects the caller may see.
//
// Policy lives here (docs/52): the transport layer only translates requests
// into service calls. All policy in this package is L1 — implementation
// decisions recorded for the Supervisor, never silently invented product
// semantics (CLAUDE.md §5).
package projects
