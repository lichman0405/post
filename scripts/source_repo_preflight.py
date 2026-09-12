#!/usr/bin/env python3
"""POST canonical source repository preflight (push-blessing gate).

Deterministic, auditable preflight for docs/69: normalizes the local
`origin` remote, verifies the integration branch, and applies the
three-way visibility model of docs/69 §5 —

  1. what the spec package recorded at generation time
     (visibility_at_package_generation — historical, never rewritten);
  2. what the repository owner CONFIRMED as the expectation
     (visibility_expectation — governance fact, see tasks/decisions.md
     L3-20260912-1: private, confirmed 2026-09-12);
  3. what is observed right now (only determinable with credentials /
     network, i.e. in Supervisor/operator context).

The verdict refuses to bless a push (exit 1) when expectation and
observation disagree, when the expectation is not recorded, or when the
visibility cannot be determined ("unknown / cannot verify") — the
preflight never guesses and never silently passes. A non-canonical origin
is rejected the same way. "Private is correct forever" is NOT encoded:
only the recorded expectation vs. the live observation is compared.

Exit codes:
  0  blessed — every gating check passed (push permitted)
  1  SPEC_BLOCKED / refused — a gating check failed or is unknown
     (see verdict.reasons; each reason carries level SPEC_BLOCKED or
     process)
  2  usage error
  3  cannot proceed — spec file unreadable or unparseable

Default behaviour is the safe, credential-free, testable one: no network,
no gh, visibility reported as unknown (-> refused). The credential-
requiring probe is opt-in via --check-visibility (needs Supervisor's gh),
matching the T0000 precedent (ops/doctor.sh --check-docker-daemon).

Injection contract (test-only, docs/69 §2.1): --fixture DIR overrides the
measured inputs so decision logic is testable without touching the real
host or the network. In fixture mode NO real git/gh command is executed:

  git-remote-v.txt        raw `git remote -v` output ('#' lines ignored)
  git-remote-get-url.txt  raw `git remote get-url origin` output
  origin-head.txt         raw `git symbolic-ref refs/remotes/origin/HEAD`
                          output (e.g. 'refs/remotes/origin/main')
  default-branch.txt      authoritative remote default branch
  visibility.txt          observed visibility: public|private|internal|
                          unknown
  gh-visibility.txt       raw `gh api ... --jq .visibility` output
                          (used when --check-visibility is set)
  gh-default-branch.txt   raw `gh api ... --jq .default_branch` output
                          (used when --check-visibility is set)

--observed-visibility VALUE takes precedence over fixtures and the gh
probe (it is the operator's explicit statement of what they observed).
Output: human lines, or --json per specs/orchestrator/
source-repo-preflight.schema.json.

Credential safety: a remote URL may embed a token
(`https://x-access-token:ghp_...@github.com/o/n.git`, as CI checkouts do).
Every URL that reaches `measured`/`detail` is passed through
speclib.redact_url() first, so tokens never land in stdout, the --json
document, or CI logs.

Branch enforcement: BRANCH-DEFAULT is a *gating* check. The local
origin/HEAD record is commonly absent (CI checkouts, fresh clones), so the
operator opt-in also resolves the authoritative remote default branch via
`gh api ... --jq .default_branch`; if neither source can determine it, the
verdict refuses rather than blessing a push over an unverified branch.
"""
import argparse
import json
import sys
from pathlib import Path

import speclib

USAGE_ERROR = 2
CANNOT_PROCEED = 3
PREFLIGHT_VERSION = 1


