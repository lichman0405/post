#!/usr/bin/env python3
"""Do the POST production runbooks still describe the commands this tree has?

Runbook / release / recovery verification, as a command (task T1204). The
corpus is the five documents an operator actually follows — docs/35 (deploy),
docs/36 (release), docs/37 (backup/DR), ops/deploy/README.md (the staging
runbook with the real commands) and ops/DEV_COMMANDS.md (the command book) —
declared in ops/runbook-steps.json rather than hard-coded here, so the set is
reviewable in one place.

What a rule is. Each rule reads the corpus and the tree and reports findings;
no findings means the documents and the commands agree *about the things the
rule looks at*. Nothing here executes the deployment: the commands that need a
Docker daemon are checked for well-formedness against the files they name, and
tests/acceptance/runbook-drill.sh runs the ones that can run on a bare host.

What this is NOT. A proof that a deployment works. There is no Dockerfile in
this repository, so no image exists and nothing can be started (ops/deploy/
README.md § "What is missing"). A rule that claimed "staging deploys" here
would be measuring nothing.

The rules:

  D1 make-targets     every `make <target>` the corpus tells an operator to run
                      is a target in the Makefile.
  D2 script-claims    every `bash X` / `python3 X` / `./X` is a file that
                      exists, and the interpreter claimed matches it.
  D3 script-flags     every `--flag` attached to a script invocation appears in
                      that script's own usage text (the heredoc for a shell
                      script, the argparse declarations for a Python one).
  D4 compose-claims   every `docker compose [-f FILE] <sub> [services]` names a
                      file that exists, services that exist in it, and a
                      `--profile` the file declares.
  D5 env-vars         every POST_*/GITEA_* variable the deployment corpus names
                      is defined in ops/deploy/staging.env.example, in the
                      staging compose file, or in the repository .env.example.
  D6 inventory        ops/runbook-steps.json covers exactly the sections of
                      docs/35, docs/36 and docs/37 (both directions), every
                      anchor is still verbatim in its document, and a step
                      classified `mechanised` or `declared` names a command
                      that resolves in this tree and that a document names.
                      `declared` means "the command exists and cannot run yet",
                      so it must point at the tree claim that blocks it — when
                      that claim stops holding, the classification has to be
                      revisited instead of outliving the blocker.
  D7 rddev-subcommands every `rddev <sub>` a command block tells a supervisor
                      to run is a subcommand cmd/rddev/main.go documents. Only
                      blocks are read: an inline `rddev …` in prose is a
                      reference, not an instruction.
  T1 tree-claims      a document that says "no X exists in this repository" is
                      re-checked against the tree (ops/runbook-steps.json
                      § tree_claims).
  T2 forward-only     the runbooks' rollback rule ("the application rolls back
                      to the previous image; the database only moves forward")
                      is re-derived from the tree: no down path is reachable
                      from the command surface.

Declared gaps. A document may contain a command list it declares to be
aspirational ("目标接口", ops/DEV_COMMANDS.md, "should support after P0"). Such
a block is excluded from the command rules AND the exclusion is itself
checked: if the marker sentence disappears, the block is enforced again. An
exclusion nobody can retract would be a hole in the rule set.

--selftest proves each rule can fail: one mutation per rule, applied to a
temporary copy (a document, the inventory, or a synthetic tree), which that
rule must reject. A rule whose mutation survives is reported as a failure —
a check that cannot fail is not evidence.

Exit codes: 0 every rule held; 1 at least one finding (or a surviving
mutation); 2 usage error; 3 the corpus or the inventory is missing.
"""
import argparse
import json
import re
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

USAGE_ERROR = 2
MISSING_INPUT = 3

# The fence languages a command block may use. A fence in another language
# (```json, ```text) is prose or data and is not read as commands.
COMMAND_FENCES = {"bash", "sh", "shell", "console"}

# docker compose subcommands the corpus may use. The list is the subset of
# Compose's surface a deployment runbook has any business in; a runbook that
# reaches for something else here is a finding for a human to look at, not
# something this rule should silently accept.
COMPOSE_SUBCOMMANDS = {"config", "up", "down", "run", "ps", "logs", "exec", "pull"}

# Environment variables that are not the deployment's to define: the shell's
# own knobs plus the two the docs use to talk about the operator's tools.
ENV_VAR_ALLOWLIST = {"POST_STAGING_PROJECT"}  # defined in the compose file itself


class Finding:
    def __init__(self, rule, message, evidence=""):
        self.rule = rule
        self.message = message
        self.evidence = evidence

    def __str__(self):
        line = "%-4s %s" % (self.rule, self.message)
        if self.evidence:
            line += "\n       %s" % self.evidence.replace("\n", "\n       ")
        return line


class Claim:
    """One thing a document tells the reader to run, and where it says so."""

    def __init__(self, doc, line, text):
        self.doc = doc
        self.line = line
        self.text = text

    def where(self):
        return "%s:%d" % (self.doc, self.line)


class Ctx:
    def __init__(self, root, corpus, docs, inventory, overrides):
        self.root = Path(root)
        self.corpus = corpus
        self.docs = docs          # path -> text (post-override)
        self.inventory = inventory
        self.overrides = overrides
        self.checked = []         # human-readable list of what was looked at

    def text(self, rel):
        return self.docs.get(rel)

    def claim_lines(self):
        """Every (doc, line, text) a command rule should read.

        Fenced command blocks contribute their lines; a fenced block whose
        preceding lines carry a declared-gap marker is excluded (and the marker
        is verified to still be there). Inline backticked tokens contribute the
        token alone, which is how the runbooks cite a single command in prose.
        """
        out = []
        for rel in self.corpus:
            text = self.text(rel)
            if text is None:
                continue
            out.extend(_fenced_lines(ctx=self, rel=rel, text=text))
        return out


