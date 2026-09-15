// Package reviews orchestrates the per-dimension scientific review use
// cases over pull requests (task T0404; docs/09 §5, docs/43).
//
// The model: one review row records ONE decision of ONE dimension
// (scientific or integrity) about ONE proposed head, attributable to the
// reviewer and — when one resolves — to the scientific responsibility
// the reviewer acted under (docs/04 §3). Multiple reviewers, dimensions
// and rounds coexist as rows; no single approve flattens the
// differences.
//
// Authorization (acceptance "权限正确") is enforced inside the service,
// before any PR lookup: the submit_scientific_review matrix row with
// ClassOf over the membership role — maintainer/owner allowed, and the
// conditional verdict for viewer/contributor/non-member resolved only
// through the reviewer-responsibility hook (ResponsibilityGate), which
// fails closed until T0604 lands the rule-based resolver.
package reviews
