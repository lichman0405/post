#!/usr/bin/env python3
"""Shared logic for the POST spec validation suite (scripts/).

This module is pure decision logic: no network, no git mutation, no
randomness, no timestamps, fixed ordering. It exists so the CLI entry
points stay thin and every decision is exercisable through the documented
injection contracts (validate_specs.py --root, source_repo_preflight.py
--fixture / --observed-visibility) without touching the real host or the
network — the T0000 precedent (ops/doctor.sh, fixture hook §6 of
ops/doctor-checks.md).

Entry points:
  scripts/validate_specs.py          spec bundle + task DAG validator (CI gate)
  scripts/spec_version.py            derived spec version marker (deterministic)
  scripts/source_repo_preflight.py   canonical source repository preflight
"""
from pathlib import Path
import hashlib
import json
import subprocess

# ---------------------------------------------------------------------------
# Canonical layout and constants
# ---------------------------------------------------------------------------

DEFAULT_TASKS_REL = "tasks/tasks.json"
SPECS_DIR_REL = "specs"
DOCS_DIR_REL = "docs"
SPEC_VERSION_REL = "specs/SPEC_VERSION.json"
SOURCE_REPO_SPEC_REL = "specs/orchestrator/source-repository.yaml"

# The version marker is the output of derivation, never one of its inputs.
SPEC_VERSION_EXCLUDED = {SPEC_VERSION_REL}

GITHUB_HOSTS = ("github.com", "www.github.com")
VISIBILITY_VALUES = ("public", "private", "internal")

# Block reasons that are governance SPEC_BLOCKED signals (L3 territory) vs.
# plain process errors. Anything not listed here defaults to SPEC_BLOCKED:
# refusing to bless is the safe default.
SPEC_BLOCKED_REASONS = {
    "expectation_unrecorded", "expectation_invalid",
    "visibility_mismatch", "visibility_unverifiable",
    "remote_not_canonical", "remote_unparseable", "remote_unverifiable",
    "remote_missing", "branch_mismatch", "spec_not_canonical",
}
PROCESS_REASONS = {"version_stale", "version_missing", "spec_unreadable"}


def default_root() -> Path:
    """Repository root: the parent of scripts/."""
    return Path(__file__).resolve().parents[1]


# ---------------------------------------------------------------------------
# Deterministic file discovery and hashing
# ---------------------------------------------------------------------------

def list_files(root: Path, rel_dirs, suffixes=None):
    """Sorted repo-relative paths of every file under the given dirs."""
    out = []
    for rel in rel_dirs:
        base = root / rel
        if not base.is_dir():
            continue
        for p in base.rglob("*"):
            if p.is_file() and (suffixes is None or p.suffix in suffixes):
                out.append(p.relative_to(root).as_posix())
    return sorted(out)


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def file_sha256(path: Path) -> str:
    return sha256_bytes(path.read_bytes())


def derive_spec_version(root: Path) -> dict:
    """Deterministically derive the spec version marker from the spec inputs.

    Inputs: tasks/tasks.json plus every file under specs/ except the marker
    file itself. Deterministic: sorted paths, SHA-256 over raw bytes, no
    timestamps, no randomness.
    """
    rels = [DEFAULT_TASKS_REL] + [
        r for r in list_files(root, (SPECS_DIR_REL,))
        if r not in SPEC_VERSION_EXCLUDED
    ]
    files = {}
    for rel in sorted(rels):
        files[rel] = file_sha256(root / rel)
    combined = sha256_bytes(
        "".join(f"{rel}\x00{digest}\n" for rel, digest in sorted(files.items()))
        .encode("utf-8")
    )
    return {
        "marker_format": 1,
        "algorithm": "sha256",
        "input_count": len(files),
        "combined_digest": combined,
        "spec_version": "sha256:" + combined[:16],
        "files": files,
    }


# ---------------------------------------------------------------------------
# YAML / JSON loading
# ---------------------------------------------------------------------------

def load_json(path: Path):
    with open(path, encoding="utf-8") as f:
        return json.load(f)


def yaml_available() -> bool:
    try:
        import yaml  # noqa: F401
        return True
    except ImportError:
        return False


def load_yaml(path: Path):
    """Load a YAML file; raises ImportError when PyYAML is unavailable
    (callers gate on yaml_available() to produce a clear error)."""
    import yaml
    with open(path, encoding="utf-8") as f:
        return yaml.safe_load(f)


# ---------------------------------------------------------------------------
# Remote URL normalization (git remote output parsing)
# ---------------------------------------------------------------------------