def _fenced_lines(ctx, rel, text):
    lines = text.splitlines()
    out = []
    fence = None
    block_start = None
    gap_ids = {g["doc"]: g for g in ctx.inventory.get("declared_gaps", [])}
    for i, raw in enumerate(lines, start=1):
        m = re.match(r"^\s*```(\w*)\s*$", raw)
        if m:
            if fence is None:
                fence = m.group(1).lower()
                block_start = i
            else:
                fence = None
                block_start = None
            continue
        if fence is None or fence not in COMMAND_FENCES:
            continue
        # A declared-gap marker must appear in the document; the check lives in
        # rule D6 so the finding is reported once, with the document's name.
        gap = gap_ids.get(rel)
        if gap and gap.get("scope", "next-fence") == "next-fence" and gap["marker"] in text:
            # The marker is a sentence in the section that introduces the block,
            # so the exclusion is written without line numbers — and it applies
            # to the FIRST block after it, not to every block below: a document
            # that declares one aspirational list still makes ordinary claims
            # everywhere else.
            marker_line = next(
                (n for n, l in enumerate(lines, start=1) if gap["marker"] in l), 0)
            first_after = min((n for n, l in enumerate(lines, start=1)
                               if re.match(r"^\s*```(\w*)\s*$", l) and n > marker_line), default=0)
            if block_start == first_after:
                continue
        out.append(Claim(rel, i, raw))
    return out


# ---------------------------------------------------------------------------
# claim extraction
# ---------------------------------------------------------------------------

MAKE_RE = re.compile(r"(?<![\w./-])make\s+([A-Za-z][A-Za-z0-9_.-]*)")
SCRIPT_INVOKE_RE = re.compile(
    r"(?<![\w./-])(?P<interp>bash|sh|python3|python|uv|psql)\s+"
    r"(?P<path>(?:\.?\.?/)?[A-Za-z0-9_][A-Za-z0-9_./-]*\.(?:sh|py|mjs|js))")
DOT_SLASH_RE = re.compile(r"(?<![\w./-])\./(?P<path>[A-Za-z0-9_][A-Za-z0-9_./-]*\.(?:sh|py|mjs|js))")
# `go run ./dir` names a PACKAGE, so it has no extension for DOT_SLASH_RE to key
# on. Go programs are documented this way in this repository (the e2e harness,
# and ops/runbook-recovery in ops/DEV_COMMANDS.md), and without this the
# invocation resolved to nothing and the flags documented beside it went
# unchecked — `--admin-url` did, until this drill's own probe caught it.
GO_RUN_RE = re.compile(r"\bgo\s+(?:run|build)\s+(?P<path>\.?\.?/[A-Za-z0-9_][A-Za-z0-9_./-]*)")
COMPOSE_RE = re.compile(r"docker\s+compose\b(?P<rest>[^\n#]*)")
FLAG_RE = re.compile(r"(?<![\w-])(--[A-Za-z][A-Za-z0-9-]*)")
ENV_VAR_RE = re.compile(r"\b(?P<var>(?:POST|GITEA)_[A-Z0-9_]+)\b")
RDDEV_RE = re.compile(r"(?<![\w./-])rddev\s+([a-z][a-z0-9-]*)")


def line_claims(ctx):
    """The (doc, line, text, kind) triples the command rules iterate over."""
    out = []
    for c in ctx.claim_lines():
        out.append(c)
    return out


def inline_tokens(ctx):
    """Backticked inline tokens outside fences, as claims of their own.

    The runbooks cite single commands in prose ("`make migrate` is a
    prerequisite"). Those are claims about today's tree exactly like a fenced
    block is, so they are read — with the fence content removed first so a
    command inside a block is not counted twice.
    """
    out = []
    for rel in ctx.corpus:
        text = ctx.text(rel)
        if text is None:
            continue
        stripped = re.sub(r"```.*?```", lambda m: "\n" * m.group(0).count("\n"),
                          text, flags=re.DOTALL)
        for i, line in enumerate(stripped.splitlines(), start=1):
            for tok in re.findall(r"`([^`\n]+)`", line):
                out.append(Claim(rel, i, tok))
    return out


def all_command_claims(ctx):
    return line_claims(ctx) + inline_tokens(ctx)


# ---------------------------------------------------------------------------
# rules
# ---------------------------------------------------------------------------

def makefile_targets(root):
    targets = set()
    path = Path(root) / "Makefile"
    if not path.is_file():
        return targets
    for line in path.read_text(encoding="utf-8").splitlines():
        m = re.match(r"^([A-Za-z0-9_][A-Za-z0-9_.-]*)\s*:(?!=)", line)
        if m:
            targets.add(m.group(1))
    return targets


def rule_make_targets(ctx):
    findings = []
    targets = makefile_targets(ctx.root)
    if not targets:
        return [Finding("D1", "no Makefile targets could be read", str(ctx.root))]
    seen = 0
    for c in all_command_claims(ctx):
        for name in MAKE_RE.findall(c.text):
            seen += 1
            if name not in targets:
                findings.append(Finding(
                    "D1", "%s: `make %s` — no such target in the Makefile" % (c.where(), name),
                    "line: %s" % c.text.strip()))
    ctx.checked.append("D1: %d make invocation(s) resolved against %d targets" % (seen, len(targets)))
    return findings


def _script_path(root, doc, raw):
    """Resolve a path named in a document the way an operator's shell would.

    A directory holding a `main.go` resolves to that file: `go run ./ops/…`
    names a package, not a file, and this repository already documents Go
    programs that way (tests/e2e-pr-flows/harness before this). Without this
    the invocation would resolve to a directory, be skipped as "not a file",
    and the flags the runbook gives it would never be checked — which is what
    happened to `--admin-url` until this drill caught it.
    """
    p = raw.strip().strip("`")
    if p.startswith("./"):
        p = p[2:]
    path = Path(root) / p
    if path.is_dir() and (path / "main.go").is_file():
        return path / "main.go", p + "/main.go"
    return path, p


