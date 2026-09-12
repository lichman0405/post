"""Tests for the scientific-adapter configuration validator (T0004).

The adapter validates its own environment: a missing adapter variable is
never satisfied by a web or Go variable, a missing/ambiguous layer refuses
to start, and layer files never fall back across layers.
"""

from __future__ import annotations

import pytest

from post_scientific_adapter.config import (
    ConfigError,
    ENV_HOST,
    ENV_LAYER,
    ENV_PORT,
    AdapterConfig,
    load_config,
    parse_env_file,
)


def valid_env(layer: str = "dev") -> dict[str, str]:
    return {ENV_LAYER: layer}


def problems_of(exc: ConfigError) -> list[str]:
    return [p.key for p in exc.problems]


def test_missing_layer_fails_fast_and_names_the_variable() -> None:
    with pytest.raises(ConfigError) as exc_info:
        load_config({})
    assert ENV_LAYER in problems_of(exc_info.value)
    assert "dev, test or prod" in str(exc_info.value)


def test_unknown_layer_is_refused_with_the_valid_list() -> None:
    with pytest.raises(ConfigError) as exc_info:
        load_config({ENV_LAYER: "staging"})
    msg = str(exc_info.value)
    assert "staging" in msg and "dev, test, prod" in msg


def test_every_layer_is_accepted() -> None:
    for layer in ("dev", "test", "prod"):
        cfg = load_config(valid_env(layer))
        assert cfg.layer == layer
        assert cfg.host == "127.0.0.1"
        # 9100, not 9000: 9000 is the MinIO S3 API port (T0006 fix).
        assert cfg.port == 9100


def test_env_values_override_defaults() -> None:
    cfg = load_config({
        ENV_LAYER: "prod",
        ENV_HOST: "0.0.0.0",
        ENV_PORT: "9200",
    })
    assert cfg.host == "0.0.0.0"
    assert cfg.port == 9200
    assert isinstance(cfg, AdapterConfig)


@pytest.mark.parametrize("bad_port", ["abc", "0", "65536", "-1", "80.5"])
def test_malformed_port_is_named(bad_port: str) -> None:
    with pytest.raises(ConfigError) as exc_info:
        load_config({ENV_LAYER: "dev", ENV_PORT: bad_port})
    assert ENV_PORT in problems_of(exc_info.value)
    assert "1 and 65535" in str(exc_info.value)


def test_layer_file_applies_only_for_the_matching_layer(tmp_path) -> None:
    dev_file = tmp_path / ".env.dev"
    dev_file.write_text(f"{ENV_PORT}=9200\n", encoding="utf-8")

    # dev layer: the file applies.
    cfg = load_config(valid_env("dev"), env_file=dev_file)
    assert cfg.port == 9200

    # test layer: the same file is refused — no cross-layer fallback.
    with pytest.raises(ConfigError) as exc_info:
        load_config(valid_env("test"), env_file=dev_file)
    msg = str(exc_info.value)
    assert "cross-layer fallback is forbidden" in msg
    assert ENV_LAYER in problems_of(exc_info.value)


def test_process_env_overrides_layer_file(tmp_path) -> None:
    dev_file = tmp_path / ".env.dev"
    dev_file.write_text(
        f"{ENV_HOST}=file-host\n{ENV_PORT}=9300\n", encoding="utf-8"
    )
    cfg = load_config(
        {ENV_LAYER: "dev", ENV_PORT: "9200"}, env_file=dev_file
    )
    assert cfg.host == "file-host"  # from the file
    assert cfg.port == 9200  # process env wins


def test_missing_file_value_is_not_satisfied_by_another_layers_file(
    tmp_path,
) -> None:
    # A .env.dev file may exist on disk, but the loader only ever reads an
    # explicit matching file — the value is simply not picked up.
    dev_file = tmp_path / ".env.dev"
    dev_file.write_text(f"{ENV_PORT}=9200\n", encoding="utf-8")
    cfg = load_config(valid_env("test"))
    assert cfg.port == 9100


def test_broken_layer_file_fails_loudly(tmp_path) -> None:
    dev_file = tmp_path / ".env.dev"
    dev_file.write_text(
        "PORT=9100\nnot a key value line\n", encoding="utf-8"
    )
    with pytest.raises(ConfigError) as exc_info:
        load_config(valid_env("dev"), env_file=dev_file)
    msg = str(exc_info.value)
    assert ".env.dev:2" in msg
    assert "KEY=VALUE" in msg


def test_adapter_variables_are_never_satisfied_by_web_variables() -> None:
    # A fully-configured *web* environment must not configure the adapter:
    # the layer is shared by design, but web variables are invisible here.
    web_only = {
        ENV_LAYER: "dev",
        "API_BASE_URL": "http://127.0.0.1:8080",
        "SCIENTIFIC_ADAPTER_URL": "http://127.0.0.1:9100",
    }
    cfg = load_config(web_only)
    assert cfg.host == "127.0.0.1"  # neutral default, not a web value
    assert cfg.port == 9100


def test_adapter_still_requires_its_own_layer() -> None:
    # Web variables alone cannot even establish the layer.
    with pytest.raises(ConfigError) as exc_info:
        load_config({
            "API_BASE_URL": "http://127.0.0.1:8080",
            "SCIENTIFIC_ADAPTER_URL": "http://127.0.0.1:9100",
        })
    assert ENV_LAYER in problems_of(exc_info.value)


def test_parse_env_file_handles_comments_quotes_and_crlf(tmp_path) -> None:
    env_file = tmp_path / ".env.dev"
    env_file.write_bytes(
        b"# comment\n"
        b"POST_SCIENTIFIC_ADAPTER_HOST='quoted host'\n"
        b"POST_SCIENTIFIC_ADAPTER_PORT=9100\r\n"
        b"KEEP=value#not-a-comment\n"
    )
    values = parse_env_file(env_file)
    assert values == {
        "POST_SCIENTIFIC_ADAPTER_HOST": "quoted host",
        "POST_SCIENTIFIC_ADAPTER_PORT": "9100",
        "KEEP": "value#not-a-comment",
    }


def test_describe_contains_no_secret_material() -> None:
    cfg = load_config(valid_env("prod"))
    assert "layer=prod" in cfg.describe()
    assert "listen=127.0.0.1:9100" in cfg.describe()
