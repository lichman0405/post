#!/usr/bin/env python3
"""Validate the POST staging Compose template (T1203).

The staging deployment template is three files that have to agree with each
other and with the specifications:

    ops/deploy/docker-compose.staging.yml
    ops/deploy/staging.env.example
    ops/deploy/reverse-proxy/nginx.conf

Agreement is the part that rots. A compose file that mounts the TLS
directory at one path while nginx reads its certificate from another starts
without a complaint and serves nothing; a service that loses its healthcheck
is silently never waited for; a `${VAR:?}` whose key is missing from the
example turns into a deployment failure in front of the operator instead of a
review comment. Each of those is a rule below.

WHY A RULE IS TRUSTWORTHY HERE: `--selftest` mutates a copy of the real
inputs, once per rule, and requires the rule to fire on its own mutation —
asserting the rule's id appears in the findings, not merely that "something
failed", so a rule that has been deleted or made unreachable fails the
selftest instead of quietly passing. A validator without a demonstrated
failing mode is a validator that only ever says ok.

Usage
    python3 ops/deploy/validate-staging-compose.py            # report
    python3 ops/deploy/validate-staging-compose.py --json     # machine-readable
    python3 ops/deploy/validate-staging-compose.py --selftest # prove each rule bites
    python3 ops/deploy/validate-staging-compose.py --with-docker
        also run `docker compose config` on the template, if the docker CLI
        can read it. Optional on purpose: the Compose binary is a third-party
        parser whose presence varies by host, and a gate whose verdict depends
        on whether a tool happens to be installed is a flaky gate. The rules
        above are the gate; this is extra evidence when it is available.

Exit codes
    0  every rule passed (and, with --selftest, every rule demonstrated failing)
    1  at least one finding
    2  usage error
    3  an input file is missing or unreadable
"""

from __future__ import annotations

import argparse
import copy
import json
import os
import re
import shutil
import subprocess
import sys
from pathlib import Path

try:
    import yaml
except ImportError:  # pragma: no cover - environment problem, not a finding
    print("error: PyYAML is required (pip install pyyaml)", file=sys.stderr)
    sys.exit(3)

HERE = Path(__file__).resolve().parent
DEFAULT_COMPOSE = HERE / "docker-compose.staging.yml"
DEFAULT_ENV = HERE / "staging.env.example"

# The services this template is required to define, and the stage each belongs
# to (docs/35_DEPLOYMENT_RUNBOOK.md:9-11). These names are part of the contract
# with the runbook: the deploy order rules below refer to them by name.
APP_SERVICES = ("migrate", "api", "worker", "mcp", "adapter", "web")

# Secret-shaped environment keys. This mirrors the alternatives of the
# repository's own scanner (internal/config/secretscan.go:17-22 secretKeyRe),
# narrowed to what can appear as a compose environment key: the Go regex also
# matches DSNs and credential-bearing URLs, which compose keys are not. It is
# a mirror, not an import — the Go scanner is unexported and lives in
# package config — so if that list grows, this one is expected to be revisited.
SECRET_KEY_RE = re.compile(
    r"(?i)(password|passwd|pwd|secret|token|credential|auth|api[_-]?key|"
    r"access[_-]?key|private[_-]?key|_key$|^key)"
)

# A URL with userinfo (scheme://user:pass@host) — the same shape
# internal/config/secretscan.go:43 urlCredRe refuses in an example file.
URL_CRED_RE = re.compile(r"://[^/\s@]+@")

VAR_RE = re.compile(r"(?<!\$)\$\{([A-Za-z_][A-Za-z0-9_]*)(:?[-?][^}]*)?\}")
IMAGE_DIGEST_RE = re.compile(r"@sha256:[0-9a-f]{64}$")

# Compose volume entry: SOURCE:TARGET[:OPTIONS]. SOURCE may itself contain a
# colon (an interpolation like ${VAR:?reason}), so the split has to be taken
# from the right, at the last colon that introduces an absolute path.
VOLUME_RE = re.compile(r"^(.*):(/[^:]*)(?::([a-z,]+))?$")


def split_volume(entry):
    """(source, target, options) for a compose volume string, or None."""
    if not isinstance(entry, str):
        return None
    m = VOLUME_RE.match(entry.strip())
    if not m:
        return None
    return m.group(1), m.group(2), (m.group(3) or "")


