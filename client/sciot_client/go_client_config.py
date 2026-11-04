"""Configuration of the client for the Go server (``http_go_client``).

One YAML file, ``go_client.yaml`` next to this module unless ``--config`` or
$SCIOT_GO_CLIENT_CONFIG names another. Precedence: command line > file >
defaults here. Unknown keys are an error, like the server's config.json: a
typo must not pass silently.

Relative paths in the file are relative to the file itself, so the file works
from any working directory; on the command line they are relative to the
working directory, as usual.
"""

from __future__ import annotations

import os
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import yaml

DEFAULT_PATH = Path(__file__).resolve().parent / "go_client.yaml"
ENV_VAR = "SCIOT_GO_CLIENT_CONFIG"
DEFAULT_SERVER = "http://127.0.0.1:8080"
DEFAULT_IMAGE = Path(__file__).resolve().parent / "stop.png"

KEYS = ("server", "device_id", "model_dir", "image", "probe")


class ConfigError(ValueError):
    """The configuration is invalid; the message says what to fix."""


@dataclass(frozen=True)
class GoClientConfig:
    server: str
    device_id: str
    model_dir: Path  # built by sciot-model build: the SAME files the server serves
    image: Path  # static input image (the camera comes in phase 4)
    # Declare the probe capability (answer the server's bandwidth probes).
    # False only withdraws it: the code supports it.
    probe: bool
    source: Path | None  # the file read, None without one


def read_file(path: Path) -> dict[str, Any]:
    """The values of a config file, paths resolved against its directory."""
    try:
        data = yaml.safe_load(path.read_text(encoding="utf-8"))
    except yaml.YAMLError as exc:
        raise ConfigError(f"{path}: invalid YAML: {exc}") from exc
    if data is None:  # empty file
        return {}
    if not isinstance(data, dict):
        raise ConfigError(f"{path}: expected a mapping of keys, got {type(data).__name__}")
    unknown = sorted(set(data) - set(KEYS))
    if unknown:
        raise ConfigError(f"{path}: unknown keys {unknown}; known: {list(KEYS)}")
    values = {k: v for k, v in data.items() if v is not None}
    for key in ("model_dir", "image"):
        if key in values:
            if not isinstance(values[key], str):
                raise ConfigError(f"{path}: {key} must be a path")
            values[key] = path.parent / Path(values[key]).expanduser()
    return values


def load(path: Path | None = None, overrides: dict[str, Any] | None = None) -> GoClientConfig:
    """Read the configuration and apply the command-line overrides.

    ``path`` is the --config option: when given (or set by $SCIOT_GO_CLIENT_CONFIG)
    the file must exist; the default file is optional. ``overrides`` maps keys
    to command-line values, None for "not given".
    """
    if path is None and os.environ.get(ENV_VAR):
        path = Path(os.environ[ENV_VAR])
    if path is not None:
        path = path.expanduser()
        if not path.is_file():
            raise ConfigError(f"config file {path} not found")
        source: Path | None = path
    else:
        source = DEFAULT_PATH if DEFAULT_PATH.is_file() else None

    values: dict[str, Any] = {
        "server": DEFAULT_SERVER,
        "image": DEFAULT_IMAGE,
        "probe": True,
    }
    if source is not None:
        values.update(read_file(source))
    for key, value in (overrides or {}).items():
        if key not in KEYS:
            raise ConfigError(f"unknown override {key!r}")
        if value is not None:
            values[key] = Path(value).expanduser() if key in ("model_dir", "image") else value
    return _validate(values, source)


def _validate(values: dict[str, Any], source: Path | None) -> GoClientConfig:
    where = f" (in {source} or on the command line)" if source else " (on the command line)"
    server = values.get("server")
    if not isinstance(server, str) or not server.startswith(("http://", "https://")):
        raise ConfigError(f"server must be an http:// or https:// URL, got {server!r}")
    device_id = values.get("device_id")
    if not isinstance(device_id, str) or not device_id:
        raise ConfigError(f"device_id is required{where}")
    model_dir = values.get("model_dir")
    if model_dir is None:
        raise ConfigError(f"model_dir is required{where}: the directory built by sciot-model build")
    model_dir = Path(model_dir).resolve()
    if not (model_dir / "manifest.json").is_file():
        raise ConfigError(
            f"model_dir {model_dir} has no manifest.json: build it with sciot-model build"
        )
    image = Path(values["image"]).resolve()
    if not image.is_file():
        raise ConfigError(f"image {image} not found")
    probe = values["probe"]
    if not isinstance(probe, bool):
        raise ConfigError(f"probe must be true or false, got {probe!r}")
    return GoClientConfig(
        server=server.rstrip("/"),
        device_id=device_id,
        model_dir=model_dir,
        image=image,
        probe=probe,
        source=source,
    )
