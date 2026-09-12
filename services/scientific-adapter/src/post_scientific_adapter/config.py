"""Configuration for the POST scientific adapter (T0004 baseline).

The adapter validates its own environment, independently of the Go core and
the Next.js web app: it reads only its own variables, so a missing adapter
variable is never satisfied by another service's variable and vice versa.

Fail-closed contract:

* ``POST_ENV`` is mandatory (``dev`` | ``test`` | ``prod``): a missing or
  ambiguous layer is a startup failure, never a guessed default;
* a layer file (``.env.<layer>`` in the working directory) is loaded only
  when it matches the current layer — a value can never fall back across
  layers; process-environment values override file values;
* required values have no defaults; a malformed value is a startup failure
  that names the offending variable and says what to do;
* error messages never echo secret values (see ``redact.redact_url`` for the
  URL form — the ``scripts/speclib.redact_url`` precedent).

This module is stdlib-only, on purpose: configuration is loaded before
anything else can be trusted.
"""

from __future__ import annotations

import os
from dataclasses import dataclass
from pathlib import Path

LAYERS = ("dev", "test", "prod")

ENV_LAYER = "POST_ENV"
ENV_HOST = "POST_SCIENTIFIC_ADAPTER_HOST"
ENV_PORT = "POST_SCIENTIFIC_ADAPTER_PORT"

DEFAULT_HOST = "127.0.0.1"
# 9100, not 9000: 9000/9001 are the MinIO S3 API/console ports from
# docker-compose.yml (T0003), and the adapter must never collide with the
# blob store (T0006 port-collision fix).
DEFAULT_PORT = 9100

_LAYER_FIX = (
    f"set {ENV_LAYER} to dev, test or prod (see .env.example)"
)


@dataclass(frozen=True)
class ConfigProblem:
    """One configuration failure: the offending variable, what is wrong and
    what to do. Problems never carry secret values."""

    key: str
    msg: str
    fix: str


class ConfigError(ValueError):
    """Aggregate configuration error; every problem names its variable."""

    def __init__(self, problems: list[ConfigProblem]) -> None:
        self.problems = problems
        super().__init__(_format_problems(problems))


def _format_problems(problems: list[ConfigProblem]) -> str:
    if len(problems) == 1:
        p = problems[0]
        return f"config: {p.msg}; {p.fix}"
    lines = [f"config: {len(problems)} problems:"]
    lines += [f"  - {p.key}: {p.msg}; {p.fix}" for p in problems]
    return "\n".join(lines)


@dataclass(frozen=True)
class AdapterConfig:
    """The validated adapter configuration."""

    layer: str
    host: str
    port: int

    def describe(self) -> str:
        """One safe startup summary line (host/port only — no secrets)."""
        return (
            f"layer={self.layer} "
            f"listen={self.host}:{self.port}"
        )


def parse_env_file(path: str | Path) -> dict[str, str]:
    """Parse a minimal KEY=VALUE environment file.

    One KEY=VALUE per line; blank lines and lines whose first non-space
    character is ``#`` are ignored (no inline comments — ``#`` is legal in a
    value). KEY must match ``[A-Za-z_][A-Za-z0-9_]*``. An optional
    surrounding pair of matching quotes is stripped from the value. A
    duplicated key keeps the last value. Anything else raises
    :class:`ConfigError` naming file and line — a broken layer file must
    fail loudly, never be half-applied.
    """
    path = Path(path)
    try:
        text = path.read_text(encoding="utf-8")
    except OSError as exc:
        raise ConfigError([ConfigProblem(
            key=ENV_LAYER,
            msg=f"cannot read layer file {str(path)!r}: {exc}",
            fix=f"fix or remove {path.name} (KEY=VALUE per line; see .env.example)",
        )]) from exc
    values: dict[str, str] = {}
    for line_no, raw in enumerate(text.splitlines(), start=1):
        line = raw.rstrip("\r")
        stripped = line.strip()
        if not stripped or stripped.startswith("#"):
            continue
        key, sep, value = line.partition("=")
        if not sep:
            raise ConfigError([ConfigProblem(
                key=ENV_LAYER,
                msg=f"{path}:{line_no}: not a KEY=VALUE line: {stripped!r}",
                fix=f"fix {path.name} (KEY=VALUE per line; see .env.example)",
            )])
        key = key.strip()
        if not key.replace("_", "a").isalnum() or key[0].isdigit():
            raise ConfigError([ConfigProblem(
                key=ENV_LAYER,
                msg=(f"{path}:{line_no}: invalid KEY {key!r} "
                     "(must match [A-Za-z_][A-Za-z0-9_]*)"),
                fix=f"fix {path.name} (KEY=VALUE per line; see .env.example)",
            )])
        value = value.strip()
        if len(value) >= 2 and value[0] == value[-1] and value[0] in "\"'":
            value = value[1:-1]
        values[key] = value
    return values