INTERPRETERS = {"bash": {"sh", "bash"}, "sh": {"sh", "bash"}, "python3": {"python", "python3"},
                "python": {"python", "python3"}, "uv": {"python", "python3"}, "psql": {"sql", "sh"}}


def shebang_kind(path):
    try:
        first = path.open("rb").readline().decode("utf-8", "replace").strip()
    except OSError:
        return ""
    if not first.startswith("#!"):
        return ""
    if "python" in first:
        return "python"
    if re.search(r"\b(?:ba)?sh\b", first):
        return "sh"
    if "psql" in first:
        return "sql"
    return ""


def rule_script_claims(ctx):
    findings = []
    seen = 0
    for c in all_command_claims(ctx):
        text = c.text
        for m in SCRIPT_INVOKE_RE.finditer(text):
            seen += 1
            path, rel = _script_path(ctx.root, c.doc, m.group("path"))
            if not path.is_file():
                findings.append(Finding(
                    "D2", "%s: `%s %s` — no such file" % (c.where(), m.group("interp"), rel),
                    "line: %s" % text.strip()))
                continue
            kind = shebang_kind(path)
            if kind and kind not in INTERPRETERS[m.group("interp")]:
                findings.append(Finding(
                    "D2", "%s: `%s %s` — the file's shebang says %s" %
                    (c.where(), m.group("interp"), rel, kind),
                    "line: %s" % text.strip()))
        for m in DOT_SLASH_RE.finditer(text):
            seen += 1
            path, rel = _script_path(ctx.root, c.doc, m.group("path"))
            if not path.is_file():
                findings.append(Finding(
                    "D2", "%s: `./%s` — no such file" % (c.where(), rel),
                    "line: %s" % text.strip()))
            elif not path.stat().st_mode & 0o111:
                findings.append(Finding(
                    "D2", "%s: `./%s` is not executable (see the mode)" % (c.where(), rel)))
        for m in GO_RUN_RE.finditer(text):
            seen += 1
            # Resolved against the tree directly rather than through
            # _script_path, which maps a package directory to its main.go: the
            # finding here is about the PACKAGE the runbook names, so a
            # directory without a main.go must be reported as such.
            pkg = m.group("path").lstrip("./")
            base = Path(ctx.root) / pkg
            if base.is_dir():
                if not (base / "main.go").is_file():
                    findings.append(Finding(
                        "D2", "%s: `go run %s` — the package has no main.go" % (c.where(), pkg),
                        "line: %s" % text.strip()))
            elif not base.is_file():
                findings.append(Finding(
                    "D2", "%s: `go run %s` — no such file or package" % (c.where(), pkg),
                    "line: %s" % text.strip()))
    ctx.checked.append("D2: %d script invocation(s) resolved against the tree" % seen)
    return findings


def declared_flags(script):
    """The flags a script says it accepts, read from the script itself.

    A shell script's usage heredoc, a Python script's argparse declarations and
    a Go program's flag declarations are the three shapes the corpus's commands
    use; anything else is reported as unreadable rather than assumed to accept
    whatever the runbook says it does.
    """
    text = script.read_text(encoding="utf-8", errors="replace")
    flags = set()
    if script.suffix == ".py":
        for m in re.finditer(r"add_argument\(\s*[\"'](--[A-Za-z0-9-]+)", text):
            flags.add(m.group(1))
    if script.suffix == ".go":
        # flag.String("admin-url", …) etc. The leading dash in the source is not
        # part of the name, so it is added here to put Go declarations in the
        # same namespace as the other two shapes.
        for m in re.finditer(r"flag\.\w+\(\s*[\"']([A-Za-z0-9-]+)[\"']", text):
            flags.add("--" + m.group(1))
    m = re.search(r"^usage\(\)\s*\{(?P<body>.*?)^EOF\s*$", text, flags=re.MULTILINE | re.DOTALL)
    if m:
        flags.update(FLAG_RE.findall(m.group("body")))
    if not flags:
        m = re.search(r"^(?:const\s+\w*[Uu]sage\s*=\s*)?`(?P<body>[^`]*--[^`]*)`", text, re.MULTILINE | re.DOTALL)
        if m:
            flags.update(FLAG_RE.findall(m.group("body")))
    return flags, bool(flags)


def rule_script_flags(ctx):
    findings = []
    seen = 0
    for c in all_command_claims(ctx):
        invocations = []
        for m in SCRIPT_INVOKE_RE.finditer(c.text):
            invocations.append((m.group("interp"), m.group("path")))
        for m in DOT_SLASH_RE.finditer(c.text):
            invocations.append(("bash", m.group("path")))
        for m in GO_RUN_RE.finditer(c.text):
            invocations.append(("go run", m.group("path")))
        for interp, raw in invocations:
            path, rel = _script_path(ctx.root, c.doc, raw)
            if not path.is_file():
                continue  # D2 reports the missing file; flags are not the finding
            flags = FLAG_RE.findall(c.text)
            if not flags:
                continue
            declared, readable = declared_flags(path)
            if not readable:
                findings.append(Finding(
                    "D3", "%s: no usage text could be read from %s, so the flags it is "
                         "invoked with cannot be checked" % (c.where(), rel),
                    "flags claimed: %s" % " ".join(sorted(set(flags)))))
                continue
            for flag in sorted(set(flags)):
                seen += 1
                if flag not in declared:
                    findings.append(Finding(
                        "D3", "%s: `%s %s` — %s does not declare %s" %
                        (c.where(), interp, rel, rel, flag),
                        "the flags it declares: %s" % " ".join(sorted(declared))))
    ctx.checked.append("D3: %d documented flag(s) resolved against their script's usage" % seen)
    return findings