def strip_comments(text):
    """Drop YAML comments.

    The variable rules scan the compose text, and this file's comments talk
    about `${VAR:?reason}` in prose — which is not a reference to anything.
    A `#` that starts a line or follows whitespace begins a comment here; none
    of the values in this template contain a `#`.
    """
    return "\n".join(re.split(r"(?:^|\s)#", line)[0] for line in text.splitlines())

# Healthcheck test forms that pass no matter what the service is doing. The
# whole point of a healthcheck is to be able to fail; one that cannot is worse
# than none, because the rest of the stack gates on it.
TRIVIAL_TESTS = {"true", ":", "exit 0", "/bin/true"}


class Inputs:
    """The three files, parsed once and handed to every rule."""

    def __init__(self, compose_path: Path, env_path: Path):
        self.compose_path = compose_path
        self.env_path = env_path
        self.compose_text = compose_path.read_text(encoding="utf-8")
        # Comments are prose about the file, not the file: `${VAR:?reason}`
        # appears in them as an explanation, and scanning them as references
        # would invent variables that the template does not use.
        self.compose_code = strip_comments(self.compose_text)
        self.env_text = env_path.read_text(encoding="utf-8")
        self.doc = yaml.safe_load(self.compose_text)
        self.services = self.doc.get("services", {}) if isinstance(self.doc, dict) else {}
        self.waivers = self.doc.get("x-post-healthcheck-waivers", {}) or {}
        self.env_active, self.env_documented, self.env_values = parse_env(self.env_text)
        self.proxy_path, self.proxy_mount, self.proxy_text = self._proxy()

    def _proxy(self):
        """Follow the proxy's mounted config — never a hardcoded path.

        The rule that checks the TLS paths must read the file the container
        actually gets, so the path is taken from the bind mount rather than
        assumed. A config move that updates one side only is then a finding.
        """
        svc = self.services.get("proxy") or {}
        for vol in svc.get("volumes", []) or []:
            parts = split_volume(vol)
            if not parts:
                continue
            source, target, _ = parts
            if target.endswith("default.conf"):
                cand = (self.compose_path.parent / source).resolve()
                if cand.is_file():
                    return cand, source, cand.read_text(encoding="utf-8")
        return None, None, ""

    def env_of(self, name: str) -> dict:
        """A service's environment, after YAML merge keys are resolved."""
        svc = self.services.get(name) or {}
        env = svc.get("environment") or {}
        return env if isinstance(env, dict) else {}

    def depends_on(self, name: str) -> dict:
        svc = self.services.get(name) or {}
        dep = svc.get("depends_on") or {}
        return dep if isinstance(dep, dict) else {}

    def profiles_of(self, name: str) -> list:
        svc = self.services.get(name) or {}
        return list(svc.get("profiles") or [])

    def required_vars(self) -> set:
        """Every `${VAR:?...}` reference — a value with no default."""
        out = set()
        for m in VAR_RE.finditer(self.compose_code):
            if m.group(2) and m.group(2).startswith(":?"):
                out.add(m.group(1))
        return out

    def all_vars(self) -> set:
        return set(m.group(1) for m in VAR_RE.finditer(self.compose_code))


def parse_env(text: str):
    """Split an env example into (active values, documented keys, any values).

    A commented line documents a key without providing it — the convention the
    example file uses for optional variables and for the bundled-infra images,
    where the point is that turning the profile on is an explicit act. A key
    that is only ever seen commented is therefore documented but not set.
    """
    active, documented, values = {}, set(), {}
    for raw in text.splitlines():
        line = raw.strip()
        if not line:
            continue
        commented = line.startswith("#")
        body = line.lstrip("#").strip() if commented else line
        if "=" not in body:
            continue
        key, _, value = body.partition("=")
        key, value = key.strip(), value.strip()
        if not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", key):
            continue
        documented.add(key)
        values.setdefault(key, value)
        if not commented:
            active[key] = value
    return active, documented, values


# ---------------------------------------------------------------------------
# Rules. Each returns a list of finding strings; empty means the rule passed.
# ---------------------------------------------------------------------------

def rule_service_images(inp):
    """R01 every service names an image."""
    out = []
    for name, svc in inp.services.items():
        if not isinstance(svc, dict) or not svc.get("image"):
            out.append(f"service {name!r} has no image")
    return out


