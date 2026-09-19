// Package aborthttp is the abort-proposal HTTP surface (T0602): the one
// route that proposes aborting a main-line scientific object,
//
//	POST /api/v1/projects/{projectId}/objects/{objectId}:abort-proposal
//
// (specs/api/openapi.yaml:345-355, summary "Propose abort for object; main
// objects require PR flow"). It carries no abort logic: it resolves the
// actor, the object, the Idempotency-Key and the body, calls the aborts
// command, and maps the command's own outcomes onto the wire envelope.
// Every rule about who may abort, what an abort records and what it writes
// lives behind that call (internal/application/aborts).
//
// # Two things the contract does not declare, and where they come from
//
// The route's declaration names the path and the 201 and nothing else: it
// declares no requestBody and no Idempotency-Key parameter. Both are
// therefore this task's design, and both are taken from something that
// already exists rather than invented:
//
//   - the body's field names are the MCP tool's argument names
//     (specs/mcp/tools.json:23, `object.abort_proposal(project_id,
//     object_version_ref, reason_code, explanation)`), the same choice
//     cmd/api/knowledgehttp/publish.go:32 records for the publish route's
//     `knowledge_version_ref`. The one field the tool does not name —
//     `replacement_ref` — is docs/46:7's optional
//     "replacement/superseding ref", which the abort must be able to
//     record; it is optional in the body exactly as it is in the record.
//   - Idempotency-Key is the shared contract parameter
//     (components.parameters.IdempotencyKey: required, minLength 8), the
//     same header every other governed write route requires, and it is
//     required here for the reason the abort is governed: a retried
//     proposal must be answered by the proposal it already created rather
//     than create a second one.
//
// # No read gate in front of the command
//
// The route runs no project read gate, deliberately, exactly as the freeze
// route does not (cmd/api/freezehttp/doc.go). The acceptance criterion is
// that an abort of an object that does not exist by a caller who may not
// abort must be indistinguishable from an abort of a real object by that
// caller: the refusal is an authorization outcome resolved before any
// lookup, and a read gate would answer 404 for a project that exists and
// disclose exactly what the command refuses to disclose.
//
// # The path's shape
//
// The route's last segment carries a literal suffix on a wildcard, which
// Go's ServeMux cannot express as `{objectId}:abort-proposal` any more than
// it can express mergehttp's `{number}:merge`. Both use the same
// workaround: the wildcard is the REST of the path (`{objectRef...}`) and
// the handler splits the suffix off itself, answering 404 for a segment that
// lacks it (the URL names nothing).
package aborthttp
