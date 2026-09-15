// Package templates owns the official project template surface (T0214):
// a user creates a project FROM an official template and gets the
// template's defaults applied once — the project row presets, schema
// profile registrations (T0213), the first project policy version (review
// defaults, docs/12 §5) and the initial research-map questions (T0211).
//
// A template never controls the project afterwards. Everything applied is
// an ordinary project-owned row: schema profile versions the owner extends
// through the profile surface, a policy version the owner supersedes
// through the policy surface, scientific objects the owner evolves through
// the RSG commands. The instantiation record
// (project_template_instantiations, 00056) pins the template id + version
// the project was created from; a template upgrade is a NEW version in the
// catalog, which only affects future instantiations — existing projects
// are never re-touched (acceptance: 模板升级不会自动改已有项目).
//
// The catalog is platform code, not user data: the six official templates
// are declared in this package with their id + version. Listing the
// catalog is a public read (templates carry no project state, so there is
// nothing to hide); instantiating runs the ordinary project-create
// authorization (create_project, enforced inside the projects service).
// Every applied default goes through the owning service's own gates with
// the SAME actor — the creator is the new project's owner, so the
// maintainer/owner gates of the profile, policy and RSG surfaces resolve
// the same way they would for a user performing the steps by hand.
//
// Partial application. The instantiation is one request but several
// project-owned writes; after the project row exists, each default is a
// separate step whose outcome the response reports individually (a failed
// profile registration never silently disappears — the caller sees the
// project AND which defaults did not apply). The project itself is the
// primary fact of the request: a later step failing never fakes a failed
// creation that actually happened. The report names the step and the
// failure class; the owning service's error text never travels with it
// (docs/45: stable codes, no dependency detail — a driver error carries
// host, user and database names). The cause goes to the service log
// instead, so the detail is kept where operators read it.
package templates