def rule_image_is_variable(inp):
    """R02 every image is a `${VAR:?...}` reference, never a literal.

    A literal image in a committed file is a mutable tag waiting to happen:
    someone edits the tag in place, and the next deploy runs a different build
    with the same compose file. A required variable makes every deployed
    version an explicit, reviewable input.
    """
    out = []
    for name, svc in inp.services.items():
        if not isinstance(svc, dict):
            continue
        image = svc.get("image")
        if not isinstance(image, str):
            continue
        m = re.fullmatch(r"\$\{([A-Za-z_][A-Za-z0-9_]*):\?[^}]*\}", image)
        if not m:
            out.append(
                f"service {name!r} image {image!r} is not a "
                "`${VAR:?reason}` reference (docs/25_CICD_DEVOPS.md:32)"
            )
    return out


def rule_image_digests(inp):
    """R03 every image value in the example is pinned by digest.

    docs/25_CICD_DEVOPS.md:32 — immutable digests. Checked on commented lines
    too: an operator who uncomments the bundled-infra block must get a
    digest-pinned reference, not a tag.
    """
    out = []
    for key, value in sorted(inp.env_values.items()):
        if not key.endswith("_IMAGE"):
            continue
        if not IMAGE_DIGEST_RE.search(value):
            out.append(
                f"{key} is not pinned by digest ({value!r}) — "
                "expected ...@sha256:<64 hex>"
            )
    return out


def rule_required_vars_documented(inp):
    """R04 every `${VAR:?...}` has an uncommented line in the example."""
    out = []
    for var in sorted(inp.required_vars()):
        if var not in inp.env_active:
            out.append(
                f"{var} has no default in the compose file but no uncommented "
                "value in the env example: the deployment fails at "
                "interpretation time"
            )
    return out


def rule_all_vars_documented(inp):
    """R05 every variable the compose mentions is documented at all."""
    out = []
    for var in sorted(inp.all_vars()):
        if var not in inp.env_documented:
            out.append(f"{var} is referenced by the compose file and not documented in the example")
    return out


def rule_healthcheck_or_waiver(inp):
    """R06 a long-running service has a healthcheck or a written-down reason."""
    out = []
    for name, svc in inp.services.items():
        if not isinstance(svc, dict):
            continue
        if str(svc.get("restart", "")).strip('"').lower() == "no":
            continue  # one-shot job: there is nothing to be "up"
        if svc.get("healthcheck"):
            continue
        reason = inp.waivers.get(name)
        if not isinstance(reason, str) or not reason.strip():
            out.append(
                f"service {name!r} has neither a healthcheck nor an entry in "
                "x-post-healthcheck-waivers"
            )
    return out


def rule_healthcheck_complete(inp):
    """R07 a healthcheck states all five fields.

    Compose's defaults silently apply otherwise, and a check with a 30s
    interval and no start_period marks a slow-starting service unhealthy
    during a deploy that is going fine.
    """
    out = []
    for name, svc in inp.services.items():
        hc = (svc or {}).get("healthcheck")
        if not isinstance(hc, dict):
            continue
        if "test" not in hc:
            out.append(f"service {name!r} healthcheck has no test")
        for field in ("interval", "timeout", "retries", "start_period"):
            if field not in hc:
                out.append(f"service {name!r} healthcheck has no {field}")
    return out


def rule_healthcheck_honest(inp):
    """R08 a healthcheck must be able to fail, and must not accept a 503."""
    out = []
    for name, svc in inp.services.items():
        hc = (svc or {}).get("healthcheck")
        if not isinstance(hc, dict):
            continue
        test = hc.get("test")
        parts = test if isinstance(test, list) else [test]
        text = " ".join(str(p) for p in parts if p is not None)
        is_shell = any(str(p) == "CMD-SHELL" for p in parts)
        body = text.split("CMD-SHELL", 1)[-1].strip() if is_shell else text
        if body.strip() in TRIVIAL_TESTS or body.strip().rstrip(";").strip() in TRIVIAL_TESTS:
            out.append(f"service {name!r} healthcheck test {text!r} can never fail")
        if re.search(r"\|\|\s*(true|:)\s*$", body):
            out.append(f"service {name!r} healthcheck swallows its own failure: {text!r}")
        # -f, --fail, and the bundled spelling -fsS are all acceptable; what
        # is not acceptable is a curl invocation that treats HTTP 503 as
        # success, which is the whole point of probing /readyz.
        curl_fails = re.search(r"(^|\s)--fail(\s|$)", text) or re.search(
            r"(^|\s)-[A-Za-z]*f[A-Za-z]*(\s|$)", text
        )
        if "curl" in text and not curl_fails:
            out.append(
                f"service {name!r} healthcheck probes with curl but not `-f`: "
                "a 4xx/5xx answers 0 and the check passes on an unhealthy "
                "service"
            )
    return out