def normalize_remote_url(url):
    """Normalize a git remote URL to {owner, name, full_name, form}.

    Supported GitHub forms:
      https://github.com/owner/name.git        (also http://)
      ssh://git@github.com/owner/name.git
      git://github.com/owner/name.git
      git@github.com:owner/name.git            (scp-like)
    A trailing '.git' and trailing '/' are stripped. Returns None when the
    URL is not a recognizable GitHub owner/name URL.
    """
    if not url or not isinstance(url, str):
        return None
    u = url.strip()
    if not u or u.startswith("#"):
        return None
    host = None
    path = None
    form = None
    if "://" in u:
        scheme, rest = u.split("://", 1)
        if scheme not in ("https", "http", "ssh", "git"):
            return None
        if "@" in rest:
            # ssh://git@github.com/... — drop the optional user part
            rest = rest.rsplit("@", 1)[1]
        host, _, path = rest.partition("/")
        form = scheme
    elif ":" in u:
        # scp-like: [user@]github.com:owner/name.git
        user_host, _, path = u.partition(":")
        if "@" in user_host:
            user_host = user_host.rsplit("@", 1)[1]
        host = user_host
        form = "scp"
    else:
        return None
    if not host or not path:
        return None
    host = host.lower()
    if host not in GITHUB_HOSTS:
        return None
    parts = [p for p in path.split("/") if p != ""]
    if len(parts) != 2:
        return None
    owner, name = parts
    if name.lower().endswith(".git"):
        name = name[:-4]
    if not owner or not name:
        return None
    if not all(c.isalnum() or c in "-_." for c in owner + name):
        return None
    return {"owner": owner, "name": name,
            "full_name": f"{owner}/{name}", "form": form}


def parse_remote_v_output(text):
    """Parse `git remote -v` output into {remote_name: url}.

    Expected line form: '<name>\\t<url> (<fetch|push>)'. Tolerates missing
    ' (type)' suffix and variable whitespace. Lines starting with '#' are
    ignored (fixture convention).
    """
    remotes = {}
    for raw in (text or "").splitlines():
        line = raw.rstrip("\n")
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        parts = line.split()
        if len(parts) < 2:
            continue
        remotes.setdefault(parts[0], parts[1])
    return remotes


def parse_get_url_output(text):
    """Parse `git remote get-url <name>` output: the last non-empty,
    non-comment line."""
    out = None
    for raw in (text or "").splitlines():
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        out = line
    return out


# ---------------------------------------------------------------------------
# Visibility verdict (three-way model: generation record / owner expectation
# / live observation — docs/69 §5, tasks/decisions.md L3-20260912-1)
# ---------------------------------------------------------------------------

def visibility_expectation_value(expectation):
    """The normalized expectation value, or None when not validly recorded."""
    if isinstance(expectation, dict):
        v = expectation.get("value")
        if isinstance(v, str) and v.lower() in VISIBILITY_VALUES:
            return v.lower()
    return None


def visibility_verdict(expectation, observed):
    """Decide the visibility check from the recorded owner expectation and
    the live observation.

    Returns (status, reason, detail): status is 'passed' | 'failed' |
    'unknown'; reason is the block code (None when passed). An unrecorded
    expectation and an undeterminable observation both refuse the blessing —
    the preflight never guesses and never silently passes.
    """
    exp_val = visibility_expectation_value(expectation)
    if exp_val is None:
        return ("failed", "expectation_unrecorded",
                "owner-confirmed visibility expectation is not recorded in "
                "specs/orchestrator/source-repository.yaml "
                "(visibility_expectation); record the owner decision first")
    obs = (observed or "").strip().lower()
    if obs not in VISIBILITY_VALUES:
        return ("unknown", "visibility_unverifiable",
                "live visibility is unknown / cannot be verified; refusing "
                "to bless a push on an unverified visibility state "
                "(docs/69 §5)")
    if obs == exp_val:
        return ("passed", None,
                f"live visibility '{obs}' matches owner-confirmed "
                f"expectation '{exp_val}'")
    return ("failed", "visibility_mismatch",
            f"live visibility '{obs}' does not match owner-confirmed "
            f"expectation '{exp_val}': L3 governance block per docs/69 §5")


# ---------------------------------------------------------------------------
# Check / verdict model
# ---------------------------------------------------------------------------

CHECK_STATUSES = ("passed", "failed", "unknown", "error")


def make_check(cid, name, status, source, measured="", expected="",
               detail="", gating=False, block_reason=None):
    """One preflight/validation check. `gating` marks checks whose failure
    or unknown status refuses the push blessing; `block_reason` is the
    machine-readable reason code used in the verdict."""
    return {
        "id": cid, "name": name, "status": status, "source": source,
        "measured": str(measured), "expected": str(expected),
        "detail": str(detail), "gating": bool(gating),
        "block_reason": block_reason,
    }


def assemble_verdict(checks):
    """Verdict over the check list. A check blocks the push blessing when
    gating=True and status is failed or unknown. Reasons carry a level:
    SPEC_BLOCKED (governance signal, docs/69 §5) or process (fixable
    bookkeeping error)."""
    reasons = []
    for c in checks:
        if c.get("gating") and c["status"] in ("failed", "unknown"):
            reason = c.get("block_reason") or \
                f"{c['id'].lower().replace('-', '_')}_{c['status']}"
            level = "process" if reason in PROCESS_REASONS else "SPEC_BLOCKED"
            reasons.append({"check": c["id"], "reason": reason,
                            "level": level, "detail": c["detail"]})
    return {"push_blessing": "bless" if not reasons else "refused",
            "reasons": reasons}