def read_compose(root, rel):
    import yaml  # required input; the drill checks for pyyaml before this runs
    with (Path(root) / rel).open(encoding="utf-8") as fh:
        return yaml.safe_load(fh) or {}


def rule_compose_claims(ctx):
    findings = []
    cache = {}
    seen = 0

    def load(rel):
        if rel not in cache:
            try:
                cache[rel] = read_compose(ctx.root, rel)
            except Exception as exc:  # unreadable YAML is a finding, not a crash
                cache[rel] = {"__error__": str(exc)}
        return cache[rel]

    for c in all_command_claims(ctx):
        for m in COMPOSE_RE.finditer(c.text):
            rest = m.group("rest").strip()
            if not rest:
                continue
            seen += 1
            toks = rest.split()
            rel = None
            profiles = []
            sub = None
            services = []
            i = 0
            while i < len(toks):
                t = toks[i]
                if t in ("-f", "--file"):
                    rel = toks[i + 1] if i + 1 < len(toks) else ""
                    i += 2
                    continue
                if t == "--profile":
                    if i + 1 < len(toks):
                        profiles.append(toks[i + 1])
                    i += 2
                    continue
                if t.startswith("-"):
                    i += 2 if (i + 1 < len(toks) and not toks[i + 1].startswith("-")
                               and t in ("-p", "--project-name", "--env-file")) else 1
                    continue
                if sub is None:
                    sub = t
                else:
                    services.append(t.rstrip("\\"))
                i += 1
            if sub is not None and sub not in COMPOSE_SUBCOMMANDS:
                findings.append(Finding(
                    "D4", "%s: `docker compose %s …` — not a subcommand this rule knows" %
                    (c.where(), sub), "line: %s" % c.text.strip()))
            if rel is None:
                continue
            rel = rel.strip("`'\"")
            if not (ctx.root / rel).is_file():
                findings.append(Finding(
                    "D4", "%s: `-f %s` — no such compose file" % (c.where(), rel),
                    "line: %s" % c.text.strip()))
                continue
            doc = load(rel)
            if "__error__" in doc:
                findings.append(Finding("D4", "%s: %s does not parse: %s" %
                                        (c.where(), rel, doc["__error__"])))
                continue
            defined = set(doc.get("services") or {})
            for name in services:
                if name in ("\\", "|", ">", "&&", ";"):
                    continue
                if name not in defined:
                    findings.append(Finding(
                        "D4", "%s: `docker compose -f %s … %s` — %s has no service %r" %
                        (c.where(), rel, name, rel, name), "line: %s" % c.text.strip()))
            for prof in profiles:
                if prof not in services_with_profile(doc):
                    findings.append(Finding(
                        "D4", "%s: `--profile %s` — %s declares no such profile" %
                        (c.where(), prof, rel), "line: %s" % c.text.strip()))
    ctx.checked.append("D4: %d compose invocation(s) resolved against their files" % seen)
    return findings


def services_with_profile(doc):
    profs = set()
    for svc in (doc.get("services") or {}).values():
        for p in (svc or {}).get("profiles") or []:
            profs.add(p)
    return profs


def rule_env_vars(ctx):
    findings = []
    defined = set()
    example = ctx.root / "ops/deploy/staging.env.example"
    for src in ("ops/deploy/staging.env.example", ".env.example"):
        p = ctx.root / src
        if p.is_file():
            defined.update(re.findall(r"^([A-Z][A-Z0-9_]*)=", p.read_text(encoding="utf-8"), re.MULTILINE))
    compose = ctx.root / "ops/deploy/docker-compose.staging.yml"
    if compose.is_file():
        defined.update(re.findall(r"\$\{([A-Z][A-Z0-9_]*)", compose.read_text(encoding="utf-8")))
    defined.update(ENV_VAR_ALLOWLIST)
    seen = 0
    for c in all_command_claims(ctx):
        if c.doc not in ("ops/deploy/README.md", "docs/35_DEPLOYMENT_RUNBOOK.md"):
            continue
        for var in ENV_VAR_RE.findall(c.text):
            seen += 1
            if var not in defined:
                findings.append(Finding(
                    "D5", "%s: %s is named but is defined in neither %s nor the compose file" %
                    (c.where(), var, example.relative_to(ctx.root)), "line: %s" % c.text.strip()))
    ctx.checked.append("D5: %d deployment variable name(s) resolved against %d defined" % (seen, len(defined)))
    return findings


def section_anchors(ctx, doc_rel, pattern):
    text = ctx.text(doc_rel)
    if text is None:
        return None
    rx = re.compile(pattern)
    out = []
    for line in text.splitlines():
        if rx.search(line):
            out.append(line.strip())
    return out