def rule_deploy_order(inp):
    """R09 the runbook's order is the template's depends_on (docs/35:9-11).

    migrations job -> API/worker/MCP -> Web. Written as configuration rather
    than as a paragraph, because a paragraph is not what Compose reads.
    """
    out = []
    for name in ("api", "worker", "mcp"):
        cond = (inp.depends_on(name).get("migrate") or {}).get("condition")
        if cond != "service_completed_successfully":
            out.append(
                f"{name} does not wait for the migrate job to EXIT ZERO "
                f"(condition: {cond!r}) — docs/35_DEPLOYMENT_RUNBOOK.md:9-10"
            )
    for name, target in (("web", "api"), ("proxy", "api"), ("web", "adapter")):
        dep = inp.depends_on(name).get(target) or {}
        if dep.get("condition") != "service_healthy":
            out.append(
                f"{name} does not wait for {target} to be healthy "
                f"(condition: {dep.get('condition')!r})"
            )
    return out


def rule_profile_deps_optional(inp):
    """R10 a dependency a profile might not start must be `required: false`.

    Otherwise `docker compose up` with the profile off refuses to start at
    all: the dependency names a service that is not part of the project.
    """
    out = []
    for name in inp.services:
        deps = inp.depends_on(name)
        mine = set(inp.profiles_of(name))
        for target, spec in deps.items():
            if not isinstance(spec, dict):
                continue
            theirs = set(inp.profiles_of(target))
            # `is not False` rather than a truthiness test: an absent key and
            # an explicit `required: true` are both defects here, and only an
            # explicit false is the fix.
            if theirs and theirs != mine and spec.get("required") is not False:
                out.append(
                    f"{name} depends on {target}, which only exists under "
                    f"profile(s) {sorted(theirs)}, without `required: false`"
                )
    return out


def rule_migrate_one_shot(inp):
    """R11 the migrations job is one-shot (docs/35:17)."""
    out = []
    svc = inp.services.get("migrate") or {}
    if not svc:
        return ["no `migrate` service: docs/35_DEPLOYMENT_RUNBOOK.md:9 makes it a step"]
    restart = str(svc.get("restart", "")).strip('"').lower()
    if restart != "no":
        out.append(
            f"migrate has restart {restart!r}, not 'no': a failed migration "
            "would be re-applied in a loop instead of stopping the deploy"
        )
    if not svc.get("command"):
        out.append("migrate has no command")
    return out


def rule_only_proxy_published(inp):
    """R12 the proxy is the only published surface, and it publishes both ports."""
    out = []
    for name, svc in inp.services.items():
        if name == "proxy":
            continue
        if (svc or {}).get("ports"):
            out.append(
                f"service {name!r} publishes ports {svc['ports']!r}; every "
                "service except the proxy stays on the compose network"
            )
    proxy = inp.services.get("proxy") or {}
    containers = set()
    for entry in proxy.get("ports", []) or []:
        if isinstance(entry, str) and ":" in entry:
            containers.add(entry.split(":")[-1].split("/")[0])
    for port in ("80", "443"):
        if port not in containers:
            out.append(f"proxy does not publish container port {port}")
    return out