# ---------------------------------------------------------------------------
# Real-host capture (read-only git / gh; runs only outside fixture mode)
# ---------------------------------------------------------------------------

def run_cmd(argv, cwd=None, timeout=15):
    """Run a read-only command; never raises. Returns (rc, stdout, stderr)."""
    try:
        proc = subprocess.run(argv, cwd=cwd, capture_output=True, text=True,
                              timeout=timeout)
        return proc.returncode, proc.stdout or "", proc.stderr or ""
    except FileNotFoundError:
        return 127, "", f"command not found: {argv[0]}"
    except subprocess.TimeoutExpired:
        return 124, "", f"command timed out: {' '.join(argv)}"
    except OSError as exc:
        return 126, "", f"command failed: {exc}"


def capture_git_remotes(root):
    """Capture the configured git remotes of the repository at `root`.

    Tries `git remote -v`, falling back to `git remote get-url origin`.
    Returns (source, remotes, detail): source 'git' when git itself ran
    (remotes may be empty — no remotes configured is an observation);
    source 'none' with remotes {} when the remotes could not be determined
    at all.
    """
    rc, out, err = run_cmd(["git", "remote", "-v"], cwd=str(root))
    if rc == 0:
        return "git", parse_remote_v_output(out), ""
    rc2, out2, err2 = run_cmd(["git", "remote", "get-url", "origin"],
                              cwd=str(root))
    if rc2 == 0 and out2.strip():
        url = parse_get_url_output(out2)
        if url:
            return "git", {"origin": url}, ""
    detail = ("cannot determine origin remote: "
              f"git remote -v rc={rc} ({err.strip()}); "
              f"git remote get-url origin rc={rc2} ({err2.strip()})")
    return "none", {}, detail


def capture_origin_head(root):
    """Capture `git symbolic-ref refs/remotes/origin/HEAD` — the recorded
    integration branch of origin. Returns (source, ref, detail); ref is ''
    when not determinable."""
    rc, out, err = run_cmd(["git", "symbolic-ref", "refs/remotes/origin/HEAD"],
                           cwd=str(root))
    if rc == 0 and out.strip():
        return "git", out.strip(), ""
    return "none", "", ("cannot determine origin/HEAD: "
                        f"rc={rc} ({err.strip()})")


# ---------------------------------------------------------------------------
# Fixture injection contract (docs/69 §2.1)
# ---------------------------------------------------------------------------

FIXTURE_FILES = {
    "remotes_v": "git-remote-v.txt",
    "remotes_url": "git-remote-get-url.txt",
    "origin_head": "origin-head.txt",
    "visibility": "visibility.txt",
    "gh_visibility": "gh-visibility.txt",
}


def fixture_text(fixture_dir, name):
    """Read a fixture file; None when fixture mode is off or the file is
    absent. In fixture mode absent inputs stay absent: the preflight never
    falls back to real host measurements once a fixture dir is given."""
    if not fixture_dir:
        return None
    path = Path(fixture_dir) / name
    if not path.is_file():
        return None
    try:
        return path.read_text(encoding="utf-8")
    except OSError:
        return ""


def resolve_observed_visibility(flag_value, fixture_dir, check_visibility,
                                root, owner, name):
    """Resolve the live visibility observation, in precedence order:
    --observed-visibility flag > fixture visibility.txt >
    --check-visibility gh probe (fixture gh-visibility.txt in fixture mode)
    > unknown. Returns (value, source, detail)."""
    if flag_value:
        return flag_value.lower(), "flag", "--observed-visibility"
    txt = fixture_text(fixture_dir, FIXTURE_FILES["visibility"])
    if txt is not None:
        lines = [l.strip() for l in txt.splitlines() if l.strip()]
        value = lines[0] if lines else "unknown"
        return value.lower(), "fixture", "fixture visibility.txt"
    if check_visibility:
        if fixture_dir is not None:
            txt = fixture_text(fixture_dir, FIXTURE_FILES["gh_visibility"])
            if txt is not None:
                lines = [l.strip() for l in txt.splitlines() if l.strip()]
                value = lines[-1] if lines else "unknown"
                return value.lower(), "fixture", "fixture gh-visibility.txt"
        rc, out, err = run_cmd(["gh", "api", f"repos/{owner}/{name}",
                                "--jq", ".visibility"])
        if rc == 0 and out.strip():
            lines = [l.strip() for l in out.splitlines() if l.strip()]
            return lines[-1].lower(), "gh", \
                f"gh api repos/{owner}/{name} --jq .visibility"
        return "unknown", "gh", f"gh probe failed: rc={rc} ({err.strip()})"
    return "unknown", "none", ("no visibility input provided "
                               "(use --observed-visibility or "
                               "--check-visibility)")