def rule_inventory(ctx):
    findings = []
    inv = ctx.inventory
    if not inv:
        return [Finding("D6", "the inventory could not be read", "ops/runbook-steps.json")]
    steps = inv.get("steps") or []
    docs = {d["path"]: d for d in inv.get("docs") or []}
    for doc_rel, spec in docs.items():
        anchors = section_anchors(ctx, doc_rel, spec["sections"])
        if anchors is None:
            findings.append(Finding("D6", "document in the corpus is missing: %s" % doc_rel))
            continue
        covered = [s for s in steps if s["doc"] == doc_rel]
        covered_anchors = [s["anchor"] for s in covered]
        # `containers` are the document's structural headings: a heading that
        # groups steps rather than being one. They are declared, and the
        # declaration is checked in both directions — a container that has
        # disappeared from the document, and a container that is also
        # classified as a step, are both findings. Without this the section
        # pattern would have to be narrow enough to skip the containers, and
        # anything that matched neither the narrow pattern nor a step anchor
        # could then be added to the runbook invisibly.
        containers = list(spec.get("containers") or [])
        for c in containers:
            if c in covered_anchors:
                findings.append(Finding(
                    "D6", "%s: %r is declared a container and also classified as a step" % (doc_rel, c)))
            elif c not in anchors:
                findings.append(Finding(
                    "D6", "%s: ops/runbook-steps.json calls %r a container, but the document has no such "
                         "section — the inventory has rotted" % (doc_rel, c)))
        missing = [a for a in anchors if a not in covered_anchors and a not in containers]
        extra = [a for a in covered_anchors if a not in anchors]
        for a in missing:
            findings.append(Finding(
                "D6", "%s: section %r is not classified in ops/runbook-steps.json" % (doc_rel, a),
                "every section of a runbook is either mechanised or explicitly not; "
                "an unclassified one is a step nobody has said how it is performed"))
        for a in extra:
            findings.append(Finding(
                "D6", "%s: ops/runbook-steps.json classifies %r, which is not a section of the document" %
                (doc_rel, a)))
    for s in steps:
        body = ctx.text(s["doc"])
        if body is None:
            findings.append(Finding("D6", "%s: inventory step %s names a document that is not in the corpus" %
                                    (s["id"], s["doc"])))
            continue
        if s["anchor"] not in body:
            findings.append(Finding(
                "D6", "%s: inventory anchor %r is no longer verbatim in %s" % (s["id"], s["anchor"], s["doc"]),
                "the inventory has rotted; re-anchor it or the step has been rewritten"))
        if s["kind"] in ("mechanised", "named-unavailable"):
            if not s.get("command"):
                findings.append(Finding(
                    "D6", "%s: classified %s with no command" % (s["id"], s["kind"])))
            else:
                if not _command_resolves(ctx, s["command"]):
                    findings.append(Finding(
                        "D6", "%s: classified %s, but %r does not resolve in this tree" %
                        (s["id"], s["kind"], s["command"])))
                elif not any(s["command"] in (ctx.text(c) or "") for c in ctx.corpus):
                    findings.append(Finding(
                        "D6", "%s: classified %s by %r, which no corpus document names" %
                        (s["id"], s["kind"], s["command"]),
                        "a mechanism the runbook does not name is a mechanism the operator cannot find"))
            if s["kind"] == "named-unavailable":
                blocker = s.get("blocked_by")
                ids = {c["id"] for c in (ctx.inventory.get("tree_claims") or [])}
                if not blocker or blocker not in ids:
                    findings.append(Finding(
                        "D6", "%s: classified named-unavailable but blocked_by %r is not a tree claim "
                             "in the inventory — the reason it cannot run yet is asserted, not checked" %
                        (s["id"], blocker)))
                else:
                    claim = next(c for c in ctx.inventory["tree_claims"] if c["id"] == blocker)
                    if _claim_holds(ctx, claim) is False:
                        findings.append(Finding(
                            "D6", "%s: classified named-unavailable because of %r, but that claim no "
                                 "longer holds — reclassify the step" % (s["id"], blocker)))
        elif s["kind"] in ("manual", "provider", "absent"):
            if s.get("command"):
                findings.append(Finding(
                    "D6", "%s: classified %s but carries a command (%r); either the step is "
                         "mechanised or the command is not one" % (s["id"], s["kind"], s["command"])))
            if not s.get("note"):
                findings.append(Finding("D6", "%s: classified %s with no note explaining how it is "
                                        "performed" % (s["id"], s["kind"])))
        else:
            findings.append(Finding("D6", "%s: unknown kind %r" % (s["id"], s["kind"])))
    for gap in inv.get("declared_gaps") or []:
        if gap["marker"] not in (ctx.text(gap["doc"]) or ""):
            findings.append(Finding(
                "D6", "%s: the declared-gap marker %r is gone — the block it excused is no longer excused" %
                (gap["doc"], gap["marker"]),
                "reason on record: %s" % gap.get("reason", "")))
    counts = {}
    for s in steps:
        counts[s["kind"]] = counts.get(s["kind"], 0) + 1
    ctx.checked.append("D6: %d inventory step(s) (%s)" %
                       (len(steps), ", ".join("%s=%d" % kv for kv in sorted(counts.items()))))
    return findings


def _command_resolves(ctx, command):
    """Does an inventory `command` name something this tree can actually run?"""
    command = command.strip()
    m = re.match(r"^make\s+([A-Za-z0-9_.-]+)$", command)
    if m:
        return m.group(1) in makefile_targets(ctx.root)
    m = re.match(r"^(?:bash|sh|python3|\./)\s*([A-Za-z0-9_./-]+\.(?:sh|py))", command)
    if m:
        rel = m.group(1)
        if rel.startswith("./"):
            rel = rel[2:]
        return (ctx.root / rel).is_file()
    m = re.match(r"^docker\s+compose\s+", command)
    if m:
        files = re.findall(r"-f\s+([A-Za-z0-9_./-]+\.ya?ml)", command)
        return all((ctx.root / f).is_file() for f in files) and bool(files)
    m = re.match(r"^curl\b[^\n]*?https?://[^/\s'\"]+(?P<path>/[^\s'\"]*)", command)
    if m:
        return _path_is_served(ctx, m.group("path"))
    return False


def _path_is_served(ctx, path):
    """Is this URL path something the tree actually serves?

    A smoke step that curls an endpoint nobody mounts is a step that fails at
    the worst moment, so the path is resolved against the route registrations:
    the web root is the web app's own page, everything else has to appear in
    the API's mux.
    """
    if path in ("/", ""):
        return (ctx.root / "apps/web").is_dir()
    hits = _grep(ctx.root, re.escape('"%s"' % path), ["cmd/api", "cmd/mcp-server"])
    return bool(hits)