def rule_tls_termination(inp):
    """R13 TLS is real: mounted certs, read-only, modern protocols, 80 -> 443."""
    out = []
    if inp.proxy_path is None:
        return ["the proxy's nginx config is not mounted from a file in the repository"]
    svc = inp.services.get("proxy") or {}
    tls_dir = None
    for vol in svc.get("volumes", []) or []:
        parts = split_volume(vol)
        if not parts:
            continue
        _, target, opts = parts
        if target.startswith("/etc/nginx/tls"):
            tls_dir = target
            if "ro" not in opts.split(","):
                out.append(f"the TLS mount {vol!r} is not read-only")
    if tls_dir is None:
        out.append("the proxy does not mount a TLS directory at /etc/nginx/tls")

    cert = re.search(r"^\s*ssl_certificate\s+([^;]+);", inp.proxy_text, re.M)
    key = re.search(r"^\s*ssl_certificate_key\s+([^;]+);", inp.proxy_text, re.M)
    if not cert or not key:
        out.append("the nginx config declares no ssl_certificate / ssl_certificate_key")
    elif tls_dir:
        for label, m in (("ssl_certificate", cert), ("ssl_certificate_key", key)):
            path = m.group(1).strip()
            if not path.startswith(tls_dir.rstrip("/") + "/"):
                out.append(
                    f"nginx reads {label} from {path!r}, outside the mounted "
                    f"directory {tls_dir!r}: the container would start with "
                    "no certificate"
                )

    proto = re.search(r"^\s*ssl_protocols\s+([^;]+);", inp.proxy_text, re.M)
    if not proto:
        out.append("the nginx config sets no ssl_protocols")
    else:
        named = proto.group(1).split()
        banned = [p for p in named if p in ("TLSv1", "TLSv1.1", "SSLv2", "SSLv3")]
        if banned:
            out.append(f"ssl_protocols still offers {banned}")
        if "TLSv1.3" not in named:
            out.append("ssl_protocols does not offer TLSv1.3")

    if not re.search(r"return\s+30[18]\s+https://", inp.proxy_text):
        out.append("the nginx config has no http -> https redirect on port 80")
    if not re.search(r"listen\s+(\[::\]:)?80\s*;", inp.proxy_text):
        out.append("the nginx config does not listen on port 80")
    return out


def rule_no_inline_secrets(inp):
    """R14 no secret is written into a committed file."""
    out = []
    for name, svc in inp.services.items():
        env = inp.env_of(name)
        for key, value in env.items():
            if not isinstance(value, str):
                value = "" if value is None else str(value)
            if SECRET_KEY_RE.search(str(key)) and "${" not in value:
                out.append(
                    f"service {name!r} sets secret-shaped {key} to a literal "
                    "value: secrets come from the env file, never from the "
                    "compose file"
                )
    for where, text in (("compose", inp.compose_text), ("nginx config", inp.proxy_text)):
        for i, line in enumerate(text.splitlines(), 1):
            body = line.lstrip("#").strip()
            for m in URL_CRED_RE.finditer(body):
                # `postgres://${POST_DB_USER:?}:${POST_DB_PASSWORD:?}@host` is
                # the shape this rule WANTS: the credentials are references.
                # Only a userinfo with no interpolation in it is a finding.
                if "${" not in m.group(0):
                    out.append(f"{where}:{i} contains a credential-bearing URL")
    return out


def rule_stateful_volumes(inp):
    """R15 stateful services declare named volumes, declared at the top level.

    A stateful service without a volume loses its data on
    `docker compose up -d --force-recreate` — which is a routine step of the
    runbook's rollback, not an accident.
    """
    out = []
    top = inp.doc.get("volumes") or {}
    for name in ("postgres", "redis", "minio", "gitea"):
        svc = inp.services.get(name)
        if not isinstance(svc, dict):
            continue
        found = []
        for vol in svc.get("volumes", []) or []:
            parts = split_volume(vol)
            if not parts:
                continue
            source = parts[0]
            if not source.startswith(".") and not source.startswith("/"):
                found.append(source)
        if not found:
            out.append(f"stateful service {name!r} declares no named volume")
        for source in found:
            if source not in top:
                out.append(f"{name} mounts named volume {source!r}, which is not declared top-level")
    return out


def rule_waivers_not_stale(inp):
    """R16 a waiver must name a service that is actually missing a healthcheck.

    A stale waiver is how a healthcheck gets deleted: the entry is there, so
    nobody looks, and the service stops being waited for.
    """
    out = []
    for name, reason in inp.waivers.items():
        svc = inp.services.get(name)
        if not isinstance(svc, dict):
            out.append(f"waiver names service {name!r}, which the template does not define")
            continue
        if svc.get("healthcheck"):
            out.append(f"waiver for {name!r} is stale: that service has a healthcheck")
        if not isinstance(reason, str) or not reason.strip():
            out.append(f"waiver for {name!r} states no reason")
    return out


def rule_web_origin_https(inp):
    """R17 the public origin is https (the prod layer's cookies are Secure)."""
    out = []
    origin = inp.env_values.get("POST_WEB_ORIGIN", "")
    if origin and not origin.startswith("https://"):
        out.append(
            f"POST_WEB_ORIGIN={origin!r} is not https: POST_ENV=prod sets the "
            "session cookie Secure, so a browser will not send it back over "
            "plain http (cmd/api/authhttp/auth_middleware.go:320)"
        )
    return out