def parse_args(argv):
    p = argparse.ArgumentParser(
        prog="scripts/source_repo_preflight.py",
        description="POST canonical source repository preflight "
                    "(docs/69 §2.1, §5).")
    p.add_argument("--root", default=str(speclib.default_root()),
                   help="repository root (default: parent of scripts/)")
    p.add_argument("--spec", default=None,
                   help=f"source-repository spec file (default: "
                        f"{speclib.SOURCE_REPO_SPEC_REL} under --root)")
    p.add_argument("--fixture", default=None,
                   help="test-only: fixture dir overriding measured inputs "
                        "(contract: docs/69 §2.1; no real git/gh runs)")
    p.add_argument("--observed-visibility", default=None,
                   choices=["public", "private", "internal", "unknown"],
                   help="live visibility as observed by the operator "
                        "(takes precedence over fixture and gh probe)")
    p.add_argument("--check-visibility", action="store_true",
                   help="opt-in: probe live visibility via "
                        "`gh api repos/{owner}/{name} --jq .visibility` "
                        "(requires Supervisor gh credentials)")
    p.add_argument("--json", action="store_true",
                   help="machine-readable output (schema: "
                        "specs/orchestrator/source-repo-preflight.schema.json)")
    return p.parse_args(argv)


def emit(root, spec_source, checks, verdict, exit_code, want_json):
    """Print result and return the exit code. Human mode prints a fixed
    deterministic report; JSON mode prints a schema-conformant object."""
    if want_json:
        print(json.dumps({
            "preflight_version": PREFLIGHT_VERSION,
            "exit_code": exit_code,
            "spec_source": spec_source,
            "checks": checks,
            "verdict": verdict,
        }, sort_keys=True, indent=2))
        return exit_code
    for c in checks:
        mark = {"passed": "ok  ", "failed": "FAIL",
                "unknown": "----", "error": "ERR "}[c["status"]]
        line = f"{mark} {c['id']:<20} {c['name']}"
        if c["measured"]:
            line += f" [{c['measured']}]"
        if c["detail"]:
            line += f" — {c['detail']}"
        print(line)
    if verdict["reasons"]:
        print(f"VERDICT: refused — push not blessed (exit {exit_code})")
        for r in verdict["reasons"]:
            print(f"  {r['level']:<12} {r['check']}: {r['reason']} — "
                  f"{r['detail']}")
    else:
        print("VERDICT: bless — every gating check passed (exit 0)")
    return exit_code


