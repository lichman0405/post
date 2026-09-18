package knowledgehttp

import (
	"context"
	"net/http"

	"github.com/lichman0405/post/internal/application/knowledgepublish"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/domain"
)

// The publication surface's wiring: the command, the public read, the project
// read gate and the membership read go in; three routes come out (the
// session/CSRF guard is owned by authhttp and composed in cmd/api/main.go,
// like every other product route).

// PublishCommand is the knowledge publication use case this surface drives
// (the production value is *knowledgepublish.Command).
type PublishCommand interface {
	// Preview answers what the publish would be and who would see it,
	// writing nothing (specs/mcp/tools.json: knowledge.publish_preview is a
	// PROPOSAL).
	Preview(ctx context.Context, in knowledgepublish.PreviewParams) (knowledgepublish.Preview, error)
	// Publish performs the governance write.
	Publish(ctx context.Context, actor knowledgepublish.Actor, in knowledgepublish.PublishParams) (knowledgepublish.Published, error)
}

// Read resolves one published knowledge object by its pid. The production
// value is *persistence.KnowledgePublishStore.
//
// It applies NO audience filter itself: the rule is
// knowledgepublish.AudienceFor and the handler applies it to what this
// returns, so the read route and the publish decision cannot come to
// different conclusions about who may read a publication.
type Read interface {
	GetPublishedKnowledge(ctx context.Context, pid string) (knowledgepublish.PublishedKnowledge, bool, error)
}

// Gate is the project read gate — the same member-only, existence-hiding
// rule as the project surface (T0106), for the two routes that are nested
// under a project. The production value is *projects.Service
// (projectAPI.Service() in main.go).
type Gate interface {
	Get(ctx context.Context, r projects.Reader, projectID string) (domain.Project, error)
}

// Membership answers whether an authenticated caller belongs to a project —
// the one bit that separates reading the network's view of a publication
// from reading its own project's.
//
// A caller with no membership answers projects.ErrMemberNotFound, which is
// "no role" rather than a failure. The production value is *projects.Service
// (the same service the Gate above is: GetMembership re-runs the project read
// gate first, and a second implementation of "is this caller a member" would
// be a second answer to it).
type Membership interface {
	GetMembership(ctx context.Context, actor domain.User, projectID string) (domain.ProjectMembership, error)
}

// Deps carries the adapters the surface needs.
type Deps struct {
	// Publish is the publication command (T0805).
	Publish PublishCommand
	// Read is the public read of a published knowledge object.
	Read Read
	// Projects is the project read gate.
	Projects Gate
	// Members is the membership read the public read needs to tell a
	// member's view of a publication from the network's.
	Members Membership
}

// New wires the handlers.
func New(deps Deps) *API {
	return &API{handlers: &handlers{
		publish:  deps.Publish,
		read:     deps.Read,
		projects: deps.Projects,
		members:  deps.Members,
	}}
}

// API is the mounted knowledge surface.
type API struct {
	handlers *handlers
}

type handlers struct {
	publish  PublishCommand
	read     Read
	projects Gate
	members  Membership
}

// Register mounts the three routes on the v1 mux.
//
// The full-path patterns are more specific than the /api/v1/projects/ subtree,
// so they coexist with it (the same technique policyhttp, releasehttp and
// assetshttp use). The paths — including the ":publish-preview" and
// ":publish" suffixes inside one segment — are the contract's, copied
// verbatim; a client cannot be asked to spell them differently.
//
// The read is the contract's /knowledge/{knowledgeId} with the server prefix,
// and it is deliberately NOT nested under a project: the identity a client
// holds is the publication's pid, exactly as the asset page's is the asset's
// (assetshttp: "the scope that decides visibility is the ASSET"). Who may read
// it is decided by the publication's own audience rule, not by a project
// segment the caller would have to know.
func (a *API) Register(v1 *http.ServeMux) {
	v1.HandleFunc("POST /api/v1/projects/{projectId}/knowledge:publish-preview", a.handlers.handlePublishPreview)
	v1.HandleFunc("POST /api/v1/projects/{projectId}/knowledge:publish", a.handlers.handlePublish)
	v1.HandleFunc("GET /api/v1/knowledge/{knowledgeId}", a.handlers.handlePublishedKnowledge)
}