def rule_app_env_layer(inp):
    """R18 every application service runs the prod configuration layer."""
    out = []
    for name in APP_SERVICES:
        if name not in inp.services:
            out.append(f"service {name!r} is not defined: docs/35:9-11 deploys it")
            continue
        env = inp.env_of(name)
        if str(env.get("POST_ENV", "")).strip() != "prod":
            out.append(
                f"service {name!r} runs POST_ENV={env.get('POST_ENV')!r}: "
                "staging deploys the production layer (the same value "
                "production does), which is the point of staging"
            )
    return out


def rule_web_runtime_config(inp):
    """R19 the web service carries the variables its own loader requires."""
    out = []
    env = inp.env_of("web")
    for key in ("API_BASE_URL", "SCIENTIFIC_ADAPTER_URL"):
        if not str(env.get(key, "")).strip():
            out.append(
                f"web has no {key}: apps/web/lib/config.ts requires it and "
                "fails the render without it"
            )
    for key in ("API_BASE_URL", "SCIENTIFIC_ADAPTER_URL"):
        value = str(env.get(key, ""))
        if value and not value.startswith("http"):
            out.append(f"web {key}={value!r} is not an absolute http(s) URL")
    return out


RULES = [
    ("R01 service-images", rule_service_images),
    ("R02 image-is-required-variable", rule_image_is_variable),
    ("R03 image-digest-pinned", rule_image_digests),
    ("R04 required-vars-have-values", rule_required_vars_documented),
    ("R05 all-vars-documented", rule_all_vars_documented),
    ("R06 healthcheck-or-waiver", rule_healthcheck_or_waiver),
    ("R07 healthcheck-complete", rule_healthcheck_complete),
    ("R08 healthcheck-no-false-green", rule_healthcheck_honest),
    ("R09 deploy-order", rule_deploy_order),
    ("R10 profile-deps-optional", rule_profile_deps_optional),
    ("R11 migrate-one-shot", rule_migrate_one_shot),
    ("R12 only-proxy-published", rule_only_proxy_published),
    ("R13 tls-termination", rule_tls_termination),
    ("R14 no-inline-secrets", rule_no_inline_secrets),
    ("R15 stateful-volumes", rule_stateful_volumes),
    ("R16 waivers-not-stale", rule_waivers_not_stale),
    ("R17 web-origin-https", rule_web_origin_https),
    ("R18 app-env-prod-layer", rule_app_env_layer),
    ("R19 web-runtime-config", rule_web_runtime_config),
]


def run_rules(inp):
    findings = []
    for rid, fn in RULES:
        for msg in fn(inp):
            findings.append((rid, msg))
    return findings


# ---------------------------------------------------------------------------
# Self-test: one mutation per rule, each of which must be caught BY THAT RULE
# ---------------------------------------------------------------------------

def _mutate_doc(inp, fn):
    doc = copy.deepcopy(inp.doc)
    fn(doc)
    return Inputs_from(inp, doc=doc, env_text=inp.env_text, proxy_text=inp.proxy_text)


def _mutate_env(inp, fn):
    text = inp.env_text
    return Inputs_from(inp, doc=copy.deepcopy(inp.doc), env_text=fn(text), proxy_text=inp.proxy_text)


def _mutate_proxy(inp, fn):
    return Inputs_from(inp, doc=copy.deepcopy(inp.doc), env_text=inp.env_text, proxy_text=fn(inp.proxy_text))


def Inputs_from(inp, doc=None, env_text=None, proxy_text=None):
    """A throwaway Inputs built from mutated parts (no filesystem access)."""
    clone = copy.copy(inp)
    clone.doc = doc if doc is not None else copy.deepcopy(inp.doc)
    clone.services = clone.doc.get("services", {})
    clone.waivers = clone.doc.get("x-post-healthcheck-waivers", {}) or {}
    if env_text is not None:
        clone.env_active, clone.env_documented, clone.env_values = parse_env(env_text)
    if proxy_text is not None:
        clone.proxy_text = proxy_text
    return clone


def _drop_image(doc):
    del doc["services"]["worker"]["image"]


def _literal_image(doc):
    doc["services"]["api"]["image"] = "registry.example.com/post/api:latest"


