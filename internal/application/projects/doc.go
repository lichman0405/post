// Package projects orchestrates the project-creation shell (T0104): create
// a project with a Public/Private visibility preset, a required purpose and
// an optional program, making the creator its owner. Every new project is
// provision-pending — the GitProvider repository is provisioned later
// (T0301). The permission matrix (T0105) and public/private read isolation
// (T0106) build on this shell.
//
// Policy lives here (docs/52): the transport layer only translates requests
// into service calls. All policy in this package is L1 — implementation
// decisions recorded for the Supervisor, never silently invented product
// semantics (CLAUDE.md §5).
package projects
