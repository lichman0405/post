// Package projectshttp is the project HTTP surface (T0104): create a
// project with a Public/Private visibility preset, purpose and optional
// program; the creator becomes owner and the project is provision-pending.
// Every handler: resolve the principal (the guard put it there — reads
// require a session too, project visibility is member-only until T0106),
// parse the request, call the projects Service, render the payload or the
// standard error envelope.
package projectshttp