def _tag_not_digest(text):
    return text.replace(
        "POST_API_IMAGE=registry.example.com/post/api@sha256:",
        "POST_API_IMAGE=registry.example.com/post/api:",
        1,
    )


def _comment_required(text):
    return text.replace("\nPOST_DB_PASSWORD=change-me", "\n#POST_DB_PASSWORD=change-me", 1)


def _rename_documented(text):
    return text.replace("POST_REDIS_ADDR", "POST_REDIS_URL")


def _drop_waiver(doc):
    del doc["x-post-healthcheck-waivers"]["worker"]


def _drop_healthcheck_field(doc):
    del doc["services"]["api"]["healthcheck"]["retries"]


def _curl_without_fail(doc):
    doc["services"]["api"]["healthcheck"]["test"] = [
        "CMD-SHELL",
        "curl -sS http://127.0.0.1:8080/readyz >/dev/null || exit 1",
    ]


def _trivial_healthcheck(doc):
    doc["services"]["mcp"]["healthcheck"]["test"] = ["CMD-SHELL", "true"]


def _drop_migrate_dependency(doc):
    del doc["services"]["api"]["depends_on"]["migrate"]


def _require_profiled_dep(doc):
    doc["services"]["api"]["depends_on"]["postgres"].pop("required", None)


def _restart_migrate(doc):
    doc["services"]["migrate"]["restart"] = "unless-stopped"


def _publish_postgres(doc):
    doc["services"]["postgres"]["ports"] = ["127.0.0.1:5432:5432"]


def _move_cert_path(text):
    return text.replace("/etc/nginx/tls/tls.crt", "/etc/ssl/certs/tls.crt", 1)


def _inline_secret(doc):
    doc["services"]["api"]["environment"]["POST_DB_PASSWORD"] = "hunter2"


def _drop_volume(doc):
    doc["services"]["postgres"]["volumes"] = ["./data:/var/lib/postgresql/data"]


def _stale_waiver(doc):
    doc["x-post-healthcheck-waivers"]["api"] = "not really needed"


def _http_origin(text):
    return text.replace("POST_WEB_ORIGIN=https://staging.example.com", "POST_WEB_ORIGIN=http://staging.example.com")


def _wrong_layer(doc):
    doc["services"]["adapter"]["environment"]["POST_ENV"] = "dev"


def _drop_web_url(doc):
    del doc["services"]["web"]["environment"]["API_BASE_URL"]


MUTANTS = [
    ("R01 service-images", lambda i: _mutate_doc(i, _drop_image)),
    ("R02 image-is-required-variable", lambda i: _mutate_doc(i, _literal_image)),
    ("R03 image-digest-pinned", lambda i: _mutate_env(i, _tag_not_digest)),
    ("R04 required-vars-have-values", lambda i: _mutate_env(i, _comment_required)),
    ("R05 all-vars-documented", lambda i: _mutate_env(i, _rename_documented)),
    ("R06 healthcheck-or-waiver", lambda i: _mutate_doc(i, _drop_waiver)),
    ("R07 healthcheck-complete", lambda i: _mutate_doc(i, _drop_healthcheck_field)),
    ("R08 healthcheck-no-false-green", lambda i: _mutate_doc(i, _curl_without_fail)),
    ("R08 healthcheck-no-false-green", lambda i: _mutate_doc(i, _trivial_healthcheck)),
    ("R09 deploy-order", lambda i: _mutate_doc(i, _drop_migrate_dependency)),
    ("R10 profile-deps-optional", lambda i: _mutate_doc(i, _require_profiled_dep)),
    ("R11 migrate-one-shot", lambda i: _mutate_doc(i, _restart_migrate)),
    ("R12 only-proxy-published", lambda i: _mutate_doc(i, _publish_postgres)),
    ("R13 tls-termination", lambda i: _mutate_proxy(i, _move_cert_path)),
    ("R14 no-inline-secrets", lambda i: _mutate_doc(i, _inline_secret)),
    ("R15 stateful-volumes", lambda i: _mutate_doc(i, _drop_volume)),
    ("R16 waivers-not-stale", lambda i: _mutate_doc(i, _stale_waiver)),
    ("R17 web-origin-https", lambda i: _mutate_env(i, _http_origin)),
    ("R18 app-env-prod-layer", lambda i: _mutate_doc(i, _wrong_layer)),
    ("R19 web-runtime-config", lambda i: _mutate_doc(i, _drop_web_url)),
]