def rule_rddev_subcommands(ctx):
    main = ctx.root / "cmd/rddev/main.go"
    if not main.is_file():
        return [Finding("D7", "cmd/rddev/main.go is missing, so no rddev subcommand can be checked")]
    text = main.read_text(encoding="utf-8")
    usage = re.findall(r"rddev\s+([a-z][a-z0-9-]*)\b", text)
    known = set(usage)
    findings = []
    seen = 0
    for c in line_claims(ctx):
        for sub in RDDEV_RE.findall(c.text):
            seen += 1
            if sub not in known:
                findings.append(Finding(
                    "D7", "%s: `rddev %s` — cmd/rddev/main.go documents no such subcommand" %
                    (c.where(), sub), "line: %s" % c.text.strip()))
    ctx.checked.append("D7: %d rddev subcommand(s) resolved against %d documented" % (seen, len(known)))
    return findings


def rule_tree_claims(ctx):
    findings = []
    claims = (ctx.inventory or {}).get("tree_claims") or []
    if not claims:
        return [Finding("T1", "no tree claims to check", "ops/runbook-steps.json § tree_claims")]
    for cl in claims:
        body = ctx.text(cl["doc"]) or ""
        if body and cl["anchor"] not in body:
            findings.append(Finding(
                "T1", "%s: the claim %r is no longer in the document" % (cl["id"], cl["anchor"])))
            continue
        check = cl.get("check") or {}
        kind = check.get("kind")
        holds = _claim_holds(ctx, cl)
        if holds is None:
            findings.append(Finding("T1", "%s: unknown check kind %r" % (cl["id"], kind)))
        elif not holds:
            findings.append(Finding(
                "T1", "%s: %s no longer holds against the tree%s" %
                (cl["id"], cl["doc"], ": " + _claim_evidence(ctx, cl) if _claim_evidence(ctx, cl) else "")))
    ctx.checked.append("T1: %d claim(s) re-derived from the tree" % len(claims))
    return findings


def _claim_holds(ctx, claim):
    """Re-derive a tree claim. True when what the document says is still true.

    The single place the three check kinds are interpreted, because both T1
    (reporting the claim) and D6 (reasoning about the claim that blocks a step)
    have to agree — and an `empty-find` claim that reads "holds when the find
    returns nothing" is exactly the sort of thing to get backwards in one copy.
    """
    check = claim.get("check") or {}
    kind = check.get("kind")
    if kind == "empty-find":
        return not _find(ctx.root, check["glob"])
    if kind == "absent":
        return not _grep(ctx.root, check["pattern"], check["paths"])
    if kind == "present":
        return (ctx.root / check["path"]).exists()
    return None


def _claim_evidence(ctx, claim):
    check = claim.get("check") or {}
    kind = check.get("kind")
    if kind == "empty-find":
        return ", ".join(_find(ctx.root, check["glob"])[:5])
    if kind == "absent":
        return "; ".join(_grep(ctx.root, check["pattern"], check["paths"])[:3])
    if kind == "present":
        return "%s does not exist" % check["path"]
    return ""


def _find(root, glob):
    out = []
    for p in sorted(Path(root).rglob(glob)):
        rel = p.relative_to(root).as_posix()
        # Worker worktrees are runtime state, not the repository under review.
        if rel.startswith(".rddev/") or rel.startswith(".git/"):
            continue
        out.append(rel)
    return out


def _grep(root, pattern, paths):
    rx = re.compile(pattern)
    hits = []
    for rel in paths:
        p = Path(root) / rel
        files = [p] if p.is_file() else sorted(x for x in p.rglob("*") if x.is_file())
        for f in files:
            try:
                for n, line in enumerate(f.read_text(encoding="utf-8", errors="replace").splitlines(), start=1):
                    if rx.search(line):
                        hits.append("%s:%d: %s" % (f.relative_to(root).as_posix(), n, line.strip()[:100]))
            except OSError:
                continue
    return hits


# The goose down API specifically. `tx.Rollback` is a transaction rollback —
# the thing every failed write needs — and matching it would make this rule
# report the opposite of what it means.
DOWN_PATH_PATTERNS = [r"\.Down\(", r"\.DownTo\(", r"\.DownContext\(", r"\.DownToContext\("]


def rule_forward_only(ctx):
    findings = []
    runner = ctx.root / "internal/persistence"
    hits = _grep(ctx.root, "|".join(DOWN_PATH_PATTERNS), ["internal/persistence"])
    if hits:
        findings.append(Finding(
            "T2", "the migration runner has a down path, so the database is not forward-only",
            "\n".join(hits[:5])))
    down_files = _find(ctx.root, "*.down.sql")
    if down_files:
        findings.append(Finding("T2", "down migrations exist in the tree: %s" % ", ".join(down_files[:5])))
    usage = ctx.root / "cmd/rddev/db.go"
    if usage.is_file():
        text = usage.read_text(encoding="utf-8")
        if re.search(r"rddev db (down|rollback|undo)", text):
            findings.append(Finding("T2", "the rddev db surface advertises a down/rollback subcommand"))
    compose = ctx.root / "ops/deploy/docker-compose.staging.yml"
    if compose.is_file():
        doc = read_compose(ctx.root, "ops/deploy/docker-compose.staging.yml")
        migrate = (doc.get("services") or {}).get("migrate") or {}
        if str(migrate.get("restart", "")).strip('"') != "no":
            findings.append(Finding(
                "T2", "the compose migrate job does not set restart: \"no\" "
                      "(a failing migration would be retried against a half-applied schema)",
                "migrate.restart = %r" % migrate.get("restart")))
    ctx.checked.append("T2: forward-only re-derived (runner down-path, down files, CLI surface, migrate restart)")
    return findings


RULES = [
    ("D1 make-targets", rule_make_targets),
    ("D2 script-claims", rule_script_claims),
    ("D3 script-flags", rule_script_flags),
    ("D4 compose-claims", rule_compose_claims),
    ("D5 env-vars", rule_env_vars),
    ("D6 inventory", rule_inventory),
    ("D7 rddev-subcommands", rule_rddev_subcommands),
    ("T1 tree-claims", rule_tree_claims),
    ("T2 forward-only", rule_forward_only),
]

