"""Docker setup must keep site config and never resume before runtime activation."""

import json
import subprocess
from pathlib import Path

import pytest

from neu_box.maintenance import docker_config


def _installed(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(
        docker_config, "_discover_binary",
        lambda name, _paths: f"/usr/bin/{name}",
    )


def test_merges_site_config_and_validates_before_replacing(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path,
) -> None:
    _installed(monkeypatch)
    config = tmp_path / "daemon.json"
    config.write_text(
        '{"data-root":"/data/docker","runtimes":{"nvidia":{"path":"/usr/bin/nvidia-runtime"}}}\n',
        encoding="utf-8",
    )
    original = config.read_text()
    plan = docker_config.prepare_docker_config(config)
    assert plan is not None and plan.content is not None
    seen: list[list[str]] = []

    def validate(command: list[str], **_kwargs):
        seen.append(command)
        assert config.read_text() == original
        return subprocess.CompletedProcess(command, 0, stdout="configuration OK\n")

    monkeypatch.setattr(docker_config.subprocess, "run", validate)
    docker_config._write_validated(plan)
    result = json.loads(config.read_text())
    assert result["data-root"] == "/data/docker"
    assert result["runtimes"]["nvidia"]["path"] == "/usr/bin/nvidia-runtime"
    assert result["runtimes"]["neu-box-runtime"]["path"] == docker_config.RUNTIME_PATH
    assert result["default-runtime"] == "neu-box-runtime"
    assert seen[0][:2] == ["/usr/bin/dockerd", "--validate"]
    assert list(tmp_path.glob("daemon.json.neubox.bak.*"))
    assert docker_config.prepare_docker_config(config).content is None


@pytest.mark.parametrize("source", [
    '{"default-runtime":"nvidia"}',
    '{"runtimes":{},"runtimes":{}}',
    '{ invalid json',
])
def test_conflicting_or_invalid_config_is_untouched(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path, source: str,
) -> None:
    _installed(monkeypatch)
    config = tmp_path / "daemon.json"
    config.write_text(source, encoding="utf-8")
    with pytest.raises(RuntimeError):
        docker_config.prepare_docker_config(config)
    assert config.read_text(encoding="utf-8") == source


def test_deferred_restart_leaves_worker_paused_and_resume_checks_live_docker(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path, capsys: pytest.CaptureFixture[str],
) -> None:
    _installed(monkeypatch)
    config = tmp_path / "daemon.json"
    config.write_text('{"default-runtime":"neu-box-runtime","runtimes":'
                      '{"neu-box-runtime":{"path":"' + docker_config.RUNTIME_PATH + '"}}}\n')
    plan = docker_config.prepare_docker_config(config)
    assert plan is not None and plan.content is None
    monkeypatch.setattr(docker_config, "_runtime_active", lambda _plan: False)
    assert docker_config.activate_docker_config(plan, restart=False) is False
    assert "sudo /usr/libexec/neu-box/neuboxctl/neuboxctl resume" in capsys.readouterr().out
    with pytest.raises(RuntimeError, match="尚未加载"):
        docker_config.verify_docker_ready(config)
    monkeypatch.setattr(docker_config, "_runtime_active", lambda _plan: True)
    docker_config.verify_docker_ready(config)


def test_upgrade_replaces_old_runtime_path_without_losing_docker_options(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path,
) -> None:
    _installed(monkeypatch)
    config = tmp_path / "daemon.json"
    config.write_text(json.dumps({
        "data-root": "/data/docker",
        "default-runtime": "neu-box-runtime",
        "runtimes": {
            "nvidia": {"path": "/usr/bin/nvidia-runtime"},
            "neu-box-runtime": {"path": "/usr/local/bin/neu-box-runtime"},
        },
    }))

    plan = docker_config.prepare_docker_config(config)

    assert plan is not None and plan.content is not None
    updated = json.loads(plan.content)
    assert updated["data-root"] == "/data/docker"
    assert updated["runtimes"]["nvidia"]["path"] == "/usr/bin/nvidia-runtime"
    assert updated["runtimes"]["neu-box-runtime"]["path"] == docker_config.RUNTIME_PATH


def test_restart_waits_for_effective_default(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path,
) -> None:
    _installed(monkeypatch)
    plan = docker_config.DockerConfig(tmp_path / "daemon.json", "/usr/bin/dockerd", "/usr/bin/docker", None)
    observed = iter([False, True])
    monkeypatch.setattr(docker_config, "_runtime_active", lambda _plan: next(observed))
    commands: list[list[str]] = []
    monkeypatch.setattr(
        docker_config.subprocess, "run",
        lambda command, **_kwargs: commands.append(command) or subprocess.CompletedProcess(command, 0),
    )
    assert docker_config.activate_docker_config(plan, restart=True)
    assert commands == [["systemctl", "restart", "docker"]]


def test_live_default_name_alone_does_not_prove_runtime_path(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path,
) -> None:
    plan = docker_config.DockerConfig(tmp_path / "daemon.json", "/usr/bin/dockerd", "/usr/bin/docker", None)
    paths = iter(["/old/wrapper", docker_config.RUNTIME_PATH])

    def docker_info(command: list[str], **_kwargs):
        if command[-1] == "{{.DefaultRuntime}}":
            output = "neu-box-runtime\n"
        else:
            output = json.dumps({"neu-box-runtime": {"path": next(paths)}})
        return subprocess.CompletedProcess(command, 0, stdout=output)

    monkeypatch.setattr(docker_config.subprocess, "run", docker_info)
    assert not docker_config._runtime_active(plan)
    assert docker_config._runtime_active(plan)
