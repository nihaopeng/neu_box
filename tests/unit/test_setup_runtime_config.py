"""The combined RPM configures its OCI runtime through neuboxctl setup."""

from pathlib import Path
import subprocess

import pytest

from neu_box import ctl
from neu_box.maintenance import pause, setup


def _executable(path: Path) -> Path:
    path.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
    path.chmod(0o755)
    return path


def _runtime_paths(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> tuple[Path, Path]:
    config_tool = _executable(tmp_path / "neu-box-config")
    wrapper = _executable(tmp_path / "neu-box-runtime")
    monkeypatch.setattr(setup, "_RUNTIME_CONFIG_TOOL", config_tool)
    monkeypatch.setattr(setup, "_RUNTIME_WRAPPER", wrapper)
    monkeypatch.setattr(setup, "RUNTIME_CONFIG_PATH", tmp_path / "runtime.env")
    return config_tool, wrapper


def test_setup_initializes_runtime_with_discovered_runc_and_worker_port(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path,
) -> None:
    config_tool, _ = _runtime_paths(monkeypatch, tmp_path)
    real_runc = _executable(tmp_path / "runc")
    monkeypatch.setattr(setup.shutil, "which", lambda name: str(real_runc) if name == "runc" else None)
    calls: list[tuple[list[str], bool]] = []

    def run(command: list[str], *, check: bool) -> subprocess.CompletedProcess:
        calls.append((command, check))
        return subprocess.CompletedProcess(command, 0)

    monkeypatch.setattr(setup.subprocess, "run", run)

    setup.initialize_runtime_config(59123)

    assert calls == [([
        str(config_tool), "init", "--path", str(tmp_path / "runtime.env"),
        "--worker-url", "http://127.0.0.1:59123", "--sync-worker-url",
        "--real-runc", str(real_runc),
    ], True)]


def test_explicit_real_runc_overrides_discovery(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path,
) -> None:
    _runtime_paths(monkeypatch, tmp_path)
    real_runc = _executable(tmp_path / "alternate-runc")
    monkeypatch.setattr(setup.shutil, "which", lambda _name: None)
    calls: list[list[str]] = []
    monkeypatch.setattr(
        setup.subprocess, "run",
        lambda command, *, check: calls.append(command),
    )

    setup.initialize_runtime_config(59075, str(real_runc))

    assert calls[0][-2:] == ["--real-runc", str(real_runc)]
    assert ctl._parser().parse_args([
        "setup", "--real-runc", str(real_runc),
    ]).real_runc == str(real_runc)


def test_cli_passes_explicit_runc_to_setup(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(ctl, "load_role_environment", lambda _role, _config: None)
    monkeypatch.setattr(ctl, "env_int", lambda _name, _default: 59123)
    calls: list[tuple[int | None, int, Path | None, str | None]] = []
    monkeypatch.setattr(
        ctl, "setup",
        lambda port, timeout, config, *, real_runc: calls.append((port, timeout, config, real_runc)),
    )

    assert ctl.main(["setup", "--real-runc", "/opt/oci/runc"]) == 0

    assert calls == [(None, 60, None, "/opt/oci/runc")]


def test_missing_runc_on_docker_node_fails_before_writing_config(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path,
) -> None:
    _runtime_paths(monkeypatch, tmp_path)
    monkeypatch.setattr(setup.shutil, "which", lambda name: "/usr/bin/docker" if name == "docker" else None)
    monkeypatch.setattr(
        setup.subprocess, "run",
        lambda *_args, **_kwargs: pytest.fail("runtime config tool must not run"),
    )

    with pytest.raises(RuntimeError, match="找不到 runc"):
        setup.initialize_runtime_config(59075)


def test_host_only_setup_does_not_require_runc(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path,
) -> None:
    _runtime_paths(monkeypatch, tmp_path)
    monkeypatch.setattr(setup.shutil, "which", lambda _name: None)
    calls: list[list[str]] = []
    monkeypatch.setattr(
        setup.subprocess, "run",
        lambda command, *, check: calls.append(command),
    )

    setup.initialize_runtime_config(59075)

    assert len(calls) == 1
    assert "--real-runc" not in calls[0]


def test_runc_cannot_resolve_to_neubox_wrapper(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path,
) -> None:
    _, wrapper = _runtime_paths(monkeypatch, tmp_path)
    runc_symlink = tmp_path / "runc"
    runc_symlink.symlink_to(wrapper)
    monkeypatch.setattr(setup.shutil, "which", lambda name: str(runc_symlink) if name == "runc" else None)
    monkeypatch.setattr(
        setup.subprocess, "run",
        lambda *_args, **_kwargs: pytest.fail("recursive runtime must not be configured"),
    )

    with pytest.raises(RuntimeError, match="递归调用"):
        setup.initialize_runtime_config(59075)


def test_runtime_config_failure_stops_setup_before_database_and_service(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setattr(setup, "require_root", lambda: None)
    monkeypatch.setattr(
        setup.subprocess, "run",
        lambda command, **_kwargs: subprocess.CompletedProcess(command, 3),
    )
    monkeypatch.setattr(setup, "migrate_config", lambda _config: None)
    monkeypatch.setattr(setup, "env_int", lambda _name, _default: 59075)
    monkeypatch.setattr(
        setup, "initialize_runtime_config",
        lambda _port, _runc: (_ for _ in ()).throw(RuntimeError("runtime init failed")),
    )
    for name in ("migrate_database", "check_database", "mark_paused"):
        monkeypatch.setattr(
            setup, name,
            lambda *_args, **_kwargs: pytest.fail("database/service work must not start"),
        )

    with pytest.raises(RuntimeError, match="runtime init failed"):
        setup.setup(None, 60)


def test_pause_backs_up_runtime_config(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    runtime_config = tmp_path / "runtime.env"
    runtime_config.write_text("NEU_BOX_WORKER_URL=http://127.0.0.1:59075\n", encoding="utf-8")
    backup = tmp_path / "worker.sqlite"
    monkeypatch.setattr(pause, "RUNTIME_CONFIG_PATH", runtime_config)
    monkeypatch.setattr(pause, "require_root", lambda: None)
    monkeypatch.setattr(pause, "mark_paused", lambda: None)
    monkeypatch.setattr(pause, "control_worker", lambda _command, _port: {"quiet": True})
    monkeypatch.setattr(pause, "env_text", lambda _name: str(tmp_path))
    monkeypatch.setattr(pause, "database_path", lambda: tmp_path / "worker.db")
    monkeypatch.setattr(pause, "backup_database", lambda *_args: backup)
    monkeypatch.setattr(pause, "sandbox_executable_path", lambda: tmp_path / "sandbox")
    monkeypatch.setattr(pause.subprocess, "run", lambda *_args, **_kwargs: None)
    monkeypatch.setattr(pause, "stop_worker_after_cleanup", lambda: None)

    pause.pause(59075, 0, None)

    assert backup.with_suffix(".runtime.env").read_bytes() == runtime_config.read_bytes()