def selftest(inp, out):
    """Every rule must reject its own mutation, and only then is it a check."""
    failures = []
    baseline = run_rules(inp)
    if baseline:
        out("FAIL selftest baseline: the real template already has findings:")
        for rid, msg in baseline:
            out(f"       [{rid}] {msg}")
        failures.append("baseline")

    for expected, mutate in MUTANTS:
        try:
            mutant = mutate(inp)
        except Exception as exc:  # a mutation that no longer applies is a rotted test
            failures.append(expected)
            out(f"FAIL selftest {expected}: mutation no longer applies ({exc!r})")
            continue
        rids = {rid for rid, _ in run_rules(mutant)}
        if expected in rids:
            out(f"ok   selftest {expected}: mutation rejected by {expected}")
        else:
            failures.append(expected)
            out(
                f"FAIL selftest {expected}: mutation survived (rules that fired: "
                f"{sorted(rids) or 'none'})"
            )
    return failures


def docker_config(inp, out):
    """Optional: hand the template to the real Compose parser."""
    if not shutil.which("docker"):
        out("skip docker compose config: no docker CLI on this host")
        return None
    env_file = inp.env_path
    cmd = [
        "docker", "compose",
        "--env-file", str(env_file),
        "-f", str(inp.compose_path),
        "--profile", "bundled-infra",
        "config", "--quiet",
    ]
    try:
        proc = subprocess.run(cmd, capture_output=True, text=True, timeout=120)
    except Exception as exc:  # noqa: BLE001 - reported, never swallowed
        out(f"skip docker compose config: {exc!r}")
        return None
    if proc.returncode != 0:
        out(f"FAIL docker compose config: exit {proc.returncode}")
        out("     " + (proc.stderr.strip().replace("\n", "\n     ") or "(no stderr)"))
        return ["docker-compose-config"]
    out("ok   docker compose config: the Compose parser accepts the template")
    return []


def main(argv=None):
    ap = argparse.ArgumentParser(add_help=True, description=__doc__.splitlines()[0])
    ap.add_argument("--compose", default=str(DEFAULT_COMPOSE))
    ap.add_argument("--env-example", default=str(DEFAULT_ENV))
    ap.add_argument("--json", action="store_true")
    ap.add_argument("--selftest", action="store_true")
    ap.add_argument("--with-docker", action="store_true")
    args = ap.parse_args(argv)

    lines = []

    def out(msg):
        lines.append(msg)
        if not args.json:
            print(msg)

    compose_path = Path(args.compose)
    env_path = Path(args.env_example)
    for path in (compose_path, env_path):
        if not path.is_file():
            print(f"error: {path} not found", file=sys.stderr)
            return 3

    try:
        inp = Inputs(compose_path, env_path)
    except yaml.YAMLError as exc:
        print(f"error: {compose_path} does not parse: {exc}", file=sys.stderr)
        return 3

    findings = run_rules(inp)
    for rid, msg in findings:
        out(f"FAIL [{rid}] {msg}")
    for rid, _ in RULES:
        if not any(f[0] == rid for f in findings):
            out(f"ok   [{rid}]")

    hard = list(findings)
    if args.selftest:
        if selftest(inp, out):
            hard.append(("selftest", "x"))
    if args.with_docker:
        docker_findings = docker_config(inp, out)
        if docker_findings:
            hard.extend((d, "x") for d in docker_findings)

    verdict = "FAIL" if hard else "PASS"
    # The line is what gets quoted from a log, so it names everything that
    # failed: `FAIL (0 finding(s) in 19 rules)` alone reads as "no rule
    # complained", which is exactly the wrong impression when the self-test or
    # the Compose parser is what went red.
    detail = f"{len(findings)} finding(s) in {len(RULES)} rules"
    extra = [rid for rid, _ in hard[len(findings):]]
    if extra:
        detail += ", " + ", ".join(f"{rid} failed" for rid in extra)
    out(f"VALIDATE RESULT: {verdict} ({detail})")
    if args.json:
        print(json.dumps({
            "verdict": verdict,
            "compose": str(compose_path),
            "env_example": str(env_path),
            "rules": len(RULES),
            "findings": [{"rule": rid, "message": msg} for rid, msg in findings],
        }, indent=2, ensure_ascii=False))
    return 1 if hard else 0


if __name__ == "__main__":
    sys.exit(main())