# What each rule asserts, for `--list-checks`. Keyed by the rule's short code and
# asserted against RULES by build_check_list below, so a rule added without a
# purpose here is an error rather than a silent omission.
CHECK_PURPOSE = {
    "D1": "every `make <target>` the corpus names is a target of the Makefile",
    "D2": "every script/executable the corpus invokes exists, and its shebang agrees with the "
          "interpreter the corpus claims",
    "D3": "every `--flag` the corpus documents appears in that script's own usage text",
    "D4": "every `docker compose -f FILE` the corpus gives names a file that exists, whose "
          "services and profiles it uses, with a subcommand Compose has",
    "D5": "every POST_*/GITEA_* deployment variable the corpus names is defined by the "
          "deployment template",
    "D6": "every section of docs/35, docs/36 and docs/37 is classified in the inventory, its "
          "anchor is verbatim, and its class agrees with the tree",
    "D7": "every `rddev <subcommand>` the corpus documents is one the CLI implements",
    "T1": "every claim the corpus makes about what this tree contains or lacks is re-derived "
          "from the tree",
    "T2": "the database really is forward-only: no down path in the runner, no down migration "
          "files, no `rddev db down`, and the migrate job pinned to restart: \"no\"",
}


def build_check_list():
    """The rules as (name, purpose) pairs. Raises if the two ever disagree."""
    out = []
    for name, _fn in RULES:
        code = name.split()[0]
        if code not in CHECK_PURPOSE:
            raise SystemExit("error: rule %r has no purpose in CHECK_PURPOSE" % name)
        out.append((name, CHECK_PURPOSE[code]))
    for code in CHECK_PURPOSE:
        if code not in {n.split()[0] for n, _ in RULES}:
            raise SystemExit("error: CHECK_PURPOSE describes %r, which is not a rule" % code)
    return out


# ---------------------------------------------------------------------------
# selftest — one mutation per rule, applied to a copy
# ---------------------------------------------------------------------------

def _sub(text, old, new, count=1):
    if old not in text:
        raise KeyError(old)
    return text.replace(old, new, count)


def mutations(root, corpus):
    """(rule, applies-to, mutation) — each must make its own rule fail."""
    dev = "ops/DEV_COMMANDS.md"
    deploy_readme = "ops/deploy/README.md"
    return [
        ("D1 make-targets", dev,
         # The mutation has to land on an ENFORCED claim: the first occurrence
         # of `make test-integration` in this document is inside the block the
         # declared gap excuses, so mutating that one would prove nothing.
         lambda t: _sub(t, "make test-integration # 真实 PostgreSQL",
                        "make test-integratio # 真实 PostgreSQL")),
        ("D2 script-claims", dev,
         lambda t: _sub(t, "./ops/backup-restore-drill.sh", "./ops/backup-restore-dril.sh")),
        ("D3 script-flags", dev,
         lambda t: (_sub(t, "--no-infra        # 栈已经起好并迁到 head 时用这个",
                         "--no-infra --no-database-at-all        # 栈已经起好并迁到 head 时用这个")
                    if "--no-infra        #" in t else
                    _sub(t, "./ops/backup-restore-drill.sh --no-infra",
                         "./ops/backup-restore-drill.sh --no-infra --no-database-at-all"))),
        ("D4 compose-claims", deploy_readme,
         lambda t: _sub(t, "up -d --wait api worker mcp adapter", "up -d --wait api workre mcp adapter")),
        ("D5 env-vars", deploy_readme,
         lambda t: _sub(t, "POST_TLS_DIR", "POST_TLS_DIRECTORY")),
        ("D6 inventory", "ops/runbook-steps.json",
         lambda t: _sub(t, "1. 备份/确认 DB。", "1. 备份/确认数据库。")),
        ("D7 rddev-subcommands", dev,
         lambda t: _sub(t, "setsid rddev drive --parallel 2", "setsid rddev drve --parallel 2")),
        ("T1 tree-claims", "ops/runbook-steps.json",
         lambda t: _sub(t, '"glob": "Dockerfile*"', '"glob": "Makefile*"')),
        # T2 reads the TREE, not a document, so its mutation is a fixture tree
        # rather than a mutated page: a tree whose migration runner calls
        # goose's down API must be rejected. A rule about the tree is only
        # proven able to fail by showing it a tree that is wrong.
        ("T2 forward-only", FIXTURE, ("down-path", {
            "internal/persistence/migrate.go":
                "package persistence\n\nfunc rollback(p *provider) { _, _ = p.Down(ctx) }\n",
            "cmd/rddev/db.go": "package main\n\nconst dbUsage = \"rddev db migrate\"\n",
            "ops/deploy/docker-compose.staging.yml":
                "services:\n  migrate:\n    restart: \"no\"\n",
        })),
    ]


# A mutation that builds a synthetic tree instead of editing a document.
FIXTURE = "\x00fixture"


def build_fixture(tmpdir, files):
    for rel, body in files.items():
        p = Path(tmpdir) / rel
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_text(body, encoding="utf-8")
    return Path(tmpdir)