def run_preflight(root, spec_path, fixture_dir, observed_flag,
                  check_visibility):
    """Run all checks. Returns (checks, verdict, exit_code)."""
    checks = []

    # SPEC-PARSE — unreadable spec means the preflight cannot proceed.
    if not spec_path.is_file():
        return ([speclib.make_check(
                    "SPEC-PARSE", "source-repository spec parses", "error",
                    "spec", "", "parseable YAML",
                    f"spec file not found: {spec_path}")],
                {"push_blessing": "refused",
                 "reasons": [{"check": "SPEC-PARSE",
                              "reason": "spec_unreadable",
                              "level": "process",
                              "detail": f"spec file not found: {spec_path}"}]},
                CANNOT_PROCEED)
    if not speclib.yaml_available():
        return ([speclib.make_check(
                    "SPEC-PARSE", "source-repository spec parses", "error",
                    "spec", "", "parseable YAML",
                    "PyYAML not importable (pip install pyyaml)")],
                {"push_blessing": "refused",
                 "reasons": [{"check": "SPEC-PARSE",
                              "reason": "spec_unreadable",
                              "level": "process",
                              "detail": "PyYAML not importable"}]},
                CANNOT_PROCEED)
    try:
        spec = speclib.load_yaml(spec_path)
    except Exception as exc:  # yaml.YAMLError / OSError
        return ([speclib.make_check(
                    "SPEC-PARSE", "source-repository spec parses", "error",
                    "spec", "", "parseable YAML",
                    f"unreadable: {exc}")],
                {"push_blessing": "refused",
                 "reasons": [{"check": "SPEC-PARSE",
                              "reason": "spec_unreadable",
                              "level": "process",
                              "detail": f"unreadable: {exc}"}]},
                CANNOT_PROCEED)
    checks.append(speclib.make_check(
        "SPEC-PARSE", "source-repository spec parses", "passed", "spec",
        str(spec_path), "parseable YAML", ""))

    src = spec.get("source_repository") if isinstance(spec, dict) else None
    if not isinstance(src, dict):
        src = {}
    owner, name = src.get("owner"), src.get("name")
    default_branch = src.get("default_branch")

    # SPEC-CANONICAL — the spec must be a self-consistent canonical record.
    problems = []
    if not isinstance(owner, str) or not owner:
        problems.append("missing owner")
    if not isinstance(name, str) or not name:
        problems.append("missing name")
    if src.get("full_name") != f"{owner}/{name}":
        problems.append(f"full_name {src.get('full_name')!r} != owner/name")
    if src.get("provider") != "github":
        problems.append(f"provider {src.get('provider')!r} != 'github'")
    if default_branch != "main":
        problems.append(f"default_branch {default_branch!r} != 'main'")
    canonical = f"{owner}/{name}" if owner and name else "<unrecorded>"
    checks.append(speclib.make_check(
        "SPEC-CANONICAL", "spec records a self-consistent canonical repo",
        "passed" if not problems else "failed",
        "spec", canonical, "provider=github, full_name=owner/name, "
        "default_branch=main",
        "; ".join(problems), gating=True,
        block_reason=None if not problems else "spec_not_canonical"))

    # SPEC-EXPECTATION — owner-confirmed visibility expectation recorded.
    expectation = src.get("visibility_expectation")
    exp_value = speclib.visibility_expectation_value(expectation)
    exp_valid = (exp_value is not None
                 and isinstance(expectation.get("confirmed_by"), str)
                 and expectation["confirmed_by"].strip()
                 and "confirmed_at" in expectation)
    if exp_valid:
        detail = (f"expectation {exp_value}, confirmed by "
                  f"{expectation['confirmed_by']} "
                  f"({expectation.get('confirmed_at')})")
        block_reason = None
    elif exp_value is None:
        detail = ("no visibility_expectation recorded: record the "
                  "owner-confirmed expectation (value/confirmed_by/"
                  "confirmed_at) — docs/69 §5, tasks/decisions.md "
                  "L3-20260912-1")
        block_reason = "expectation_unrecorded"
    else:
        detail = ("visibility_expectation incomplete: value/confirmed_by/"
                  "confirmed_at required")
        block_reason = "expectation_invalid"
    checks.append(speclib.make_check(
        "SPEC-EXPECTATION", "owner-confirmed visibility expectation "
        "recorded",
        "passed" if exp_valid else "failed",
        "spec", exp_value or "<unrecorded>",
        "visibility_expectation (value + confirmed_by + confirmed_at)",
        detail, gating=True, block_reason=block_reason))

    # REPO-CANONICAL — normalize origin and compare with the spec record.
    # An empty capture (git ran, no remotes listed) is a definite
    # observation -> remote_missing; a failed capture is indeterminate ->
    # remote_unverifiable. The preflight never confuses the two.
    if fixture_dir is not None:
        source = "fixture"
        determinable = False
        remotes = {}
        detail = ""
        txt_v = speclib.fixture_text(fixture_dir,
                                     speclib.FIXTURE_FILES["remotes_v"])
        txt_url = speclib.fixture_text(fixture_dir,
                                       speclib.FIXTURE_FILES["remotes_url"])
        if txt_v is not None or txt_url is not None:
            determinable = True
            if txt_v is not None:
                remotes = speclib.parse_remote_v_output(txt_v)
                detail = "fixture git-remote-v.txt" + \
                    ("" if remotes else " (no remotes listed)")
            else:
                url = speclib.parse_get_url_output(txt_url)
                remotes = {"origin": url} if url else {}
                detail = "fixture git-remote-get-url.txt" + \
                    ("" if remotes else " (no origin URL)")
        else:
            detail = ("fixture mode: no git-remote-v.txt / "
                      "git-remote-get-url.txt provided")
    else:
        source, remotes, detail = speclib.capture_git_remotes(root)
        determinable = source == "git"
        if source == "none":
            detail = detail or "cannot determine origin remote"
    origin = remotes.get("origin")
    if origin is None:
        if remotes:
            checks.append(speclib.make_check(
                "REPO-CANONICAL", "origin remote normalizes to the canonical "
                "owner/name",
                "failed", source, ", ".join(sorted(remotes)),
                f"origin -> {canonical}",
                f"no 'origin' remote (configured: "
                f"{', '.join(sorted(remotes))})",
                gating=True, block_reason="remote_missing"))
        elif determinable:
            checks.append(speclib.make_check(
                "REPO-CANONICAL", "origin remote normalizes to the canonical "
                "owner/name",
                "failed", source, "", f"origin -> {canonical}",
                detail or "no remotes configured",
                gating=True, block_reason="remote_missing"))
        else:
            checks.append(speclib.make_check(
                "REPO-CANONICAL", "origin remote normalizes to the canonical "
                "owner/name",
                "unknown", source, "", f"origin -> {canonical}",
                detail or "no remotes configured",
                gating=True, block_reason="remote_unverifiable"))
    else:
        norm = speclib.normalize_remote_url(origin)
        if norm is None:
            checks.append(speclib.make_check(
                "REPO-CANONICAL", "origin remote normalizes to the canonical "
                "owner/name",
                "failed", source, speclib.redact_url(origin), f"origin -> {canonical}",
                "origin URL is not a recognizable GitHub owner/name URL",
                gating=True, block_reason="remote_unparseable"))
        elif norm["full_name"].lower() != canonical.lower():
            checks.append(speclib.make_check(
                "REPO-CANONICAL", "origin remote normalizes to the canonical "
                "owner/name",
                "failed", source, norm["full_name"], canonical,
                f"origin '{speclib.redact_url(origin)}' normalizes to "
                f"{norm['full_name']}, "
                f"not canonical {canonical} (docs/69 §2)",
                gating=True, block_reason="remote_not_canonical"))
        else:
            checks.append(speclib.make_check(
                "REPO-CANONICAL", "origin remote normalizes to the canonical "
                "owner/name",
                "passed", source, norm["full_name"], canonical,
                f"origin '{speclib.redact_url(origin)}' "
                f"({norm['form']} form) normalizes to "
                f"{norm['full_name']}",
                gating=True))

    # BRANCH-DEFAULT — the integration branch must be *verifiable* and match.
    # The local origin/HEAD record is routinely absent (CI checkouts, fresh
    # clones), so fall back to the authoritative remote answer that the
    # operator opt-in unlocks. Unknown is gating: a gate that cannot verify
    # the integration branch must refuse, exactly as the visibility gate
    # does — a push blessed over an unverified branch is not verifiable.
    if fixture_dir is not None:
        txt = speclib.fixture_text(fixture_dir,
                                   speclib.FIXTURE_FILES["origin_head"])
        ref = speclib.parse_get_url_output(txt) if txt is not None else ""
        source = "fixture"
        detail = "fixture mode: no origin-head.txt provided"
    else:
        source, ref, detail = speclib.capture_origin_head(root)
    local_branch = ref.rsplit("/", 1)[-1] if ref else ""

    auth_branch, auth_source, auth_detail = speclib.resolve_default_branch(
        fixture_dir, check_visibility, owner, name)

    # `expected` must always be a string for the report; an unrecorded
    # default_branch is already a gating failure via SPEC-CANONICAL.
    expected_branch = (str(default_branch)
                       if isinstance(default_branch, str) and default_branch
                       else "<unrecorded>")

    if local_branch and auth_branch and local_branch != auth_branch:
        checks.append(speclib.make_check(
            "BRANCH-DEFAULT", "integration branch matches spec "
            "default_branch",
            "failed", f"{source}+{auth_source}", local_branch, expected_branch,
            f"state drift: local origin/HEAD records '{local_branch}' but the "
            f"remote reports '{auth_branch}'",
            gating=True, block_reason="branch_mismatch"))
    else:
        observed_branch = local_branch or auth_branch
        observed_source = source if local_branch else auth_source
        observed_detail = (f"origin/HEAD -> {ref}" if local_branch
                           else auth_detail)
        if not observed_branch:
            checks.append(speclib.make_check(
                "BRANCH-DEFAULT", "integration branch matches spec "
                "default_branch",
                "unknown", observed_source, "", expected_branch,
                f"{detail}; cannot verify the integration branch "
                f"(no determinable source) — refusing to bless",
                gating=True, block_reason="branch_unverifiable"))
        elif observed_branch == default_branch:
            checks.append(speclib.make_check(
                "BRANCH-DEFAULT", "integration branch matches spec "
                "default_branch",
                "passed", observed_source, observed_branch, expected_branch,
                observed_detail))
        else:
            checks.append(speclib.make_check(
                "BRANCH-DEFAULT", "integration branch matches spec "
                "default_branch",
                "failed", observed_source, observed_branch, expected_branch,
                f"integration branch is '{observed_branch}' "
                f"({observed_detail}), spec default_branch is "
                f"'{default_branch}'",
                gating=True, block_reason="branch_mismatch"))

    # VISIBILITY — the three-way model verdict (docs/69 §5).
    if not exp_valid:
        checks.append(speclib.make_check(
            "VISIBILITY", "live visibility matches owner expectation",
            "error", "spec", "", f"expectation {exp_value or '<unrecorded>'}",
            "cannot evaluate: expectation not validly recorded "
            "(see SPEC-EXPECTATION)", gating=False))
    else:
        obs, vsource, vdetail = speclib.resolve_observed_visibility(
            observed_flag, fixture_dir, check_visibility, root, owner, name)
        status, reason, detail = speclib.visibility_verdict(expectation, obs)
        checks.append(speclib.make_check(
            "VISIBILITY", "live visibility matches owner expectation",
            status, vsource, obs, f"expectation {exp_value}",
            f"{vdetail}; {detail}" if vdetail else detail,
            gating=True, block_reason=reason))

    # SPEC-VERSION — checked-in marker must match the derived digest.
    derived = speclib.derive_spec_version(root)
    marker_path = root / speclib.SPEC_VERSION_REL
    if not marker_path.is_file():
        checks.append(speclib.make_check(
            "SPEC-VERSION", "spec version marker matches derived digest",
            "failed", "file", "<missing>", derived["combined_digest"],
            f"{speclib.SPEC_VERSION_REL} missing — run "
            f"scripts/spec_version.py --write",
            gating=True, block_reason="version_missing"))
    else:
        try:
            marker = speclib.load_json(marker_path)
        except (OSError, json.JSONDecodeError) as exc:
            marker = None
        got = marker.get("combined_digest") if isinstance(marker, dict) \
            else None
        if got != derived["combined_digest"]:
            checks.append(speclib.make_check(
                "SPEC-VERSION", "spec version marker matches derived digest",
                "failed", "file", got or "<unreadable>",
                derived["combined_digest"],
                "checked-in marker is stale — run "
                "scripts/spec_version.py --write",
                gating=True, block_reason="version_stale"))
        else:
            checks.append(speclib.make_check(
                "SPEC-VERSION", "spec version marker matches derived digest",
                "passed", "file", derived["spec_version"],
                derived["combined_digest"],
                f"marker current ({derived['input_count']} inputs)"))

    verdict = speclib.assemble_verdict(checks)
    exit_code = 0 if verdict["push_blessing"] == "bless" else 1
    return checks, verdict, exit_code


def main(argv):
    args = parse_args(argv)
    root = Path(args.root)
    if not root.is_dir():
        print(f"error: root directory not found: {root}", file=sys.stderr)
        return USAGE_ERROR
    if args.fixture is not None and not Path(args.fixture).is_dir():
        print(f"error: fixture directory not found: {args.fixture}",
              file=sys.stderr)
        return USAGE_ERROR
    spec_rel = args.spec or speclib.SOURCE_REPO_SPEC_REL
    spec_path = Path(args.spec) if args.spec else root / spec_rel
    checks, verdict, exit_code = run_preflight(
        root, spec_path, args.fixture, args.observed_visibility,
        args.check_visibility)
    return emit(root, str(spec_path), checks, verdict, exit_code, args.json)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
