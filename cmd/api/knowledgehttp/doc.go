// Package knowledgehttp is the knowledge publication surface (T0805): the
// contract's publish pair (POST .../knowledge:publish-preview and POST
// .../knowledge:publish) and the public read the publlcation makes possible
// (GET /knowledge/{knowledgeId}).
//
// # Where the routes come from
//
// All three paths are the CONTRACT's (specs/api/openapi.yaml):
//
//	/projects/{projectId}/knowledge:publish-preview
//	/projects/{projectId}/knowledge:publish
//	/knowledge/{knowledgeId}   (security: [], "Published knowledge object
//	                            plus origin/network evidence state")
//
// The first two are registered verbatim under the v1 server prefix, the way
// assetshttp registers the asset pair; the read carries `security: []`, so
// an anonymous caller reaches it and the handler decides what that caller
// may see — which is what `security: []` means everywhere else in this tree.
//
// # What the handlers decide, and what they do not
//
// Nothing about a publication is decided here. The body is read, the
// Idempotency-Key comes off the header, the principal is resolved, and the
// command runs; every governance decision — whether this actor may publish,
// whether the version passed publication_review, whether it is already
// published, and what the row contains — belongs to
// internal/application/knowledgepublish and commits or refuses inside that
// command's transaction. A transport that decided any of it would be the
// second, weaker copy of a security decision.
//
// The read route does decide one thing, and it is the one thing that belongs
// here: whether THIS caller may see the publication it resolved. The rule
// itself is knowledgepublish.AudienceFor (the command's own function, not a
// second one written for the read); what the transport adds is the
// membership fact the rule needs for a members-only publication, and the
// 404 it answers when the rule refuses.
package knowledgehttp