def run_selftest(root, corpus, inventory, overrides, out):
    """Apply each mutation on its own and require its own rule to complain."""
    failures = 0
    passed = 0
    for rule_name, victim, mutate in mutations(root, corpus):
        docs = load_docs(root, corpus, overrides)
        inv_text = inventory
        try:
            if victim == FIXTURE:
                name, files = mutate
                tmp = tempfile.mkdtemp(prefix="runbook-verify-fixture-")
                try:
                    fixture_root = build_fixture(tmp, files)
                    ctx = Ctx(fixture_root, [], {}, json.loads(inv_text), {})
                    rule_id = rule_name.split()[0]
                    fired = [f for rid, fn in RULES if rid.split()[0] == rule_id for f in fn(ctx)]
                    if fired:
                        passed += 1
                        print("ok   selftest %s: fixture tree rejected (%s)" %
                              (rule_name, fired[0].message), file=out)
                    else:
                        failures += 1
                        print("FAIL selftest %s: the fixture tree (%s) was accepted — this rule "
                              "cannot fail" % (rule_name, name), file=out)
                finally:
                    shutil.rmtree(tmp, ignore_errors=True)
                continue
            if victim == "ops/runbook-steps.json":
                inv_text = mutate(inv_text)
                inv = json.loads(inv_text)
                mutated_docs = docs
            else:
                docs = dict(docs)
                docs[victim] = mutate(docs[victim])
                inv = json.loads(inv_text)
        except KeyError as exc:
            print("FAIL selftest %s: the mutation anchor %s is gone — this check has rotted"
                  % (rule_name, exc), file=out)
            failures += 1
            continue
        except Exception as exc:  # noqa: BLE001 - a broken mutation is a test failure
            print("FAIL selftest %s: the mutation could not be applied: %s" % (rule_name, exc), file=out)
            failures += 1
            continue
        ctx = Ctx(root, corpus, docs, inv, overrides)
        rule_id = rule_name.split()[0]
        fired = []
        for rid, fn in RULES:
            if rid.split()[0] == rule_id:
                fired = fn(ctx)
        if fired:
            passed += 1
            print("ok   selftest %s: mutation rejected (%s)" % (rule_name, fired[0].message), file=out)
        else:
            failures += 1
            print("FAIL selftest %s: mutation survived — this rule cannot fail" % rule_name, file=out)
    print("selftest: %d mutation(s) rejected, %d survived" % (passed, failures), file=out)
    return failures


# ---------------------------------------------------------------------------
# driver
# ---------------------------------------------------------------------------

def load_docs(root, corpus, overrides):
    docs = {}
    for rel in corpus:
        src = overrides.get(rel, rel)
        p = Path(root) / src
        docs[rel] = p.read_text(encoding="utf-8") if p.is_file() else None
    return docs


def parse_args(argv):
    p = argparse.ArgumentParser(
        prog="ops/runbook-verify.py",
        description="Verify that the POST runbooks still describe the commands this tree has "
                    "(task T1204). Findings are printed one per line; none means the documents "
                    "and the tree agree about what this rule set looks at.")
    p.add_argument("--root", default=None, help="repository root (default: parent of ops/)")
    p.add_argument("--json", action="store_true", help="print findings as JSON")
    p.add_argument("--selftest", action="store_true",
                   help="prove each rule can fail: one mutation per rule, applied to a temporary copy")
    p.add_argument("--override", action="append", default=[], metavar="DOC=FILE",
                   help="read the corpus document DOC from FILE instead (used by the drill's "
                        "own mutation check); repeatable")
    p.add_argument("--list-checks", action="store_true",
                   help="print what each rule asserts and exit 0 WITHOUT verifying anything "
                        "(not a way to pass the gate: no rule runs)")
    return p.parse_args(argv)


def main(argv=None):
    args = parse_args(argv)
    if args.list_checks:
        # Answered without reading the tree, and it exits 0 regardless of what
        # the tree looks like: this is a list of questions, not an answer to
        # them. It is deliberately NOT a verification mode — a `--list-checks`
        # run that returned 0 on a red tree would be a way to make the gate
        # green without checking anything.
        print("ops/runbook-verify.py checks %d rules; this listing verifies nothing:" % len(RULES))
        for name, purpose in build_check_list():
            print("  %-22s %s" % (name, purpose))
        print("run this command with no arguments to run them")
        return 0
    root = Path(args.root) if args.root else Path(__file__).resolve().parent.parent
    if not (root / "ops/runbook-steps.json").is_file():
        print("error: ops/runbook-steps.json not found under %s" % root, file=sys.stderr)
        return MISSING_INPUT
    inventory_text = (root / "ops/runbook-steps.json").read_text(encoding="utf-8")
    inventory = json.loads(inventory_text)
    corpus = list(inventory.get("corpus") or [])
    if not corpus:
        print("error: the inventory names no documents", file=sys.stderr)
        return MISSING_INPUT
    overrides = {}
    for spec in args.override:
        if "=" not in spec:
            print("error: --override needs DOC=FILE, got %r" % spec, file=sys.stderr)
            return USAGE_ERROR
        doc, src = spec.split("=", 1)
        overrides[doc] = src
    docs = load_docs(root, corpus, overrides)
    for rel in corpus:
        if docs[rel] is None:
            print("error: corpus document missing: %s" % rel, file=sys.stderr)
            return MISSING_INPUT

    if args.selftest:
        failures = run_selftest(root, corpus, inventory_text, overrides, sys.stdout)
        return 1 if failures else 0

    ctx = Ctx(root, corpus, docs, inventory, overrides)
    findings = []
    for name, fn in RULES:
        try:
            findings.extend(fn(ctx))
        except Exception as exc:  # noqa: BLE001 - a rule that crashes is a finding
            findings.append(Finding(name.split()[0], "rule crashed: %s: %s" % (type(exc).__name__, exc)))

    if args.json:
        print(json.dumps({
            "corpus": corpus,
            "checks": ctx.checked,
            "findings": [{"rule": f.rule, "message": f.message, "evidence": f.evidence} for f in findings],
        }, ensure_ascii=False, indent=2))
    else:
        print("corpus: %s" % ", ".join(corpus))
        for line in ctx.checked:
            print("ok   %s" % line)
        for f in findings:
            print("FAIL %s" % f)
        print("runbook-verify: %d finding(s)" % len(findings))
    return 1 if findings else 0


if __name__ == "__main__":
    sys.exit(main())
