// Package projectshttp is the project HTTP surface (T0104/T0106): create
// a project with a Public/Private visibility preset, purpose and optional
// program; the creator becomes owner and the project is provision-pending.
// Reads are visibility-aware (T0106): public project shells are readable
// by anyone — anonymous included; private projects answer the
// existence-hiding 404 for everyone but members, and the list never
// contains a private project the caller may not see. Writes still require
// a session; every handler parses the request, calls the projects
// Service, and renders the payload or the standard error envelope.
package projectshttp
