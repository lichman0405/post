// Package releasehttp is the HTTP surface of the immutable release
// (T0606): POST to create (the only write — releases are append-only,
// there is no update and no delete; the mux answers 405 for them), and
// the reads: the project's release list, one release, and the manifest
// export that renders only from the stored snapshot. Reads are exactly
// as visible as the project (the shared project read gate); the create
// runs the release command's own authorization (ActionCreateRelease).
package releasehttp