def load_config(
    env: dict[str, str],
    *,
    env_file: str | Path | None = None,
) -> AdapterConfig:
    """Load and validate the adapter configuration.

    ``env`` is the environment source (process environment in production,
    a controlled map in tests). ``env_file`` is an optional layer file; when
    given, its base name must be ``.env.<layer>`` for the current layer,
    otherwise the load is refused (cross-layer fallback is forbidden).
    Raises :class:`ConfigError` naming every offending variable.
    """
    problems: list[ConfigProblem] = []

    raw_layer = (env.get(ENV_LAYER) or "").strip()
    if not raw_layer:
        problems.append(ConfigProblem(
            key=ENV_LAYER,
            msg="the configuration layer is not set: refusing to guess",
            fix=_LAYER_FIX,
        ))
    elif raw_layer not in LAYERS:
        problems.append(ConfigProblem(
            key=ENV_LAYER,
            msg=(f"unknown configuration layer {raw_layer!r}; "
                 "valid layers are dev, test, prod"),
            fix=_LAYER_FIX,
        ))
    layer = raw_layer

    values: dict[str, str] = {}
    if env_file is not None:
        path = Path(env_file)
        if path.name != f".env.{layer}":
            problems.append(ConfigProblem(
                key=ENV_LAYER,
                msg=(f"refusing to load {str(path)!r} under layer {layer}: "
                     "the file layer does not match POST_ENV and "
                     "cross-layer fallback is forbidden"),
                fix=(f"use a file named .env.{layer} or set {ENV_LAYER} "
                     "to the file's layer"),
            ))
        else:
            values.update(parse_env_file(path))
    # Process environment overrides the layer file.
    for key, value in env.items():
        if value:
            values[key] = value

    host = values.get(ENV_HOST) or DEFAULT_HOST
    raw_port = values.get(ENV_PORT) or str(DEFAULT_PORT)
    try:
        port = int(raw_port, 10)
    except ValueError:
        port = -1
    if port < 1 or port > 65535:
        problems.append(ConfigProblem(
            key=ENV_PORT,
            msg=(f"{ENV_PORT} has an invalid value {raw_port!r}: "
                 "must be an integer between 1 and 65535"),
            fix=f"set {ENV_PORT} to a listen port (see .env.example)",
        ))

    if problems:
        raise ConfigError(problems)
    return AdapterConfig(layer=layer, host=host, port=port)


def load_config_from_cwd(
    cwd: str | Path | None = None,
) -> AdapterConfig:
    """Load the adapter configuration from the process environment plus an
    optional ``.env.<layer>`` file in the working directory."""
    env = dict(os.environ)
    layer = (env.get(ENV_LAYER) or "").strip()
    path = Path(cwd) if cwd is not None else Path.cwd()
    env_file = path / f".env.{layer}" if layer else None
    if env_file is not None and not env_file.is_file():
        env_file = None
    return load_config(env, env_file=env_file)
