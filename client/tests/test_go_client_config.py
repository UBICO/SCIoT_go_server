"""go_client_config: file, defaults, command-line overrides, validation."""

from __future__ import annotations

from pathlib import Path

import pytest

from sciot_client import go_client_config as gcc
from sciot_client.go_client_config import ConfigError, load


@pytest.fixture
def model_dir(tmp_path: Path) -> Path:
    d = tmp_path / "models" / "yolo"
    d.mkdir(parents=True)
    (d / "manifest.json").write_text("{}")
    return d


@pytest.fixture(autouse=True)
def no_env(monkeypatch):
    monkeypatch.delenv(gcc.ENV_VAR, raising=False)


def write(path: Path, text: str) -> Path:
    path.write_text(text, encoding="utf-8")
    return path


def test_file_values_and_relative_paths(tmp_path, model_dir):
    (tmp_path / "img.png").write_bytes(b"")
    cfg_path = write(
        tmp_path / "c.yaml",
        "server: http://10.0.0.2:8080/\ndevice_id: cam_1\nmodel_dir: models/yolo\nimage: img.png\n",
    )
    cfg = load(cfg_path)
    assert cfg.server == "http://10.0.0.2:8080"  # trailing slash removed
    assert cfg.device_id == "cam_1"
    # Relative to the file, not to the working directory.
    assert cfg.model_dir == model_dir.resolve()
    assert cfg.image == (tmp_path / "img.png").resolve()
    assert cfg.source == cfg_path


def test_command_line_wins(tmp_path, model_dir, monkeypatch):
    cfg_path = write(tmp_path / "c.yaml", f"device_id: a\nmodel_dir: {model_dir}\n")
    other = tmp_path / "other"
    other.mkdir()
    (other / "manifest.json").write_text("{}")
    monkeypatch.chdir(tmp_path)
    cfg = load(
        cfg_path,
        {
            "device_id": "b",
            "server": "http://h:1",
            "model_dir": Path("other"),
            "image": None,
        },
    )
    assert cfg.device_id == "b" and cfg.server == "http://h:1"
    # On the command line, relative to the working directory.
    assert cfg.model_dir == other.resolve()


def test_defaults(tmp_path, model_dir):
    cfg = load(write(tmp_path / "c.yaml", f"device_id: a\nmodel_dir: {model_dir}\n"))
    assert cfg.server == gcc.DEFAULT_SERVER and cfg.image == gcc.DEFAULT_IMAGE.resolve()
    assert cfg.probe is True


def test_probe_can_only_be_withdrawn(tmp_path, model_dir):
    base = f"device_id: a\nmodel_dir: {model_dir}\n"
    assert load(write(tmp_path / "c.yaml", base + "probe: false\n")).probe is False
    # --no-probe on the command line wins over the file.
    cfg_path = write(tmp_path / "c.yaml", base + "probe: true\n")
    assert load(cfg_path, {"probe": False}).probe is False
    assert load(cfg_path, {"probe": None}).probe is True  # option not given
    with pytest.raises(ConfigError, match="probe must be true or false"):
        load(write(tmp_path / "c.yaml", base + "probe: maybe\n"))


def test_null_values_mean_absent(tmp_path, model_dir):
    # The committed file has "model_dir:" with no value.
    cfg_path = write(tmp_path / "c.yaml", "device_id: a\nmodel_dir:\nserver:\n")
    with pytest.raises(ConfigError, match="model_dir is required"):
        load(cfg_path)
    cfg = load(cfg_path, {"model_dir": model_dir})
    assert cfg.server == gcc.DEFAULT_SERVER


def test_env_var_names_the_file(tmp_path, model_dir, monkeypatch):
    cfg_path = write(tmp_path / "c.yaml", f"device_id: env\nmodel_dir: {model_dir}\n")
    monkeypatch.setenv(gcc.ENV_VAR, str(cfg_path))
    assert load().device_id == "env"


def test_default_file_is_optional(tmp_path, model_dir, monkeypatch):
    monkeypatch.setattr(gcc, "DEFAULT_PATH", tmp_path / "missing.yaml")
    cfg = load(None, {"device_id": "a", "model_dir": model_dir})
    assert cfg.source is None


def test_explicit_file_must_exist(tmp_path):
    with pytest.raises(ConfigError, match="not found"):
        load(tmp_path / "missing.yaml")


@pytest.mark.parametrize(
    ("text", "match"),
    [
        ("device_id: a\nmodle_dir: x\n", "unknown keys \\['modle_dir'\\]"),
        ("- a\n- b\n", "expected a mapping"),
        ("device_id: [a\n", "invalid YAML"),
        ("device_id: a\nmodel_dir: 3\n", "model_dir must be a path"),
    ],
)
def test_invalid_files(tmp_path, text, match):
    with pytest.raises(ConfigError, match=match):
        load(write(tmp_path / "c.yaml", text))


def test_validation(tmp_path, model_dir):
    base = f"model_dir: {model_dir}\n"
    with pytest.raises(ConfigError, match="device_id is required"):
        load(write(tmp_path / "c.yaml", base))
    with pytest.raises(ConfigError, match="http:// or https://"):
        load(write(tmp_path / "c.yaml", base + "device_id: a\nserver: 127.0.0.1:8080\n"))
    with pytest.raises(ConfigError, match="no manifest.json"):
        load(write(tmp_path / "c.yaml", f"device_id: a\nmodel_dir: {tmp_path}\n"))
    with pytest.raises(ConfigError, match="image .* not found"):
        load(write(tmp_path / "c.yaml", base + "device_id: a\nimage: nope.png\n"))


def test_committed_file_is_valid(model_dir):
    # The file in the repo parses and only lacks model_dir.
    cfg = load(gcc.DEFAULT_PATH, {"model_dir": model_dir})
    assert cfg.device_id and cfg.image.is_file()
