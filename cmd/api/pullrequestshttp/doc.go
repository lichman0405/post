// Package pullrequestshttp is the pull-request API surface (T0403): the
// project's PR list, one PR's detail, and one PR's integrity check report
// — the machine result the PR page's checks section renders (the T0408
// "Checks" tab consumes the same endpoint).
//
// Every route runs the project read gate first (T0106 read matrix): a PR
// and its report are exactly as visible as their project, so a denied
// read answers the same existence-hiding 404 as every other project read.
// The detail and checks endpoints parse the {number} segment strictly:
// a non-numeric segment is a 404, never a 400 — the URL names nothing.
package pullrequestshttp
